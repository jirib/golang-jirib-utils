// Package ctl provides the transport for daemon control sockets over Unix domain sockets.
// Frames consist of a fixed 8-byte header followed by an optional payload.
package ctl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the wire protocol version.
const Version uint8 = 1

// HeaderSize is the frame header size: version (uint8), type (uint8), reserved (uint16=0), length (uint32 big-endian).
const HeaderSize = 8

// MaxPayload bounds frame payload allocations (8 MiB).
const MaxPayload = 8 << 20

// Type identifies a message type. Application command types use values < 128.
type Type uint8

const (
	// TypeReply is a successful response frame.
	TypeReply Type = 129
	// TypeError is an error response frame.
	TypeError Type = 130
)

func (t Type) String() string {
	switch t {
	case TypeReply:
		return "reply"
	case TypeError:
		return "error"
	default:
		return fmt.Sprintf("type-%d", uint8(t))
	}
}

// IsResponse reports whether t is a response frame (TypeReply or TypeError).
func (t Type) IsResponse() bool { return t == TypeReply || t == TypeError }

// ErrProtocol marks a framing or wire protocol violation.
var ErrProtocol = errors.New("ctl: protocol violation")

// ReadHeader reads and validates an 8-byte frame header from r.
func ReadHeader(r io.Reader) (Type, int, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, err
	}
	if hdr[0] != Version {
		return 0, 0, fmt.Errorf("%w: unsupported version %d (this build speaks %d)", ErrProtocol, hdr[0], Version)
	}
	if hdr[2] != 0 || hdr[3] != 0 {
		return 0, 0, fmt.Errorf("%w: reserved header bytes must be zero, got %#x", ErrProtocol, hdr[2:4])
	}
	t := Type(hdr[1])
	n := int(binary.BigEndian.Uint32(hdr[4:8]))
	if n < 0 || n > MaxPayload {
		return 0, 0, fmt.Errorf("%w: payload length %d out of range [0, %d]", ErrProtocol, n, MaxPayload)
	}
	return t, n, nil
}

// WriteHeader writes a frame header for payloadLen bytes.
func WriteHeader(w io.Writer, t Type, payloadLen int) error {
	if payloadLen < 0 || payloadLen > MaxPayload {
		return fmt.Errorf("%w: payload length %d out of range [0, %d]", ErrProtocol, payloadLen, MaxPayload)
	}
	if t == 0 {
		return fmt.Errorf("%w: refusing to write message type 0", ErrProtocol)
	}
	var hdr [HeaderSize]byte
	hdr[0] = Version
	hdr[1] = byte(t)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(payloadLen))
	_, err := w.Write(hdr[:])
	return err
}

// WriteMessage writes a complete frame (header and payload).
func WriteMessage(w io.Writer, t Type, payload []byte) error {
	if err := WriteHeader(w, t, len(payload)); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadMessage reads a complete frame (header and payload).
func ReadMessage(r io.Reader) (Type, []byte, error) {
	t, n, err := ReadHeader(r)
	if err != nil {
		return 0, nil, err
	}
	if n == 0 {
		return t, nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, fmt.Errorf("reading %d-byte payload: %w", n, err)
	}
	return t, buf, nil
}
