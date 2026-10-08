// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

package otlpmini

import "strconv"

// PartialSuccess reports that the collector accepted a request but rejected
// some of the data in it.
//
// OTLP puts this in the body of an HTTP 200, so treating 200 as full success
// hides data loss. The exporter surfaces it instead of silently counting the
// export as clean.
//
// A partial rejection is not retryable: resending the same payload produces
// the same rejection. See
// https://opentelemetry.io/docs/specs/otlp/#partial-success-1
type PartialSuccess struct {
	RejectedDataPoints int64
	ErrorMessage       string
}

func (p *PartialSuccess) Error() string {
	s := "otlpmini: collector rejected " +
		strconv.FormatInt(p.RejectedDataPoints, 10) + " data points"
	if p.ErrorMessage != "" {
		s += ": " + p.ErrorMessage
	}
	return s
}

// byte helpers
//
// These exist instead of bytes/strings calls so the socket build keeps its
// dependency list minimal, and so the header parsing below is
// case-insensitive, as HTTP requires.

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func indexByteSlice(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

// indexCRLFCRLF finds the end of the header block.
func indexCRLFCRLF(b []byte) int {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == '\r' && b[i+1] == '\n' && b[i+2] == '\r' && b[i+3] == '\n' {
			return i
		}
	}
	return -1
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// equalFold compares a byte slice with a lowercase literal, ignoring case.
func equalFold(b []byte, lowerLiteral string) bool {
	if len(b) != len(lowerLiteral) {
		return false
	}
	for i := 0; i < len(b); i++ {
		if lowerByte(b[i]) != lowerLiteral[i] {
			return false
		}
	}
	return true
}

// containsFold reports whether b contains the lowercase literal, ignoring case.
func containsFold(b []byte, lowerLiteral string) bool {
	if len(lowerLiteral) == 0 {
		return true
	}
	for i := 0; i+len(lowerLiteral) <= len(b); i++ {
		match := true
		for j := 0; j < len(lowerLiteral); j++ {
			if lowerByte(b[i+j]) != lowerLiteral[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

// headerValue returns the value of a header, matching the name
// case-insensitively as HTTP requires. head is the header block without the
// trailing CRLFCRLF.
func headerValue(head []byte, lowerName string) ([]byte, bool) {
	// Skip the status line.
	i := 0
	for i < len(head) {
		if head[i] == '\n' {
			i++
			break
		}
		i++
	}
	for i < len(head) {
		lineEnd := i
		for lineEnd < len(head) && head[lineEnd] != '\n' {
			lineEnd++
		}
		line := head[i:lineEnd]
		if colon := indexByteSlice(line, ':'); colon > 0 {
			if equalFold(trimSpace(line[:colon]), lowerName) {
				return trimSpace(line[colon+1:]), true
			}
		}
		i = lineEnd + 1
	}
	return nil, false
}

// headerHasToken reports whether a header contains a token, both compared
// case-insensitively. "Connection: close" and "CONNECTION: Close" are the same
// header in HTTP, so matching literal spellings is wrong.
func headerHasToken(head []byte, lowerName, lowerToken string) bool {
	v, ok := headerValue(head, lowerName)
	if !ok {
		return false
	}
	return containsFold(v, lowerToken)
}

// siblingPath replaces the last path segment of an OTLP signal path, so that
// /v1/metrics becomes /v1/logs. A path without the standard /v1/<signal>
// suffix gets /v1/<signal> appended, which is what the OTLP spec derives
// from a base endpoint.
func siblingPath(path, signal string) string {
	const v1 = "/v1/"
	for i := len(path) - len(v1); i >= 0; i-- {
		if path[i:i+len(v1)] == v1 {
			return path[:i] + v1 + signal
		}
	}
	if len(path) > 0 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path + v1 + signal
}
