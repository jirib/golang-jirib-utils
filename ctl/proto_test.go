package ctl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("{}"), []byte("hello"), bytes.Repeat([]byte("x"), 4096)} {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, testCmd, payload); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
		typ, got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if typ != testCmd {
			t.Errorf("type = %v, want request", typ)
		}
		if !bytes.Equal(got, payload) && !(len(got) == 0 && len(payload) == 0) {
			t.Errorf("payload = %d bytes, want %d", len(got), len(payload))
		}
	}
}

// The header must be byte-exact: the length is big-endian at offset 4 and the
// reserved field is zero, because that layout is the documented wire contract
// (and rtrd's). A refactor that switched to native-endian would keep every
// Go<->Go test passing while breaking any other implementation.
func TestHeaderLayoutIsWireStable(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, TypeReply, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	want := []byte{Version, byte(TypeReply), 0, 0, 0, 0, 0, 4, 'a', 'b', 'c', 'd'}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("frame = %#v, want %#v", buf.Bytes(), want)
	}
	if n := binary.BigEndian.Uint32(buf.Bytes()[4:8]); n != 4 {
		t.Errorf("length field = %d, want 4", n)
	}
}

// rtrd validates the announced length before reading the body ("packet too
// small" / "packet too large"). So must we: an oversized length that we
// honoured would be an allocation sized by an unauthenticated peer.
func TestReadHeaderRejectsOutOfRangeLength(t *testing.T) {
	build := func(length uint32) []byte {
		var buf bytes.Buffer
		hdr := []byte{Version, byte(testCmd), 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(hdr[4:8], length)
		buf.Write(hdr)
		return buf.Bytes()
	}

	if _, _, err := ReadMessage(bytes.NewReader(build(1 << 30))); !errors.Is(err, ErrProtocol) {
		t.Errorf("oversized length: err = %v, want ErrProtocol", err)
	}
	// A length is a payload length, not a total, so it may legitimately be 0.
	if typ, n, err := ReadHeader(bytes.NewReader(build(0))); err != nil || typ != testCmd || n != 0 {
		t.Errorf("zero-length payload: got (%v, %d, %v), want (request, 0, nil)", typ, n, err)
	}
}

func TestReadHeaderRejectsBadVersionAndReserved(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		hdr := []byte{Version + 1, byte(testCmd), 0, 0, 0, 0, 0, 0}
		if _, _, err := ReadHeader(bytes.NewReader(hdr)); !errors.Is(err, ErrProtocol) {
			t.Errorf("err = %v, want ErrProtocol", err)
		} else if !strings.Contains(err.Error(), "unsupported version") {
			t.Errorf("error should name the version problem, got %v", err)
		}
	})
	t.Run("reserved", func(t *testing.T) {
		hdr := []byte{Version, byte(testCmd), 0, 1, 0, 0, 0, 0}
		if _, _, err := ReadHeader(bytes.NewReader(hdr)); !errors.Is(err, ErrProtocol) {
			t.Errorf("err = %v, want ErrProtocol", err)
		} else if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("error should name the reserved field, got %v", err)
		}
	})
}

// WriteHeader must refuse to emit a frame it could not then read back, so a
// caller cannot put an unencodable length on the wire. The type itself is the
// application's to choose — ctl validates only that it is not zero, since the
// daemon owns its own command enum.
func TestWriteHeaderRefusesInvalidFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, 0, nil); err == nil {
		t.Error("wrote a type-0 frame")
	}
	if err := WriteMessage(&buf, testCmd, make([]byte, MaxPayload+1)); err == nil {
		t.Error("oversized payload was written")
	}
}

// A truncated body must not be reported as a short but valid frame.
func TestReadMessageRejectsTruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, testCmd, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	truncated := buf.Bytes()[:HeaderSize+4]
	if _, _, err := ReadMessage(bytes.NewReader(truncated)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestTypeStrings(t *testing.T) {
	for typ, want := range map[Type]string{
		TypeReply: "reply",
		TypeError: "error",
		Type(42):  "type-42",
	} {
		if got := typ.String(); got != want {
			t.Errorf("Type(%d).String() = %q, want %q", uint8(typ), got, want)
		}
	}
	// Only the two response types are named here; a command's type is the
	// daemon's own enum and must render as a number it can look up.
	for _, typ := range []Type{TypeReply, TypeError} {
		if !typ.IsResponse() {
			t.Errorf("Type(%d).IsResponse() = false", uint8(typ))
		}
	}
	if testCmd.IsResponse() {
		t.Error("a command type must not be a response type")
	}
}

// A response type must never be written where a request belongs, and type 0 is
// never writable at all — both would put a frame on the wire the peer cannot
// interpret.
func TestWriteRefusesTypeZero(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, 0, nil); err == nil {
		t.Error("wrote a type-0 frame")
	}
}

// testCmd stands in for an application-defined command type in the framing
// tests. ctl has no request type of its own — see Type's doc comment.
const testCmd Type = 7
