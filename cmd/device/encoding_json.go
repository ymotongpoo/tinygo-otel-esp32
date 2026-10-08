// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

//go:build otlpjson

package main

import (
	"github.com/ymotongpoo/tinygo-otel-esp32/otlpjson"
	"github.com/ymotongpoo/tinygo-otel-esp32/otlpmini"
)

// This build encodes OTLP as JSON with encoding/json.
//
// OTLP/HTTP specifies two encodings, binary protobuf and JSON, and a collector
// accepts either. The JSON path is therefore not a workaround but the
// protocol, and it needs nothing outside the standard library: no hand-written
// wire format and no third-party codec. On TinyGo that makes it the shortest
// correct route from a microcontroller to an OpenTelemetry pipeline.
//
// The cost is payload size. See the README for the measured comparison.

const encodingName = "json (encoding/json)"

type (
	encoder    = otlpjson.Encoder
	instrument = otlpjson.Instrument
	attr       = otlpjson.Attr
)

const (
	kindGauge   = otlpjson.KindGauge
	kindCounter = otlpjson.KindCounter
)

func newEncoder(scopeName, scopeVersion string, resourceAttrs []attr) *encoder {
	return otlpjson.NewEncoder(scopeName, scopeVersion, resourceAttrs)
}

// newExporter builds the HTTP exporter. otlpmini owns the transport for both
// encodings; only the body and its Content-Type differ.
func newExporter(endpoint string) *otlpmini.Exporter {
	return otlpmini.NewExporter(endpoint)
}

type exporter = *otlpmini.Exporter
