package b2

import (
	"crypto/sha1"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestSHA1TrailerReader(t *testing.T) {
	input := []byte("the quick brown fox jumps over the lazy dog")
	wantSum := sha1.Sum(input)
	wantHex := hex.EncodeToString(wantSum[:])

	r := newSHA1TrailerReader(strings.NewReader(string(input)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if len(out) != len(input)+40 {
		t.Fatalf("output length = %d, want %d", len(out), len(input)+40)
	}
	if string(out[:len(input)]) != string(input) {
		t.Fatalf("output prefix mismatch: got %q, want %q", out[:len(input)], input)
	}
	gotTrailer := string(out[len(input):])
	if gotTrailer != wantHex {
		t.Fatalf("trailer = %q, want %q", gotTrailer, wantHex)
	}

	// A further Read after EOF must keep returning io.EOF.
	n, err := r.Read(make([]byte, 8))
	if n != 0 || err != io.EOF {
		t.Fatalf("Read() after EOF = (%d, %v), want (0, io.EOF)", n, err)
	}
}

func TestSHA1TrailerReaderEmptyInput(t *testing.T) {
	wantSum := sha1.Sum(nil)
	wantHex := hex.EncodeToString(wantSum[:])

	r := newSHA1TrailerReader(strings.NewReader(""))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if len(out) != 40 {
		t.Fatalf("output length = %d, want 40", len(out))
	}
	if string(out) != wantHex {
		t.Fatalf("trailer = %q, want %q", out, wantHex)
	}
}

func TestSHA1TrailerReaderSmallReadBuffer(t *testing.T) {
	// Exercise the state machine with a read buffer smaller than both the
	// input and the trailer, to make sure partial reads across the
	// data/trailer boundary are handled correctly.
	input := []byte("0123456789")
	wantSum := sha1.Sum(input)
	wantHex := hex.EncodeToString(wantSum[:])

	r := newSHA1TrailerReader(strings.NewReader(string(input)))
	var out []byte
	buf := make([]byte, 3)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
	}

	if len(out) != len(input)+40 {
		t.Fatalf("output length = %d, want %d", len(out), len(input)+40)
	}
	if string(out[len(input):]) != wantHex {
		t.Fatalf("trailer = %q, want %q", out[len(input):], wantHex)
	}
}
