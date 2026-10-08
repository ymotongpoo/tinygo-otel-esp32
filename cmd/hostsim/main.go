// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

//go:build !tinygo

// Command hostsim runs the same otlpmini pipeline on a workstation.
//
// It exists so that the encode path and the collector configuration can be
// verified without a board, and so that a failure during the live demo can be
// isolated: if hostsim succeeds against the same collector, the problem is on
// the device.
//
//	go run ./cmd/hostsim -endpoint http://localhost:4318/v1/metrics -n 3
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"time"

	"github.com/ymotongpoo/tinygo-otel-esp32/otlpmini"
)

func main() {
	endpoint := flag.String("endpoint", "http://localhost:4318/v1/metrics",
		"OTLP HTTP metrics endpoint")
	n := flag.Int("n", 1, "number of exports; 0 means run until interrupted")
	interval := flag.Duration("interval", 10*time.Second, "export interval")
	deviceID := flag.String("device-id", "hostsim-01", "value for device.id")
	encodeOnly := flag.Bool("encode-only", false,
		"encode and print the payload size without sending")
	flag.Parse()

	reg := otlpmini.NewRegistry("tinygo-otel-esp32", "0.1.0", []otlpmini.Attr{
		{Key: "service.name", Value: "tinygo-otel-esp32"},
		{Key: "service.version", Value: "0.1.0"},
		{Key: "device.id", Value: *deviceID},
		{Key: "device.model.identifier", Value: "hostsim"},
	})

	uptime := reg.Register(&otlpmini.Instrument{
		Name: "device.uptime", Unit: "ms", Kind: otlpmini.KindGauge,
		Desc: "Milliseconds since the firmware started measuring.",
	})
	heapInuse := reg.Register(&otlpmini.Instrument{
		Name: "device.memory.usage", Unit: "By", Kind: otlpmini.KindGauge,
		Desc:  "Heap bytes in use, from runtime.ReadMemStats.",
		Attrs: []otlpmini.Attr{{Key: "device.memory.pool", Value: "go_heap"}},
	})
	heapTotal := reg.Register(&otlpmini.Instrument{
		Name: "device.memory.limit", Unit: "By", Kind: otlpmini.KindGauge,
		Desc:  "Total heap bytes available to the Go runtime.",
		Attrs: []otlpmini.Attr{{Key: "device.memory.pool", Value: "go_heap"}},
	})
	arenaUsed := reg.Register(&otlpmini.Instrument{
		Name: "device.memory.usage", Unit: "By", Kind: otlpmini.KindGauge,
		Desc:  "Bytes in use in the espradio blob arena.",
		Attrs: []otlpmini.Attr{{Key: "device.memory.pool", Value: "espradio_arena"}},
	})
	arenaCap := reg.Register(&otlpmini.Instrument{
		Name: "device.memory.limit", Unit: "By", Kind: otlpmini.KindGauge,
		Desc:  "Capacity of the espradio blob arena.",
		Attrs: []otlpmini.Attr{{Key: "device.memory.pool", Value: "espradio_arena"}},
	})
	payload := reg.Register(&otlpmini.Instrument{
		Name: "device.export.payload", Unit: "By", Kind: otlpmini.KindGauge,
		Desc: "Encoded size of the previous OTLP payload.",
	})
	exportOK := reg.Register(&otlpmini.Instrument{
		Name: "device.export.attempts", Unit: "{attempt}", Kind: otlpmini.KindCounter,
		Desc:  "Exports to the collector, by outcome.",
		Attrs: []otlpmini.Attr{{Key: "outcome", Value: "success"}},
	})
	exportFail := reg.Register(&otlpmini.Instrument{
		Name: "device.export.attempts", Unit: "{attempt}", Kind: otlpmini.KindCounter,
		Desc:  "Exports to the collector, by outcome.",
		Attrs: []otlpmini.Attr{{Key: "outcome", Value: "failure"}},
	})

	exp := otlpmini.NewExporter(*endpoint)
	start := time.Now()
	startNanos := uint64(start.UnixNano())

	for i := 0; *n == 0 || i < *n; i++ {
		if i > 0 {
			time.Sleep(*interval)
		}

		uptime.Set(float64(time.Since(start).Milliseconds()))

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		heapInuse.Set(float64(ms.HeapInuse))
		heapTotal.Set(float64(ms.HeapSys))

		// There is no blob arena on a workstation. Stand-in values keep the
		// payload shape identical to the device so that the dashboard can be
		// built before the board is available.
		arenaCap.Set(65536)
		arenaUsed.Set(20000 + rand.Float64()*4000)

		encoded, err := reg.Encode(startNanos, uint64(time.Now().UnixNano()))
		if err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(1)
		}

		if *encodeOnly {
			fmt.Printf("encoded %d bytes (%d metric streams)\n", len(encoded), 8)
			payload.Set(float64(len(encoded)))
			continue
		}

		sent, err := exp.Export(encoded, reg.ContentType())
		if err != nil {
			exportFail.Add(1)
			fmt.Fprintln(os.Stderr, "export failed:", err)
			continue
		}
		exportOK.Add(1)
		payload.Set(float64(sent))
		fmt.Printf("exported %d bytes; heap inuse %d; success %d failure %d\n",
			sent, ms.HeapInuse,
			int64(math.Round(exportOK.Value())),
			int64(math.Round(exportFail.Value())))
	}
}
