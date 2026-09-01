// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sambox

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// HTTP capsules (RFC 9297) frame datagrams on a reliable stream, which is the
// form connect-udp (RFC 9298) takes when the transport is not QUIC — here, a
// Unix socket or vsock. This codec is deliberately standalone: the guest side
// lives in the tun2connect library, but importing that library would pull a
// userspace TCP stack into the module every SAM binary builds from, and the
// boundary needs sixty lines of framing, not a netstack.

// capsuleTypeDatagram carries one HTTP Datagram, whose payload for connect-udp
// is a context ID (0) plus the UDP payload (RFC 9298 section 5).
const capsuleTypeDatagram = 0x00

// maxCapsulePayload bounds a peer's declared capsule length so a sandbox
// cannot make the boundary allocate unbounded memory.
const maxCapsulePayload = 1 << 16

// varints are QUIC variable-length integers (RFC 9000 section 16).

func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return binary.BigEndian.AppendUint16(b, uint16(v)|0x4000)
	case v < 1<<30:
		return binary.BigEndian.AppendUint32(b, uint32(v)|0x8000_0000)
	case v < 1<<62:
		return binary.BigEndian.AppendUint64(b, v|0xc000_0000_0000_0000)
	default:
		panic("varint overflow")
	}
}

func readVarint(r io.ByteReader) (uint64, error) {
	b0, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	v := uint64(b0 & 0x3f)
	for i := 1; i < 1<<(b0>>6); i++ {
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		v = v<<8 | uint64(b)
	}
	return v, nil
}

// CapsuleStream frames HTTP Datagrams on a reliable stream. It is exported so
// tests and non-tun2connect clients can speak the boundary's UDP form.
type CapsuleStream struct {
	wmu sync.Mutex
	w   io.Writer
	r   *bufio.Reader
}

func NewCapsuleStream(rw io.ReadWriter) *CapsuleStream {
	return &CapsuleStream{w: rw, r: bufio.NewReader(rw)}
}

// WriteDatagram sends one UDP payload as a DATAGRAM capsule with context ID 0.
// Safe for concurrent writers.
func (s *CapsuleStream) WriteDatagram(p []byte) error {
	buf := make([]byte, 0, len(p)+8)
	buf = appendVarint(buf, capsuleTypeDatagram)
	buf = appendVarint(buf, uint64(len(p))+1) // +1: the context ID below
	buf = appendVarint(buf, 0)
	buf = append(buf, p...)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.w.Write(buf)
	return err
}

// ReadDatagram returns the next UDP payload, skipping capsule types and
// datagram contexts it does not understand, as RFC 9297 requires.
func (s *CapsuleStream) ReadDatagram() ([]byte, error) {
	for {
		ctype, err := readVarint(s.r)
		if err != nil {
			return nil, err
		}
		clen, err := readVarint(s.r)
		if err != nil {
			return nil, err
		}
		if clen > maxCapsulePayload {
			return nil, fmt.Errorf("sambox: capsule of %d bytes exceeds limit", clen)
		}
		value := make([]byte, clen)
		if _, err := io.ReadFull(s.r, value); err != nil {
			return nil, err
		}
		if ctype != capsuleTypeDatagram {
			continue
		}
		rd := bytes.NewReader(value)
		ctxID, err := readVarint(rd)
		if err != nil {
			return nil, errors.New("sambox: malformed DATAGRAM capsule")
		}
		if ctxID != 0 {
			continue
		}
		return value[len(value)-rd.Len():], nil
	}
}
