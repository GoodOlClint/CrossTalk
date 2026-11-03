package appserver

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// RFC 6455 opcodes.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// MaxMessageSize bounds one reassembled websocket message.
const MaxMessageSize = 64 << 20

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var (
	ErrHandshake   = errors.New("websocket handshake failed")
	ErrFrameTooBig = errors.New("websocket message exceeds limit")
	ErrClosed      = errors.New("websocket closed by peer")
)

// AcceptKey is the Sec-WebSocket-Accept value for a Sec-WebSocket-Key.
func AcceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// ClientHandshake performs the client upgrade on conn and returns a reader
// positioned after the response headers.
func ClientHandshake(conn net.Conn, host, path string) (*bufio.Reader, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshake, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("%w: status %s", ErrHandshake, resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return nil, fmt.Errorf("%w: missing Upgrade: websocket", ErrHandshake)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != AcceptKey(key) {
		return nil, fmt.Errorf("%w: bad Sec-WebSocket-Accept %q", ErrHandshake, got)
	}
	return br, nil
}

// WriteFrame writes one final frame. Clients must mask (RFC 6455 §5.3);
// servers must not.
func WriteFrame(w io.Writer, op byte, payload []byte, mask bool) error {
	n := len(payload)
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|op)
	var mbit byte
	if mask {
		mbit = 0x80
	}
	switch {
	case n < 126:
		hdr = append(hdr, mbit|byte(n))
	case n < 1<<16:
		hdr = append(hdr, mbit|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, mbit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := payload
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		hdr = append(hdr, key[:]...)
		body = make([]byte, n)
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}
	buf := append(hdr, body...)
	_, err := w.Write(buf)
	return err
}

type frame struct {
	fin     bool
	op      byte
	payload []byte
}

func readFrame(r io.Reader, limit int) (frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return frame{}, err
	}
	f := frame{fin: h[0]&0x80 != 0, op: h[0] & 0x0F}
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return frame{}, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > uint64(limit) {
		return frame{}, ErrFrameTooBig
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return frame{}, err
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return frame{}, err
	}
	if masked {
		for i := range f.payload {
			f.payload[i] ^= key[i%4]
		}
	}
	return f, nil
}

// ReadMessage returns the next data message, reassembling fragments.
// control is called for ping/pong frames interleaved with data; a close
// frame returns ErrClosed.
func ReadMessage(r io.Reader, control func(op byte, payload []byte) error) (byte, []byte, error) {
	var (
		op  byte
		buf []byte
		mid bool
	)
	for {
		f, err := readFrame(r, MaxMessageSize)
		if err != nil {
			return 0, nil, err
		}
		switch f.op {
		case OpClose:
			return 0, nil, ErrClosed
		case OpPing, OpPong:
			if control != nil {
				if err := control(f.op, f.payload); err != nil {
					return 0, nil, err
				}
			}
			continue
		case OpContinuation:
			if !mid {
				return 0, nil, errors.New("websocket: continuation without start")
			}
		case OpText, OpBinary:
			if mid {
				return 0, nil, errors.New("websocket: new message inside fragmented message")
			}
			op, mid = f.op, true
		default:
			return 0, nil, fmt.Errorf("websocket: unknown opcode %#x", f.op)
		}
		if len(buf)+len(f.payload) > MaxMessageSize {
			return 0, nil, ErrFrameTooBig
		}
		buf = append(buf, f.payload...)
		if f.fin {
			return op, buf, nil
		}
	}
}
