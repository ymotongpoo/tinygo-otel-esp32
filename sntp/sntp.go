// Package sntp implements the parts of SNTP that a microcontroller needs to
// set its clock.
//
// It exists because espradio's lneto stack implements NTP but
// netlink.Esplink does not expose it: NetConnect leaves
// StackConfig.NTPServer as the zero netip.Addr, so the stack never starts a
// sync, and the rstack() accessor that reaches DoNTP is unexported (espradio
// v0.3.0, netlink/netlink.go). Without a client here the clock stays at the
// epoch and every OTLP timestamp is wrong.
//
// The packet handling lives in this package rather than in the firmware so
// that it can be tested on a host. The first version of this code was written
// behind a tinygo build tag, and a comment claiming "version 4" sat above a
// byte that meant version 3 for as long as nothing could compile it.
package sntp

import "errors"

// PacketSize is the size of an SNTP header (RFC 4330 section 4).
const PacketSize = 48

// Offsets into the packet.
const (
	offsetLIVNMode     = 0
	offsetStratum      = 1
	offsetTransmitSec  = 40
	offsetTransmitFrac = 44
)

// ClientV4 is the first byte of a client request: LI=0, VN=4, Mode=3.
//
// It is built from its fields rather than written as a literal. The obvious
// literal 0x1B is version *3*; writing it and calling it v4 in a comment is
// exactly the mistake this constant prevents.
const ClientV4 = byte(0<<6 | 4<<3 | 3)

// Modes that a client may receive (RFC 4330 section 4).
const (
	modeServer    = 4
	modeBroadcast = 5
)

// epochOffset is the gap between the NTP epoch (1900-01-01) and the Unix
// epoch (1970-01-01), in seconds.
const epochOffset = 2208988800

// era1Offset spans one wrap of the 32-bit seconds field.
//
// RFC 4330 section 3 resolves the ambiguity by the high bit: set means era 0
// (1968-2036), clear means era 1 (2036-2104). Treating every value as era 0
// makes a timestamp from 2036 decode as 1900.
const era1Offset = int64(1) << 32

// Errors returned by Parse.
var (
	ErrShortReply   = errors.New("sntp: reply shorter than 48 bytes")
	ErrZeroStamp    = errors.New("sntp: server returned a zero transmit timestamp")
	ErrKissOfDeath  = errors.New("sntp: server returned a kiss-of-death (stratum 0)")
	ErrNotServer    = errors.New("sntp: reply is not from a server")
	ErrWrongVersion = errors.New("sntp: reply version is not 3 or 4")
	// ErrUnsynchronized is returned for LI=3 ("alarm condition"), which
	// RFC 4330 section 5 says a client must discard.
	ErrUnsynchronized = errors.New("sntp: server clock is not synchronized (LI=3)")
)

// Request returns a client request packet.
func Request() []byte {
	p := make([]byte, PacketSize)
	p[offsetLIVNMode] = ClientV4
	return p
}

// Parse validates a reply and returns its transmit timestamp as Unix seconds
// and nanoseconds.
//
// Validation matters because an unsolicited or malformed packet arriving on
// the socket would otherwise set the clock, and every exported timestamp
// depends on it.
func Parse(p []byte) (unixSec int64, nsec int64, err error) {
	if len(p) < PacketSize {
		return 0, 0, ErrShortReply
	}

	mode := p[offsetLIVNMode] & 0x7
	if mode != modeServer && mode != modeBroadcast {
		return 0, 0, ErrNotServer
	}
	// Accept 3 and 4: a v3 server may answer a v4 request, and the timestamp
	// format is identical.
	if v := (p[offsetLIVNMode] >> 3) & 0x7; v != 3 && v != 4 {
		return 0, 0, ErrWrongVersion
	}
	if p[offsetLIVNMode]>>6 == 3 {
		return 0, 0, ErrUnsynchronized
	}
	// Stratum 0 is a kiss-of-death: the packet carries an error code in the
	// reference identifier, not a usable time.
	if p[offsetStratum] == 0 {
		return 0, 0, ErrKissOfDeath
	}

	secs := uint32(p[offsetTransmitSec])<<24 |
		uint32(p[offsetTransmitSec+1])<<16 |
		uint32(p[offsetTransmitSec+2])<<8 |
		uint32(p[offsetTransmitSec+3])
	frac := uint32(p[offsetTransmitFrac])<<24 |
		uint32(p[offsetTransmitFrac+1])<<16 |
		uint32(p[offsetTransmitFrac+2])<<8 |
		uint32(p[offsetTransmitFrac+3])
	if secs == 0 && frac == 0 {
		return 0, 0, ErrZeroStamp
	}

	unixSec, nsec = toUnix(secs, frac)
	return unixSec, nsec, nil
}

// toUnix converts an era-aware NTP timestamp to Unix time.
func toUnix(secs, frac uint32) (int64, int64) {
	s := int64(secs) - epochOffset
	if secs&0x80000000 == 0 {
		// Era 1: 2036-02-07 onwards.
		s = int64(secs) + era1Offset - epochOffset
	}
	// The fraction is in units of 2^-32 seconds.
	return s, int64(frac) * 1e9 >> 32
}
