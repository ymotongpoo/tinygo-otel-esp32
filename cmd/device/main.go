//go:build tinygo

// Command device is the firmware for the demo. It connects to WiFi, waits for
// the wall clock, then exports metrics to an OTLP collector on the local
// network.
//
// Build and flash:
//
//	tinygo flash -target=xiao-esp32s3 -monitor \
//	  -ldflags "-X main.ssid=NET -X main.password=PASS -X main.endpoint=http://192.168.1.10:4318/v1/metrics" \
//	  ./cmd/device
package main

import (
	"errors"
	"runtime"
	"strconv"
	"time"

	"github.com/ymotongpoo/tinygo-otel-esp32/otlpjson"
	"github.com/ymotongpoo/tinygo-otel-esp32/otlpmini"

	"tinygo.org/x/drivers/netdev"
	nl "tinygo.org/x/drivers/netlink"
	"tinygo.org/x/espradio"
	link "tinygo.org/x/espradio/netlink"
)

// Set with -ldflags "-X main.<name>=<value>". Credentials are not committed.
var (
	ssid     string
	password string
	endpoint string
	deviceID = "xiao-s3-01"
	target   = "xiao-esp32s3"
)

const exportInterval = 10 * time.Second

func main() {
	// Give the USB serial console time to attach so the first lines are not
	// lost.
	time.Sleep(2 * time.Second)

	if endpoint == "" {
		fatal("endpoint not set; build with -ldflags \"-X main.endpoint=...\"")
	}

	l := &link.Esplink{}
	netdev.UseNetdev(l)

	println("encoding:", encodingName)
	println("connecting to wifi:", ssid)
	if err := l.NetConnect(&nl.ConnectParams{Ssid: ssid, Passphrase: password}); err != nil {
		fatal("wifi connect: " + err.Error())
	}
	if addr, err := l.Addr(); err == nil {
		println("wifi connected, address:", addr.String())
	} else {
		println("wifi connected")
	}

	// A cumulative counter needs a real start time or the backend cannot tell
	// a reset from a gap. espradio does not sync the clock on its own, so ask
	// an SNTP server directly. See sntp.go for why.
	start := syncClockOrWarn(l)
	startNanos := uint64(start.UnixNano())

	resourceAttrs := []attr{
		{Key: "service.name", Value: "tinygo-otel-esp32"},
		{Key: "service.version", Value: "0.1.0"},
		{Key: "device.id", Value: deviceID},
		{Key: "device.model.identifier", Value: target},
	}
	reg := newEncoder("tinygo-otel-esp32", "0.1.0", resourceAttrs)

	uptime := reg.Register(&instrument{
		Name: "device.uptime", Unit: "ms", Kind: kindGauge,
		Desc: "Milliseconds since the firmware started measuring.",
	})
	heapInuse := reg.Register(&instrument{
		Name: "device.memory.usage", Unit: "By", Kind: kindGauge,
		Desc: "Heap bytes in use, from runtime.ReadMemStats.",
		Attrs: []attr{
			{Key: "device.memory.pool", Value: "go_heap"},
		},
	})
	heapTotal := reg.Register(&instrument{
		Name: "device.memory.limit", Unit: "By", Kind: kindGauge,
		Desc: "Total heap bytes available to the Go runtime.",
		Attrs: []attr{
			{Key: "device.memory.pool", Value: "go_heap"},
		},
	})
	// The WiFi blob allocates from a fixed arena that the Go GC does not see.
	// Exporting it is the only way to tell a Go heap problem from a radio
	// driver problem.
	arenaUsed := reg.Register(&instrument{
		Name: "device.memory.usage", Unit: "By", Kind: kindGauge,
		Desc: "Bytes in use in the espradio blob arena.",
		Attrs: []attr{
			{Key: "device.memory.pool", Value: "espradio_arena"},
		},
	})
	arenaCap := reg.Register(&instrument{
		Name: "device.memory.limit", Unit: "By", Kind: kindGauge,
		Desc: "Capacity of the espradio blob arena.",
		Attrs: []attr{
			{Key: "device.memory.pool", Value: "espradio_arena"},
		},
	})
	// Signal strength of the associated access point. A weak link explains
	// export failures that would otherwise look like a collector problem.
	rssi := reg.Register(&instrument{
		Name: "device.wifi.rssi", Unit: "dBm", Kind: kindGauge,
		Desc: "RSSI of the associated access point.",
	})
	payload := reg.Register(&instrument{
		Name: "device.export.payload", Unit: "By", Kind: kindGauge,
		Desc: "Encoded size of the previous OTLP payload.",
	})
	exportOK := reg.Register(&instrument{
		Name: "device.export.attempts", Unit: "{attempt}", Kind: kindCounter,
		Desc: "Exports to the collector, by outcome.",
		Attrs: []attr{
			{Key: "outcome", Value: "success"},
		},
	})
	exportFail := reg.Register(&instrument{
		Name: "device.export.attempts", Unit: "{attempt}", Kind: kindCounter,
		Desc: "Exports to the collector, by outcome.",
		Attrs: []attr{
			{Key: "outcome", Value: "failure"},
		},
	})
	// A partial rejection is neither a success nor a transport failure, and
	// folding it into either one hides data loss behind a healthy-looking
	// success rate.
	exportRejected := reg.Register(&instrument{
		Name: "device.export.attempts", Unit: "{attempt}", Kind: kindCounter,
		Desc: "Exports to the collector, by outcome.",
		Attrs: []attr{
			{Key: "outcome", Value: "partial_rejection"},
		},
	})

	logsDropped := reg.Register(&instrument{
		Name: "device.logs.dropped", Unit: "{record}", Kind: kindCounter,
		Desc: "Log records overwritten before they could be exported.",
	})

	spansDropped := reg.Register(&instrument{
		Name: "device.spans.dropped", Unit: "{span}", Kind: kindCounter,
		Desc: "Spans overwritten before they could be exported.",
	})

	exp := newExporter(endpoint)
	events := newEventLog(resourceAttrs, exp.SiblingPath("logs"))
	spans := newTracer(resourceAttrs, exp.SiblingPath("traces"))
	boot := time.Now()

	startAttrs := []otlpjson.Attr{{Key: "encoding", Value: encodingName}}
	if addr, err := l.Addr(); err == nil {
		startAttrs = append(startAttrs, otlpjson.Attr{Key: "network.local.address", Value: addr.String()})
	}
	if v, err := stationRSSI(); err == nil {
		startAttrs = append(startAttrs, otlpjson.Attr{Key: "wifi.rssi", Value: strconv.Itoa(v)})
	}
	events.add(otlpjson.SeverityInfo, "device started", startAttrs...)

	// Consecutive failures since the last success. Logging every failure
	// would fill the bounded buffer during an outage with identical records;
	// logging the transitions keeps the start and the end of the outage.
	failStreak := 0

	// Declared outside the loop: ReadMemStats fills it in place, so reusing
	// one value keeps the reporting path from allocating what it measures.
	var ms runtime.MemStats

	for {
		trace := newTraceID()
		cycle := spans.start(trace, otlpjson.SpanID{}, "export cycle", otlpjson.SpanKindInternal)

		collect := spans.start(trace, cycle.s.SpanID, "collect", otlpjson.SpanKindInternal)
		uptime.Set(float64(time.Since(boot).Milliseconds()))

		runtime.ReadMemStats(&ms)
		heapInuse.Set(float64(ms.HeapInuse))
		heapTotal.Set(float64(ms.HeapSys))

		if v, err := stationRSSI(); err == nil {
			rssi.Set(float64(v))
		}

		used, capacity := espradio.ArenaStats()
		arenaUsed.Set(float64(used))
		arenaCap.Set(float64(capacity))
		spans.end(collect, nil)

		// The counters and the payload gauge describe the previous export, so
		// they are collected before this attempt changes them.
		enc := spans.start(trace, cycle.s.SpanID, "encode", otlpjson.SpanKindInternal)
		body, err := reg.Encode(startNanos, uint64(time.Now().UnixNano()))
		spans.end(enc, err, otlpjson.Attr{Key: "otlp.encoding", Value: encodingName},
			otlpjson.Attr{Key: "otlp.payload.size", Value: strconv.Itoa(len(body))})
		if err != nil {
			// An encoding failure is a bug, not a transient fault, so it is
			// not counted as a failed export.
			println("encode failed:", err.Error())
			spans.end(cycle, err)
			time.Sleep(exportInterval)
			continue
		}

		send := spans.start(trace, cycle.s.SpanID, "POST /v1/metrics", otlpjson.SpanKindClient)
		n, err := exp.Export(body, reg.ContentType())
		spans.end(send, err, otlpjson.Attr{Key: "http.request.method", Value: "POST"},
			otlpjson.Attr{Key: "url.full", Value: endpoint})
		spans.end(cycle, err)
		switch {
		case err == nil:
			exportOK.Add(1)
			payload.Set(float64(n))
			if failStreak > 0 {
				events.add(otlpjson.SeverityInfo, "export recovered",
					otlpjson.Attr{Key: "failed_attempts", Value: strconv.Itoa(failStreak)})
				failStreak = 0
			}
			events.flush(exp)
			spans.flush(exp)
			logsDropped.Set(float64(events.buf.Dropped))
			spansDropped.Set(float64(spans.buf.Dropped))
			println("exported", n, "bytes; heap inuse", int(ms.HeapInuse),
				"arena", int(used), "/", int(capacity), "rssi", int(rssi.Value()))

		case errors.Is(err, otlpmini.ErrPartialSuccess):
			// The collector stored the request but dropped some points. That
			// is data loss, so it is not a success, but resending the same
			// payload would be rejected the same way.
			exportRejected.Add(1)
			payload.Set(float64(n))
			println("partially rejected:", err.Error())
			events.add(otlpjson.SeverityWarn, "collector rejected data points",
				otlpjson.Attr{Key: "error.message", Value: err.Error()})

		default:
			exportFail.Add(1)
			println("export failed:", err.Error())
			if failStreak == 0 {
				events.add(otlpjson.SeverityError, "export failed",
					otlpjson.Attr{Key: "error.message", Value: err.Error()})
			}
			failStreak++
		}

		time.Sleep(exportInterval)
	}
}

// ntpHost is resolved via DNS at startup. A pool name is used rather than a
// fixed address so the demo does not depend on one server being reachable.
const ntpHost = "pool.ntp.org"

// syncClockOrWarn sets the clock from SNTP and returns the resulting time.
//
// On failure it returns the unsynchronised clock and says so, rather than
// blocking forever: a device that exports with visibly wrong timestamps is
// easier to diagnose during a live demo than one that prints nothing.
func syncClockOrWarn(l *link.Esplink) time.Time {
	const (
		timeout = 5 * time.Second
		tries   = 3
	)

	addr, err := l.GetHostByName(ntpHost)
	if err != nil {
		println("sntp: cannot resolve", ntpHost+":", err.Error())
		println("warning: clock not synchronised; timestamps will be wrong")
		return time.Now()
	}
	println("sntp: resolved", ntpHost, "to", addr.String())

	for i := 0; i < tries; i++ {
		t, err := sntpQuery(addr.String(), timeout)
		if err == nil {
			applyClock(t)
			now := time.Now()
			println("sntp: clock set to", now.String())
			return now
		}
		println("sntp: attempt failed:", err.Error())
	}

	println("warning: clock not synchronised; timestamps will be wrong")
	return time.Now()
}

func fatal(msg string) {
	for {
		println("fatal:", msg)
		time.Sleep(5 * time.Second)
	}
}
