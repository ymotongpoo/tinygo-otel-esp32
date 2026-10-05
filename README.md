# tinygo-otel-esp32

TinyGo で ESP32-S3 から OpenTelemetry Collector に OTLP でメトリクスを送る最小実装です。
TinyGo Conference 2026 のセッション「TinyGoでオブザーバビリティ？OpenTelemetryはTinyGoで使えるのか」のデモ実装です。

## これは何を示すための実装か

OpenTelemetry Go SDK はマイコン向けにビルドできません。それでも OTLP というプロトコルには到達できます。この実装は、その両方を実機で測って示すためにあります。

公式の部品のうち、使えたのは OTLP のスキーマ（フィールド番号と JSON マッピング）だけでした。SDK、エクスポーター、protobuf ランタイムはいずれも実機では動かないので、エンコーダと HTTP クライアントを自作しています。

| 層 | 公式の部品 | この実装 |
| --- | --- | --- |
| SDK | `otel/sdk/metric` | `otlpmini.Registry` / `otlpjson.Encoder`（計装器の登録と値の保持だけ） |
| エンコード | `google.golang.org/protobuf` | 手書き protobuf（`wire`）または `encoding/json`（`otlpjson`） |
| 送信 | `otlpmetrichttp` | `net/http` 版、または手書き HTTP/1.1 クライアント（`-tags socket`） |
| 時刻 | OS の時計 | SNTP クライアント（`sntp`） |

## 検証環境

TinyGo 0.42.0（LLVM 22.1.4）、Go 1.26.0、espradio v0.3.0、opentelemetry-proto v1.11.0 のフィールド番号、Collector 0.160.0、grafana/otel-lgtm 0.32.1。

実機は M5Stack CoreS3（ESP32-S3 rev v0.2、16MB Flash）で、ターゲットは `esp32s3-generic` です。検証の記録は `../_review/hardware-validation-2026-09-15.md` にあります。

## 送っているテレメトリー

メトリクス、ログ、トレースの3つのシグナルを送ります。メトリクスは 10 秒ごとに送り、ログとトレースはメトリクスの送信が成功した直後にまとめて送ります。

### メトリクス

12 ストリームです。

| メトリクス | 種類 | 属性 | 内容 |
| --- | --- | --- | --- |
| `device.uptime` | gauge | | 起動からの経過ミリ秒 |
| `device.wifi.rssi` | gauge | | 接続中の AP の RSSI（dBm） |
| `device.memory.usage` | gauge | `device.memory.pool=go_heap` | Go ヒープの使用量 |
| `device.memory.limit` | gauge | `device.memory.pool=go_heap` | Go ヒープの確保量 |
| `device.memory.usage` | gauge | `device.memory.pool=espradio_arena` | Wi-Fi ブロブのアリーナ使用量 |
| `device.memory.limit` | gauge | `device.memory.pool=espradio_arena` | 同アリーナの容量 |
| `device.export.payload` | gauge | | 前回送ったペイロードのバイト数 |
| `device.export.attempts` | counter | `outcome=success` | 送信成功 |
| `device.export.attempts` | counter | `outcome=failure` | 送信失敗（接続断、5xx など） |
| `device.export.attempts` | counter | `outcome=partial_rejection` | Collector が一部のデータポイントを拒否した |
| `device.logs.dropped` | counter | | 送る前にバッファから押し出されたログの件数 |
| `device.spans.dropped` | counter | | 送る前にバッファから押し出されたスパンの件数 |

### ログ

デバイスで起きたイベントを OTLP のログとして `/v1/logs` に送ります。

| 本文 | 重要度 | 属性 | 契機 |
| --- | --- | --- | --- |
| `device started` | INFO | `encoding`、`network.local.address`、`wifi.rssi` | 起動して時刻を合わせた直後 |
| `export failed` | ERROR | `error.message` | 送信の失敗が始まったとき（連続する失敗は1件にまとめる） |
| `export recovered` | INFO | `failed_attempts` | 失敗のあと最初に成功したとき |
| `collector rejected data points` | WARN | `error.message` | partialSuccess を受け取ったとき |

ログはメトリクスのエンコーディングにかかわらず常に JSON で送ります。OTLP/HTTP はリクエストごとに Content-Type を選べるので、ログのために手書きの protobuf エンコーダを増やす理由がないからです。メトリクスと同じ接続を使い回すので、2つ目のシグナルを足しても接続とバッファは増えません。

ログは 16 件までの固定長のバッファに溜め、メトリクスの送信が成功した直後にまとめて送ります。Collector に届かない間もイベントは発生し続けるので、上限が無いと通信障害がそのままメモリ不足によるリセットになります。上限を超えたら古いものから押し出し、その件数を `device.logs.dropped` で送ります。タイムスタンプはイベントが起きた時刻なので、障害中のイベントもあとから正しい時刻で並びます。

実機で Collector を 25 秒止めたところ、`export failed` は Collector が止まっていた 07:04:54 の時刻で、`export recovered`（`failed_attempts=2`）は復帰後の 07:05:17 の時刻で Loki に届きました。

### トレース

送信の1周期を1つのトレースにしています。

| スパン | 種類 | 内容 |
| --- | --- | --- |
| `export cycle` | INTERNAL | 1周期全体（ルート） |
| `collect` | INTERNAL | メモリ統計、RSSI、アリーナの読み取り |
| `encode` | INTERNAL | メトリクスのエンコード。属性 `otlp.encoding`、`otlp.payload.size` |
| `POST /v1/metrics` | CLIENT | Collector への HTTP リクエスト。失敗するとステータスが ERROR になる |

実機で計測した1周期の例は、全体 14.5 ms、`collect` 0.47 ms、`encode` 2.31 ms、`POST /v1/metrics` 11.34 ms でした。

トレース ID とスパン ID は `crypto/rand` で作っています。TinyGo は esp32s3 の `crypto/rand` をハードウェアの乱数生成器（`machine.GetRNG`）で実装しています。

ログと同じく常に JSON で送り、16 件の固定長バッファに溜めます。ある周期のスパンは次の周期のメトリクス送信が成功したときに送ります。送信のスパンは送信が終わるまで閉じられないからです。

OTLP/JSON はトレース ID とスパン ID を 16 進文字列で書きます。protobuf の canonical JSON（base64）とは違う、OTLP の仕様上の例外です。`protojson` は canonical JSON に従うので、16 進の ID を受け付けはするものの base64 として誤ったバイト列に解釈します。そのためテストでは ID を base64 に書き換えてから `protojson` で構造を確かめ、16 進であることは別のテストで固定しています。Collector が 16 進を正しく読むことは、実際の Collector と Tempo に送って確認しました。

Tempo の span metrics generator がスパンから RED メトリクス（`traces_spanmetrics_*`）を作るので、ダッシュボードではスパンごとの所要時間の推移も見られます。

### リソース属性

リソース属性は `service.name`、`service.version`、`device.id`、`device.model.identifier` の 4 件です。`deployment.environment.name` と `telemetry.sdk.*` はデバイスではなく Collector の `resource` プロセッサーで付けています。

## 動かす

### バックエンドを立てる

```sh
make stack     # edge collector (:4319) + grafana/otel-lgtm (:3000)
```

構成は「デバイス → edge collector → otel-lgtm」です。デバイスは LAN 内の Collector に平文の OTLP/HTTP で送り、Collector が属性を足して Grafana 側へ転送します。外部のバックエンドへ送るなら TLS と認証情報は Collector に置きます。

Grafana は `http://localhost:3000/d/tinygo-device` で、ダッシュボード `TinyGo ESP32 device` がプロビジョニングされています。メトリクスのパネル8枚、ログのパネル1枚、トレースのパネル2枚があります。ダッシュボードの JSON は `grafana/gen_dashboard.py` から生成しているので、変更するときはそちらを直します。

![ダッシュボード](../_review/grafana-dashboard-2026-10-02.png)

ホストの 4318 番はローカルの Alloy などが使っていることが多いので、ホスト側のポートは 4319 にしています（`COLLECTOR_PORT` で変更できます）。

### ボードなしで確かめる

`cmd/hostsim` は同じエンコーダとエクスポーターをワークステーションで動かします。デモ中に送信が失敗したとき、hostsim が同じ Collector に対して成功するなら、原因はデバイス側にあると分かります。

```sh
make encode    # 送信せずにエンコード結果のバイト数だけ表示
make hostsim   # Collector へ 3 回送る
```

### 実機に書き込む

```sh
make flash SSID=yournet PASSWORD=yourpass \
  ENDPOINT=http://192.168.0.52:4319/v1/metrics
```

`ENDPOINT` には Collector を動かしているマシンの LAN アドレスを指定します。`localhost` はデバイスから届きません。認証情報は `-ldflags` でバイナリに埋め込み、ファイルには書きません。

ビルドタグは 2 つあり、独立に選べます。既定は `socket otlpjson` です。

| `TAGS` | エンコーダ | HTTP クライアント |
| --- | --- | --- |
| `socket otlpjson`（既定） | `encoding/json` | 手書き |
| `socket` | 手書き protobuf | 手書き |
| `otlpjson` | `encoding/json` | `net/http` |
| `""` | 手書き protobuf | `net/http` |

既定の `socket otlpjson` は自前のコードが一番少ない組み合わせです。手書きエンコーダが要らず、OTLP の本体は標準ライブラリだけで話せます。

Linux で `/dev/ttyACM0` が `Permission denied` になる場合は `dialout` グループに入ります（`sudo usermod -aG dialout $USER` のあと再ログイン）。

## 計測結果

### バイナリサイズと RAM

`cmd/device` 全体（Wi-Fi 接続、SNTP、RSSI、メトリクス、ログ、トレースの送信）、`esp32s3-generic`、`-stack-size=16KB`。2026-10-02 に現行コードで計測しました。

| メトリクスのエンコーダ | HTTP クライアント | flash | RAM |
| --- | --- | --- | --- |
| 手書き protobuf | `net/http` | 1,067,327 | 147,636 |
| `encoding/json` | `net/http` | 1,067,175 | 147,636 |
| 手書き protobuf | 手書き（`socket`） | 748,907 | 114,796 |
| `encoding/json` | 手書き（`socket`） | 748,691 | 114,796 |

`net/http` をやめると flash が約 32 万バイト（30%）減ります。`net/http` は `crypto/tls` を引き込み、そこから `crypto/x509`、`encoding/asn1`、`math/big` が付いてきます。このデモは平文の HTTP しか話さないので、この分は何にも使われていません。

ログとトレースはエンコーダの選択にかかわらず `encoding/json` で送るので、メトリクスのエンコーダを替えても差は 216 バイト（socket 版）しかありません。メトリクスだけを送っていた版では、protobuf が 739,467、JSON が 740,007 で、JSON のほうが 540 バイト大きい結果でした。

サイズを測るときは `ssid` と `endpoint` の両方を設定する必要があります。どちらかが空だと TinyGo がネットワーク処理か送信ループを到達不能と判断して削除し、実際より約 6 分の 1 のサイズを報告します。`make build-all` と `make sizes` は `SIZE_LDFLAGS` で両方を埋めています。

### ペイロード

10 ストリームのペイロードは、実機のシリアル出力で次のとおりでした。

| エンコーディング | バイト数 |
| --- | --- |
| バイナリ protobuf | 1,379 |
| JSON | 3,016（2.19 倍） |

10 秒間隔なら JSON でも 300 B/s 程度なので、LAN 内のデモでは差になりません。電池駆動や LPWAN のように帯域が限られる環境では protobuf を選ぶ理由があります。

### メモリ

この用途ではメモリは足りています。

| 項目 | バイト数 | DRAM 比 |
| --- | --- | --- |
| TinyGo が使える DRAM | 425,984 | 100% |
| ヒープの上限 | 298,287 | 70% |
| GC 後のヒープ（メトリクスのみ） | 約 124,000〜179,000 | 29〜42% |
| GC 後のヒープ（3 シグナル） | 約 204,000〜206,000 | 48% |

当初は送信ごとにヒープが 8,272 バイト増えるのを見てリークを疑いましたが、正体は使い捨ての確保（churn）でした。原因は 2 つあり、どちらも直しました。

- lneto はダイヤルごとに TxBuf 4,096 バイトと RxBuf 1,024 バイトを確保します。接続を使い回すようにしました。
- `json.Marshal` は呼ぶたびに新しいバッファを作ります（`encode.go` の `append([]byte(nil), ...)`）。`json.Encoder` と使い回しの `bytes.Buffer` に替えました。

| 実装 | 1 送信あたりのヒープ増加 |
| --- | --- |
| ダイヤル毎回、`json.Marshal` | 8,272 |
| 接続を使い回す | 4,816 |
| さらにバッファを使い回す | 688 |
| 応答の読み切りと partialSuccess 解析を追加（JSON、メトリクスのみ） | 800〜848 |
| メトリクスのみ、手書き protobuf | 2,752 |
| ログとトレースを追加（現行、JSON） | 2,992 |

メトリクスのみの JSON 版は 30 分の連続試験で 180 回中 176 回がちょうど 800 バイトでした。例外の 2 回は接続確立と一致し、初回接続が 46,848 バイト、Collector 再起動後の再接続が 6,944 バイトです。GC は約 20 分に 1 回です。

ログとトレースを足した現行版は、送信サイクルごとに 3 回 POST し、スパン 4 本ぶんの ID の hex 化と文字列の組み立てが加わるので 2,992 バイトになります。30 分の試験では、GC をまたがない送信間の差分 172 回のうち 170 回がこの値でした。残りの 2 回は接続の確立と一致し、初回接続が 80,560 バイト、Collector 再起動後の再接続が 9,136 バイトです。試験のハーネスが出す平均値（3,708 バイト）はこの 2 回を含みます。GC は約 5 分に 1 回です。

protobuf 版の 2,752 バイトは JSON 版より大きく、原因はまだ調べていません。ホストのベンチマークでは手書きエンコーダの確保は 0 回なので、エンコーダ以外（応答の解析など）の経路だと見ています。

espradio のアリーナは 31,312 / 49,144 バイトで一定で、Wi-Fi ドライバ側の使用量は増えません。

### 連続稼働

JSON + socket、16KB スタック、10 秒間隔で 30 分。15 分の時点で Collector を再起動します。

| 項目 | メトリクスのみ（`8e0b7ea`） | 3 シグナル（2026-10-02） |
| --- | --- | --- |
| 送信 | 180 回 | 180 回 |
| 失敗 | 0 回 | 0 回 |
| 1 送信あたりのヒープの増加 | 800 バイト | 2,992 バイト（172 回中 170 回） |
| GC | 1 回 | 7 回（約 5 分おき） |
| GC 後のヒープ | 178,704 | 203,776〜206,128 |
| Collector 再起動からの回復 | 6.1 秒 | 9.1 秒 |

3 シグナル版では GC 後のヒープが 7 回とも 204KB 前後で、増え続けていません。回復までの時間は、再起動から次の送信サイクルまでの待ちで決まり、6.1 秒と 9.1 秒の差は再起動のタイミングによるものです。

試験のハーネスは `../_review/soak_restart.py` です。

## 設計上の判断

### 公式の SDK とエクスポーターは使わない

| 構成 | 結果 |
| --- | --- |
| `otel/metric` + `metric/noop`（API のみ） | ビルド成功（6,400 バイト） |
| `otel/sdk/metric` | ビルド失敗。`sdk/resource` → `golang.org/x/sys/unix` で `system calls are not supported` |
| `otlpmetrichttp` | ビルド失敗。`envconfig.go:143:19: undefined: tls.X509KeyPair` |

落ちる理由はメモリではなく、SDK 層がホスト OS を前提にしていることです。

### 生成された protobuf 型は使わない

`go.opentelemetry.io/proto/otlp` の生成型はクロスコンパイルが通り、ホストのテストも通りますが、実機では最初のマーシャルで落ちます。

```
panic: unimplemented: (reflect.Type).MethodByName()
```

protobuf-go の `makeStructInfo`（`internal/impl/message.go`）が `XXX_OneofFuncs` と `XXX_OneofWrappers` を探すために `MethodByName` を呼び、TinyGo の `reflect` はこのメソッドが未実装です。

`dynamicpb` 経由のマーシャルは実機で動きます。ただし同じ 8 ストリームのペイロードを組み立てるのに 1 回あたり 29,277 バイトを確保するので、常時送信には向きません（手書きエンコーダは 0 バイト）。

### OTLP/JSON は回避策ではなくプロトコルそのもの

OTLP/HTTP は protobuf と JSON の 2 つのエンコーディングを仕様として定めており、Collector はどちらも受け付けます。`encoding/json` は実機で動くので、OTLP は標準ライブラリだけで話せます。

条件が 1 つあり、`encoding/json` は入れ子のメッセージを再帰的に処理するので既定の 8KB スタックでは足りません。

### スタックサイズは 16KB

実機（JSON + socket）で境界を測りました。

| `-stack-size` | 結果 | 症状 |
| --- | --- | --- |
| 8KB（既定） | 落ちる | `EXCCAUSE = 28 (LoadProhibited)` |
| 12KB | 落ちる | `EXCCAUSE = 28 (LoadProhibited)` |
| 14KB | 落ちる | `fatal error: goroutine stack overflow` |
| 15KB | 動く | |
| 16KB | 動く | 30 分の連続試験を通過 |

12KB 以下では、TinyGo がスタック不足を報告する前にメモリが壊れて `LoadProhibited` になります。スタック不足のエラーが出ないからといって、スタックが足りているとは言えません。

`-stack-size` は単位が必須です（`-stack-size=16384` は `Unrecognized size suffix` でエラーになります）。

### TLS はデバイスに載せない

espradio v0.3.0 に動作する TLS クライアントはありません。デバイスは同じネットワーク上の Collector に平文で送り、外向きの TLS は Collector が受け持ちます。

### 時刻同期は自分で書く

OTLP の cumulative sum には `StartTimeUnixNano` が必要で、これが無いとバックエンドはカウンターのリセットとデータの欠損を区別できません。

espradio の lneto スタックは NTP を実装していますが、`netlink.NetConnect` が `StackConfig.NTPServer` をゼロ値のまま残し、`DoNTP` に届く `rstack()` は非公開です。そのため `sntp` パッケージで SNTP を直接話し、`runtime.AdjustTimeOffset` で時計を合わせます。パケット処理はホストでテストできるように `cmd/device` から切り出しています。応答は RFC 4330 の規則（モード、バージョン、stratum 0 の kiss-of-death、LI=3 の非同期）で検証してから時計に反映し、2036 年の桁あふれにも対応しています。

### RSSI はブロブの関数を直接呼ぶ

espradio v0.3.0 には接続中の AP の RSSI を返す Go の関数がありません。`espradio.Scan()` が返すのは周辺の AP です。ただし `esp_wifi_sta_get_rssi` はリンク済みの Wi-Fi ブロブ（`libnet80211.a`）にあるので、`cmd/device/rssi.go` の cgo プリアンブルでプロトタイプを宣言して直接呼んでいます。espradio は変更していません。

### 送信の失敗は 3 種類に分ける

- 接続断と 429、502、503、504 は再試行します（OTLP 仕様の再試行対象）
- 400 などそれ以外の応答は再試行しません。同じバイト列を送り直しても同じ結果になるからです
- HTTP 200 でも `partialSuccess.rejectedDataPoints` が 0 でなければ `partial_rejection` として数えます。成功に含めると、データの欠損が成功率の裏に隠れます

応答はヘッダーを CRLFCRLF まで、本文を `Content-Length` まで読み切ってから接続を再利用します。

### エンコーダの正しさの確かめ方

- 手書き protobuf は、出力したバイト列を公式の protobuf ランタイムで `Unmarshal` して照合します（ホストでは公式実装が動くため）
- JSON は、出力を `protojson`（OTLP の JSON マッピングの参照実装）で `Unmarshal` します。`DiscardUnknown` は false のままなので、フィールド名の綴り誤りはテストで落ちます
- `timeUnixNano` は uint64 のナノ秒で、JSON の数値（float64）にすると精度を失うので、文字列で出していることをテストで固定しています

## 既知の注意点

### 起動時の SHA-256 警告

TinyGo でビルドしたイメージはすべて、起動時にこの警告を出します。プログラムは正常に動きます。

```
SHA-256 comparison failed:
Calculated: ...
Expected: ...
Attempting to boot anyway...
```

イメージ内の SHA-256 はファイルの内容と一致し、フラッシュの内容もファイルと一致し、esptool の `image-info` も `Validation hash: valid` と判定します。ROM が計算する値だけが違い、ROM が何をハッシュしているかは特定できていません。7KB の hello world でも再現します。TinyGo の `builder/esp.go`（v0.42.0）には「ハッシュは付けない」というコメントと `hash_appended: true` の設定が並んでいて、この食い違いが原因の候補です。

### CoreS3 の AXP2101

画面と Grove Port A の電源は AXP2101 PMU が管理していて、初期化しないと給電されません。このファームウェアは AXP2101 を触らないので、画面はずっと消えたままです。Port A に I2C センサーを付けるなら、先に AXP2101 を初期化する必要があります。

実機が無応答になったときは、USB を抜いても内蔵バッテリーから給電が続くので電源が切れません。ボトムのベースを外して電源ボタンを 10 秒押し、ベースを戻さずに USB を挿し直すと復帰します。

### PSRAM は使えない

CoreS3 は 8MB の PSRAM を持っていますが、TinyGo 0.42.0 の `esp32s3.ld` は内部 SRAM（416KB）だけを扱います。ESP32-S3 の PSRAM 対応は PR #5554 で進行中です。このデモの作業セットは内部 SRAM に収まるので、PSRAM が入っても変わりません。

## 未実装と未検証

- センサー（温湿度など）は接続していません
- `net/http` 版は実機で長時間動かしていません（ビルドは通ります）
- ESP32-C3 は実機で試していません

## 構成

```
wire/                   手書きの protobuf エンコーダ（reflect 不使用）
otlpmini/
  registry.go           手書き protobuf での OTLP エンコード
  encoder.go            エンコーダのインターフェース
  exporter.go           net/http 版の送信
  exporter_socket.go    手書き HTTP/1.1 クライアント（-tags socket）
  http_util.go          応答ヘッダーの解析
  partial_success.go    partialSuccess の解析（protobuf と JSON）
otlpjson/
  otlpjson.go           encoding/json での OTLP メトリクスのエンコード（-tags otlpjson）
  logs.go               固定長のログバッファと OTLP ログのエンコード
  traces.go             固定長のスパンバッファと OTLP トレースのエンコード
sntp/                   SNTP のパケット処理（ホストでテスト可能）
cmd/device/
  main.go               ファームウェア（//go:build tinygo）
  sntp.go               SNTP のソケット処理
  rssi.go               esp_wifi_sta_get_rssi の呼び出し
  logs.go               デバイスのイベントの記録と送信
  traces.go             送信周期のスパンの記録と送信
  encoding_*.go         ビルドタグによるエンコーダの選択
cmd/hostsim/            ワークステーション用のハーネス
collector/config.yaml   edge collector（3シグナルを受信、属性を付与、otel-lgtm へ転送）
grafana/                ダッシュボード（gen_dashboard.py で生成）とプロビジョニング設定
docker-compose.yaml     edge collector + grafana/otel-lgtm
```
