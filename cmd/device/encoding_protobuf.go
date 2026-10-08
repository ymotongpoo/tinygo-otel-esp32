// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

//go:build !otlpjson

package main

import "github.com/ymotongpoo/tinygo-otel-esp32/otlpmini"

// This build encodes OTLP as binary protobuf, hand-written by the wire
// package. It is the default because binary protobuf is what most
// OpenTelemetry deployments expect.
//
// The encoding is hand-written rather than delegated to
// google.golang.org/protobuf, which compiles for this target but panics on the
// first Marshal (reflect.Type.MethodByName is unimplemented in TinyGo).
//
// Build with -tags otlpjson to use encoding/json instead. The measurement code
// in main.go is identical either way: both encoders expose Register, Encode
// and ContentType, so only these aliases change.

const encodingName = "protobuf (hand-written)"

type (
	encoder    = otlpmini.Registry
	instrument = otlpmini.Instrument
	attr       = otlpmini.Attr
)

const (
	kindGauge   = otlpmini.KindGauge
	kindCounter = otlpmini.KindCounter
)

func newEncoder(scopeName, scopeVersion string, resourceAttrs []attr) *encoder {
	return otlpmini.NewRegistry(scopeName, scopeVersion, resourceAttrs)
}

// newExporter builds the HTTP exporter. The transport (net/http or a
// hand-written socket client) is selected by the socket build tag, which is
// independent of the encoding.
func newExporter(endpoint string) *otlpmini.Exporter {
	return otlpmini.NewExporter(endpoint)
}

type exporter = *otlpmini.Exporter
