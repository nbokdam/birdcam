// Package ingest implements the server side of the media channel: TCP
// listeners on the ingest port pool, preamble validation and RFC 4571 framed
// RTP reading.
package ingest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxPreambleLen is the maximum accepted preamble line length (incl. '\n').
const MaxPreambleLen = 256

// MaxFrameSize is the largest RTP packet forwarded. Bigger frames are read
// and dropped (the agent payloads with mtu=1200, see protocol).
const MaxFrameSize = 1460

var (
	ErrPreambleTooLong = errors.New("ingest: preamble too long")
	ErrBadPreamble     = errors.New("ingest: malformed preamble")
	ErrFrameTooLarge   = errors.New("ingest: frame too large")
)

// ReadPreamble reads the first line ("BIRDCAM1 <session_id> <token>\n") and
// returns session id and token. It never reads past the newline.
func ReadPreamble(r *bufio.Reader) (sessionID, token string, err error) {
	var sb strings.Builder
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", "", err
		}
		sb.WriteByte(b)
		if b == '\n' {
			break
		}
		if sb.Len() >= MaxPreambleLen {
			return "", "", ErrPreambleTooLong
		}
	}
	return ParsePreamble(sb.String())
}

// ParsePreamble parses a complete preamble line including the trailing '\n'.
func ParsePreamble(line string) (sessionID, token string, err error) {
	if !strings.HasSuffix(line, "\n") {
		return "", "", ErrBadPreamble
	}
	parts := strings.Split(strings.TrimSuffix(line, "\n"), " ")
	if len(parts) != 3 || parts[0] != "BIRDCAM1" || parts[1] == "" || parts[2] == "" {
		return "", "", ErrBadPreamble
	}
	return parts[1], parts[2], nil
}

// ReadFrame reads one RFC 4571 frame (2-byte big-endian length + payload)
// into buf and returns the payload. Zero-length frames return an empty slice.
// If the frame is larger than len(buf), it is discarded and ErrFrameTooLarge
// is returned; the stream stays in sync so the caller may continue.
func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > len(buf) {
		if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
			return nil, unexpected(err)
		}
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, n)
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return nil, unexpected(err)
	}
	return buf[:n], nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// AppendFrame appends an RFC 4571 frame for pkt to dst.
func AppendFrame(dst, pkt []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(pkt)))
	return append(dst, pkt...)
}
