// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

//go:build tinygo

package main

// espradio v0.3.0 links the Espressif WiFi blob (libnet80211.a), which
// implements esp_wifi_sta_get_rssi, but has no Go wrapper for it.
// espradio.Scan() is not a substitute: it reports nearby access points, not
// the link the station is associated with.
//
// The symbol is already in the binary, so declaring the prototype here is
// enough to call it. Nothing in espradio is modified.

/*
int esp_wifi_sta_get_rssi(int *rssi);
*/
import "C"

import "errors"

var errRSSI = errors.New("rssi: esp_wifi_sta_get_rssi failed")

// stationRSSI returns the RSSI of the associated access point in dBm.
func stationRSSI() (int, error) {
	var v C.int
	if code := C.esp_wifi_sta_get_rssi(&v); code != 0 {
		return 0, errRSSI
	}
	return int(v), nil
}
