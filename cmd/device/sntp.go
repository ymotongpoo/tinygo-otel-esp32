// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

//go:build tinygo

package main

import (
	"net"
	"runtime"
	"time"

	"github.com/ymotongpoo/tinygo-otel-esp32/sntp"
)

// The packet handling lives in the sntp package so it can be tested on a host.
// This file is only the socket plumbing, which needs the device.

// sntpQuery asks one server for the time over UDP port 123.
func sntpQuery(serverIP string, timeout time.Duration) (time.Time, error) {
	conn, err := net.DialTimeout("udp", serverIP+":123", timeout)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	if _, err := conn.Write(sntp.Request()); err != nil {
		return time.Time{}, err
	}

	resp := make([]byte, sntp.PacketSize)
	n, err := conn.Read(resp)
	if err != nil {
		return time.Time{}, err
	}

	sec, nsec, err := sntp.Parse(resp[:n])
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, nsec), nil
}

// applyClock moves the runtime clock to t.
//
// TinyGo has no settimeofday. runtime.AdjustTimeOffset shifts the monotonic
// clock's mapping to wall time by a delta, so the delta is computed against
// what the clock currently reads.
func applyClock(t time.Time) {
	runtime.AdjustTimeOffset(int64(t.Sub(time.Now())))
}
