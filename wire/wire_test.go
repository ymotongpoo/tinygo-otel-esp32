package wire

import (
	"testing"

	cpb "go.opentelemetry.io/proto/otlp/common/v1"
	mpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// decodeLen reads the length-delimited field that Nested wrote and returns the
// declared length plus the bytes that actually follow it.
//
// This checks the framing directly. Decoding a synthetic tree with protobuf
// would also fail for reasons unrelated to framing, which would hide the bug
// being tested.
func decodeLen(t *testing.T, b []byte) (declared int, payload []byte) {
	t.Helper()
	if len(b) == 0 {
		t.Fatal("empty buffer")
	}
	// Skip the tag varint.
	i := 0
	for i < len(b) && b[i] >= 0x80 {
		i++
	}
	i++
	var v uint64
	shift := uint(0)
	for ; i < len(b); i++ {
		v |= uint64(b[i]&0x7f) << shift
		if b[i] < 0x80 {
			i++
			break
		}
		shift += 7
	}
	return int(v), b[i:]
}

// Nested reserves a fixed-width length prefix and closes the gap afterwards.
// A message whose contents need a 4-byte length varint overflows that
// reservation, truncating the payload instead of reporting an error.
func TestNestedLengthBoundaries(t *testing.T) {
	for _, size := range []int{
		0, 1,
		126, 127, 128, // 1 -> 2 byte varint
		16382, 16383, 16384, // 2 -> 3
		2097150, 2097151, 2097152, // 3 -> 4
		1 << 22, // comfortably into 4-byte territory
	} {
		w := NewBuffer(64)
		// Position-dependent content. All-zero filler is invariant under an
		// off-by-one shift, so it would hide exactly the bug this checks.
		content := make([]byte, size)
		for i := range content {
			content[i] = byte(i%251 + 1)
		}

		w.Nested(FieldMetricsDataResourceMetrics, func() {
			// Raw bytes: this test is about framing, not about the inner
			// message being a valid Resource.
			w.b = append(w.b, content...)
		})

		declared, payload := decodeLen(t, w.Bytes())
		if declared != size {
			t.Errorf("size %d: declared length %d", size, declared)
		}
		if len(payload) != size {
			t.Errorf("size %d: %d bytes followed the prefix (lost %d)",
				size, len(payload), size-len(payload))
			continue
		}
		for i := range content {
			if payload[i] != content[i] {
				t.Errorf("size %d: content corrupted at byte %d (%#x != %#x)",
					size, i, payload[i], content[i])
				break
			}
		}
	}
}

// The same case with a buffer that must grow while the length is patched.
func TestNestedFourByteLengthWithTightCapacity(t *testing.T) {
	const size = 1 << 21 // needs a 4-byte length varint

	w := NewBuffer(16)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("patching a 4-byte length panicked: %v", r)
		}
	}()
	w.Nested(FieldMetricsDataResourceMetrics, func() {
		w.b = append(w.b, make([]byte, size)...)
	})

	declared, payload := decodeLen(t, w.Bytes())
	if declared != size || len(payload) != size {
		t.Errorf("declared %d, payload %d, want %d", declared, len(payload), size)
	}
}

// A realistic payload large enough to cross the 3-byte boundary must still
// decode with the reference protobuf runtime.
func TestLargeRealPayloadRoundTrips(t *testing.T) {
	w := NewBuffer(4096)
	// Enough attributes to push the outer message past 2 MB.
	const attrs = 30000
	value := string(make([]byte, 64))
	for i := range value {
		_ = i
	}

	w.Nested(FieldMetricsDataResourceMetrics, func() {
		w.Nested(FieldResourceMetricsResource, func() {
			for i := 0; i < attrs; i++ {
				w.Nested(FieldResourceAttributes, func() {
					w.String(FieldKeyValueKey, "k")
					w.Nested(FieldKeyValueValue, func() {
						w.String(FieldAnyValueStringValue,
							"0123456789012345678901234567890123456789012345678901234567890123")
					})
				})
			}
		})
	})

	if got := w.Len(); got < 1<<21 {
		t.Fatalf("payload is %d bytes; the test needs more than %d", got, 1<<21)
	}

	var md mpb.MetricsData
	if err := proto.Unmarshal(w.Bytes(), &md); err != nil {
		t.Fatalf("a %d byte payload did not decode: %v", w.Len(), err)
	}
	if n := len(md.ResourceMetrics[0].Resource.Attributes); n != attrs {
		t.Errorf("attributes = %d, want %d", n, attrs)
	}
	var kv *cpb.KeyValue = md.ResourceMetrics[0].Resource.Attributes[0]
	if kv.Key != "k" {
		t.Errorf("first key = %q, want k", kv.Key)
	}
}

// A nested message with no contents is a valid zero-length field.
func TestNestedEmptyMessage(t *testing.T) {
	w := NewBuffer(64)
	w.Nested(FieldMetricsDataResourceMetrics, func() {})

	var md mpb.MetricsData
	if err := proto.Unmarshal(w.Bytes(), &md); err != nil {
		t.Fatalf("empty nested message did not decode: %v", err)
	}
	if len(md.ResourceMetrics) != 1 {
		t.Errorf("ResourceMetrics = %d, want 1 (present but empty)", len(md.ResourceMetrics))
	}
}
