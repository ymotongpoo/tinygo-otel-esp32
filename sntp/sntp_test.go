package sntp

import (
	"errors"
	"testing"
	"time"
)

// The literal 0x1B is version 3. This test is the reason the constant is built
// from its fields instead of written as a literal.
func TestClientV4IsActuallyVersion4(t *testing.T) {
	if v := (ClientV4 >> 3) & 0x7; v != 4 {
		t.Errorf("ClientV4 version = %d, want 4", v)
	}
	if m := ClientV4 & 0x7; m != 3 {
		t.Errorf("ClientV4 mode = %d, want 3 (client)", m)
	}
	if li := (ClientV4 >> 6) & 0x3; li != 0 {
		t.Errorf("ClientV4 leap indicator = %d, want 0", li)
	}
	if ClientV4 == 0x1B {
		t.Error("ClientV4 is 0x1B, which is version 3, not 4")
	}
}

func TestRequestIsWellFormed(t *testing.T) {
	p := Request()
	if len(p) != PacketSize {
		t.Fatalf("len = %d, want %d", len(p), PacketSize)
	}
	if p[0] != ClientV4 {
		t.Errorf("first byte = %#x, want %#x", p[0], ClientV4)
	}
	for i, b := range p[1:] {
		if b != 0 {
			t.Errorf("byte %d = %#x, want 0", i+1, b)
		}
	}
}

// reply builds a server reply carrying the given era-0 seconds.
func reply(secs uint32, frac uint32) []byte {
	p := make([]byte, PacketSize)
	p[offsetLIVNMode] = 0<<6 | 4<<3 | modeServer
	p[offsetStratum] = 2
	p[offsetTransmitSec] = byte(secs >> 24)
	p[offsetTransmitSec+1] = byte(secs >> 16)
	p[offsetTransmitSec+2] = byte(secs >> 8)
	p[offsetTransmitSec+3] = byte(secs)
	p[offsetTransmitFrac] = byte(frac >> 24)
	p[offsetTransmitFrac+1] = byte(frac >> 16)
	p[offsetTransmitFrac+2] = byte(frac >> 8)
	p[offsetTransmitFrac+3] = byte(frac)
	return p
}

func TestParseKnownTimestamp(t *testing.T) {
	// 2026-09-15 00:00:00 UTC.
	want := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	secs := uint32(want.Unix() + epochOffset)

	sec, nsec, err := Parse(reply(secs, 0))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := time.Unix(sec, nsec).UTC()
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// The 32-bit seconds field wraps on 2036-02-07. Treating every value as era 0
// makes those timestamps decode as 1900, which would silently break the clock
// on a device still running then.
func TestParseSurvivesEra1Rollover(t *testing.T) {
	// One hour after the rollover: era-0 seconds wrap to a small value with
	// the high bit clear.
	want := time.Date(2036, 2, 7, 7, 28, 16, 0, time.UTC)
	secs := uint32(uint64(want.Unix()+epochOffset) & 0xFFFFFFFF)
	if secs&0x80000000 != 0 {
		t.Fatalf("test setup: expected the high bit clear, got %#x", secs)
	}

	sec, _, err := Parse(reply(secs, 0))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := time.Unix(sec, 0).UTC()
	if got.Year() == 1900 {
		t.Fatalf("era 1 timestamp decoded as %s; the era bit was ignored", got)
	}
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestParseFraction(t *testing.T) {
	// 0x80000000 is exactly half a second.
	_, nsec, err := Parse(reply(uint32(epochOffset), 0x80000000))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if nsec != 500_000_000 {
		t.Errorf("nsec = %d, want 500000000", nsec)
	}
}

// An unsolicited or broken packet must not be allowed to set the clock.
func TestParseRejectsBadReplies(t *testing.T) {
	valid := reply(uint32(epochOffset+1_800_000_000), 0)

	cases := []struct {
		name   string
		mutate func([]byte) []byte
		want   error
	}{
		{"short", func(p []byte) []byte { return p[:47] }, ErrShortReply},
		{"zero timestamp", func(p []byte) []byte {
			q := append([]byte(nil), p...)
			for i := offsetTransmitSec; i < offsetTransmitSec+8; i++ {
				q[i] = 0
			}
			return q
		}, ErrZeroStamp},
		{"kiss of death", func(p []byte) []byte {
			q := append([]byte(nil), p...)
			q[offsetStratum] = 0
			return q
		}, ErrKissOfDeath},
		{"client mode echoed back", func(p []byte) []byte {
			q := append([]byte(nil), p...)
			q[offsetLIVNMode] = 0<<6 | 4<<3 | 3
			return q
		}, ErrNotServer},
		{"version 1", func(p []byte) []byte {
			q := append([]byte(nil), p...)
			q[offsetLIVNMode] = 0<<6 | 1<<3 | modeServer
			return q
		}, ErrWrongVersion},
		// RFC 4330 section 5: LI=3 means the server's clock is not
		// synchronized, so its timestamp must not be used.
		{"unsynchronized server", func(p []byte) []byte {
			q := append([]byte(nil), p...)
			q[offsetLIVNMode] = 3<<6 | 4<<3 | modeServer
			return q
		}, ErrUnsynchronized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse(tc.mutate(valid))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A v3 server may answer a v4 request; the timestamp format is identical, so
// rejecting it would lose a working time source.
func TestParseAcceptsVersion3Server(t *testing.T) {
	p := reply(uint32(epochOffset+1_800_000_000), 0)
	p[offsetLIVNMode] = 0<<6 | 3<<3 | modeServer

	if _, _, err := Parse(p); err != nil {
		t.Errorf("a v3 server reply was rejected: %v", err)
	}
}

// LI values 1 and 2 only announce a leap second; the time is still valid.
func TestParseAcceptsLeapWarning(t *testing.T) {
	for _, li := range []byte{1, 2} {
		p := reply(uint32(epochOffset+1_800_000_000), 0)
		p[offsetLIVNMode] = li<<6 | 4<<3 | modeServer
		if _, _, err := Parse(p); err != nil {
			t.Errorf("LI=%d: err = %v, want nil", li, err)
		}
	}
}
