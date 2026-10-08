// Author: Yoshi Yamaguchi <yoshi@grafana.com>
// SPDX-License-Identifier: Apache-2.0

package otlpmini

import "strconv"

// parsePartialSuccess extracts a partial-success report from a collector
// response body, or returns nil when every data point was accepted.
//
// The response mirrors the request encoding: a protobuf request gets an
// ExportMetricsServiceResponse in binary protobuf, a JSON request gets the
// same message in OTLP JSON. Both are handled here so the exporter does not
// need to know which encoder produced the request.
//
// An empty body, `{}`, or a partialSuccess with zero rejected points all mean
// full success, which is what a healthy collector returns.
func parsePartialSuccess(body []byte) *PartialSuccess {
	b := trimSpace(body)
	if len(b) == 0 {
		return nil
	}
	if b[0] == '{' {
		return parsePartialSuccessJSON(b)
	}
	return parsePartialSuccessProto(b)
}

// Field numbers from opentelemetry-proto
// collector/metrics/v1/metrics_service.proto.
const (
	fieldResponsePartialSuccess = 1
	fieldPartialRejectedPoints  = 1
	fieldPartialErrorMessage    = 2
)

// parsePartialSuccessProto walks just enough of the protobuf wire format to
// read partial_success. A full decoder is not needed and would pull in the
// protobuf runtime, which panics on this target.
func parsePartialSuccessProto(b []byte) *PartialSuccess {
	for len(b) > 0 {
		field, wireType, rest, ok := readTag(b)
		if !ok {
			return nil
		}
		b = rest
		switch wireType {
		case 0:
			_, b, ok = readVarint(b)
		case 1:
			if len(b) < 8 {
				return nil
			}
			b = b[8:]
		case 2:
			var n uint64
			n, b, ok = readVarint(b)
			if !ok || uint64(len(b)) < n {
				return nil
			}
			if field == fieldResponsePartialSuccess {
				return decodePartialSuccessMessage(b[:n])
			}
			b = b[n:]
		case 5:
			if len(b) < 4 {
				return nil
			}
			b = b[4:]
		default:
			return nil
		}
		if !ok {
			return nil
		}
	}
	return nil
}

func decodePartialSuccessMessage(b []byte) *PartialSuccess {
	out := &PartialSuccess{}
	for len(b) > 0 {
		field, wireType, rest, ok := readTag(b)
		if !ok {
			return nil
		}
		b = rest
		switch {
		case field == fieldPartialRejectedPoints && wireType == 0:
			var v uint64
			v, b, ok = readVarint(b)
			if !ok {
				return nil
			}
			out.RejectedDataPoints = int64(v)
		case field == fieldPartialErrorMessage && wireType == 2:
			var n uint64
			n, b, ok = readVarint(b)
			if !ok || uint64(len(b)) < n {
				return nil
			}
			out.ErrorMessage = string(b[:n])
			b = b[n:]
		default:
			switch wireType {
			case 0:
				_, b, ok = readVarint(b)
			case 1:
				if len(b) < 8 {
					return nil
				}
				b = b[8:]
			case 2:
				var n uint64
				n, b, ok = readVarint(b)
				if !ok || uint64(len(b)) < n {
					return nil
				}
				b = b[n:]
			case 5:
				if len(b) < 4 {
					return nil
				}
				b = b[4:]
			default:
				return nil
			}
			if !ok {
				return nil
			}
		}
	}
	// A present but all-zero partialSuccess is what success looks like.
	if out.RejectedDataPoints == 0 && out.ErrorMessage == "" {
		return nil
	}
	return out
}

func readTag(b []byte) (field int, wireType int, rest []byte, ok bool) {
	v, rest, ok := readVarint(b)
	if !ok {
		return 0, 0, nil, false
	}
	return int(v >> 3), int(v & 7), rest, true
}

func readVarint(b []byte) (uint64, []byte, bool) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i] < 0x80 {
			return v, b[i+1:], true
		}
	}
	return 0, nil, false
}

// parsePartialSuccessJSON reads the OTLP JSON form. A hand parser is used
// rather than encoding/json so that the socket build does not depend on the
// JSON encoder, which the protobuf path would otherwise not need.
//
// rejectedDataPoints is an int64 and OTLP JSON permits it as a string or a
// number, so both are accepted.
func parsePartialSuccessJSON(b []byte) *PartialSuccess {
	ps, ok := jsonFindKey(b, "partialSuccess")
	if !ok {
		return nil
	}
	out := &PartialSuccess{}
	if v, ok := jsonFindKey(ps, "rejectedDataPoints"); ok {
		if n, err := strconv.ParseInt(string(jsonUnquote(v)), 10, 64); err == nil {
			out.RejectedDataPoints = n
		}
	}
	if v, ok := jsonFindKey(ps, "errorMessage"); ok {
		out.ErrorMessage = string(jsonUnquote(v))
	}
	if out.RejectedDataPoints == 0 && out.ErrorMessage == "" {
		return nil
	}
	return out
}

// jsonFindKey returns the raw value for a top-level key within one JSON
// object. It does not validate the document; the collector produces it.
func jsonFindKey(b []byte, key string) ([]byte, bool) {
	quoted := `"` + key + `"`
	idx := -1
	for i := 0; i+len(quoted) <= len(b); i++ {
		if string(b[i:i+len(quoted)]) == quoted {
			idx = i + len(quoted)
			break
		}
	}
	if idx < 0 {
		return nil, false
	}
	for idx < len(b) && (b[idx] == ' ' || b[idx] == ':' || b[idx] == '\t' ||
		b[idx] == '\n' || b[idx] == '\r') {
		idx++
	}
	if idx >= len(b) {
		return nil, false
	}
	switch b[idx] {
	case '{', '[':
		open, close := b[idx], byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 0
		for i := idx; i < len(b); i++ {
			switch b[i] {
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return b[idx : i+1], true
				}
			}
		}
		return nil, false
	case '"':
		for i := idx + 1; i < len(b); i++ {
			if b[i] == '\\' {
				i++
				continue
			}
			if b[i] == '"' {
				return b[idx : i+1], true
			}
		}
		return nil, false
	default:
		end := idx
		for end < len(b) && b[end] != ',' && b[end] != '}' && b[end] != ']' {
			end++
		}
		return trimSpace(b[idx:end]), true
	}
}

func jsonUnquote(b []byte) []byte {
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		return b[1 : len(b)-1]
	}
	return b
}
