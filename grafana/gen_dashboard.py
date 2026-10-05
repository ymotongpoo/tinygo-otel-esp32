# Generates tinygo-device.json. Edit this file, not the JSON:
#   python3 grafana/gen_dashboard.py

import json

DS = {"type": "prometheus", "uid": "prometheus"}


def ts(id_, title, x, y, w, h, targets, unit, extra=None):
    p = {
        "id": id_, "type": "timeseries", "title": title, "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 2, "fillOpacity": 10}}, "overrides": []},
        "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}},
        "targets": [dict(datasource=DS, refId=chr(65 + i), **t) for i, t in enumerate(targets)],
    }
    if extra:
        p["fieldConfig"]["defaults"].update(extra)
    return p


def stat(id_, title, x, y, w, h, expr, unit, thresholds=None):
    return {
        "id": id_, "type": "stat", "title": title, "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "fieldConfig": {"defaults": {"unit": unit, "thresholds": thresholds or {"mode": "absolute", "steps": [{"color": "green", "value": None}]}}, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "area"},
        "targets": [{"datasource": DS, "refId": "A", "expr": expr}],
    }


sel = '{service_name="tinygo-otel-esp32"}'
panels = [
    stat(1, "Uptime", 0, 0, 6, 4, f"device_uptime_milliseconds{sel}", "ms"),
    stat(2, "Wi-Fi RSSI", 6, 0, 6, 4, f"device_wifi_rssi_dBm{sel}", "dBm",
         {"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "orange", "value": -80}, {"color": "green", "value": -67}]}),
    stat(3, "Go heap in use", 12, 0, 6, 4, f'device_memory_usage_bytes{{service_name="tinygo-otel-esp32",device_memory_pool="go_heap"}}', "bytes"),
    stat(4, "Payload", 18, 0, 6, 4, f"device_export_payload_bytes{sel}", "bytes"),
    ts(5, "Go heap (sawtooth = GC)", 0, 4, 12, 9, [
        {"expr": 'device_memory_usage_bytes{service_name="tinygo-otel-esp32",device_memory_pool="go_heap"}', "legendFormat": "in use"},
        {"expr": 'device_memory_limit_bytes{service_name="tinygo-otel-esp32",device_memory_pool="go_heap"}', "legendFormat": "heap sys"},
    ], "bytes"),
    ts(6, "espradio arena (not visible to Go GC)", 12, 4, 12, 9, [
        {"expr": 'device_memory_usage_bytes{service_name="tinygo-otel-esp32",device_memory_pool="espradio_arena"}', "legendFormat": "in use"},
        {"expr": 'device_memory_limit_bytes{service_name="tinygo-otel-esp32",device_memory_pool="espradio_arena"}', "legendFormat": "capacity"},
    ], "bytes"),
    ts(7, "Exports per minute by outcome", 0, 13, 12, 8, [
        {"expr": f"sum by (outcome) (rate(device_export_attempts_total{sel}[2m])) * 60", "legendFormat": "{{outcome}}"},
    ], "short"),
    ts(8, "Wi-Fi RSSI", 12, 13, 12, 8, [
        {"expr": f"device_wifi_rssi_dBm{sel}", "legendFormat": "rssi"},
    ], "dBm"),
    {
        "id": 9, "type": "logs", "title": "Device events (OTLP logs)",
        "datasource": {"type": "loki", "uid": "loki"},
        "gridPos": {"x": 0, "y": 21, "w": 12, "h": 10},
        "options": {"showTime": True, "sortOrder": "Descending", "wrapLogMessage": True, "enableLogDetails": True},
        "targets": [{"datasource": {"type": "loki", "uid": "loki"}, "refId": "A",
                     "expr": '{service_name="tinygo-otel-esp32"}'}],
    },
    {
        "id": 10, "type": "table", "title": "Export cycles (OTLP traces)",
        "datasource": {"type": "tempo", "uid": "tempo"},
        "gridPos": {"x": 12, "y": 21, "w": 12, "h": 10},
        "targets": [{"datasource": {"type": "tempo", "uid": "tempo"}, "refId": "A",
                     "queryType": "traceql", "limit": 20, "tableType": "traces",
                     "query": '{resource.service.name="tinygo-otel-esp32"}'}],
    },
    ts(11, "Span duration p50 (Tempo span metrics)", 0, 31, 24, 8, [
        {"expr": 'histogram_quantile(0.5, sum by (le, span_name) (rate(traces_spanmetrics_latency_bucket{service="tinygo-otel-esp32"}[2m])))',
         "legendFormat": "p50 {{span_name}}"},
    ], "s"),
]

dash = {
    "uid": "tinygo-device", "title": "TinyGo ESP32 device", "schemaVersion": 39,
    "time": {"from": "now-15m", "to": "now"}, "refresh": "10s", "tags": ["tinygo", "esp32"],
    "panels": panels,
}
json.dump(dash, open(__import__("os").path.join(__import__("os").path.dirname(__file__), "tinygo-device.json"), "w"), indent=2)
print("ok")
