package otlpmini

// Encoder is what the firmware needs from an OTLP encoder: turn the current
// instrument values into a request body, and say what Content-Type describes
// it.
//
// Two implementations exist because OTLP/HTTP specifies two encodings and the
// choice is a real trade-off on a microcontroller rather than a detail:
//
//   - otlpjson uses encoding/json. It is the standard library only, so it is
//     the shortest correct path from TinyGo to an OpenTelemetry pipeline.
//   - wire hand-writes the binary protobuf encoding, because
//     google.golang.org/protobuf compiles for these targets but panics at run
//     time (reflect.Type.MethodByName is unimplemented in TinyGo).
//
// The firmware depends on this interface so that a build tag selects the
// encoding without touching the measurement code.
type Encoder interface {
	// Encode returns the request body for the current values.
	//
	// startUnixNano is when the firmware started measuring; OTLP requires it
	// on cumulative sums so a backend can tell a counter reset from a gap.
	Encode(startUnixNano, nowUnixNano uint64) ([]byte, error)

	// ContentType reports the Content-Type for the encoded body.
	ContentType() string
}
