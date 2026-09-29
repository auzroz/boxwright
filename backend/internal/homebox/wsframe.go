// RFC 6455 framing over a byte stream, and nothing else: no dial, no HTTP, no
// Homebox. It is its own file because a length-parsing bug does not announce
// itself -- the frame boundary slides by a few bytes, every frame after it is
// read out of the middle of a payload, and the symptom in production is a
// watcher that quietly stops delivering events. Here that same bug is a table
// test over a byte slice.
//
// This is the subset a read-only SUBSCRIBER needs. Boxwright opens one socket
// to Homebox's /v1/ws/events, advertises no extension, negotiates no
// subprotocol, and never sends an application message -- the only frames it
// writes are the mandatory pong and a close. So what is missing is missing on
// purpose:
//
//   - outbound fragmentation and the writer state machine that goes with it:
//     every frame we send is complete in itself.
//   - outbound UTF-8 validation: we send no text.
//   - the inbound unmasking path: a server MUST NOT mask, so a masked inbound
//     frame is a broken peer rather than a case to handle.
//   - reassembly of a fragmented message: readFrame hands back each frame as it
//     arrives and the stream layer decides what to do with the parts. Homebox
//     has never fragmented one of these payloads -- they are 16 and 27 bytes --
//     but a frame that arrives has to be parsed rather than assumed away.
//
// The tripwire: the day Boxwright needs to SEND application messages, or to
// negotiate an extension such as permessage-deflate, this stops being a small
// honest subset and becomes half a WebSocket implementation maintained by
// people who are not maintaining a WebSocket implementation. That is the
// concrete moment to take a real dependency, and until then there is no reason
// to.

package homebox

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Opcodes (RFC 6455 section 5.2). Homebox sends opText for both the 10-second
// {"event":"ping"} keepalive and every mutation event; opPing is the separate
// CONTROL frame, and answering that one is what actually holds the connection
// open (measured: ignore it and the peer hangs up ~6 seconds later).
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxFramePayload caps an inbound payload. The two this socket has ever carried
// are 16 bytes ({"event":"ping"}) and 27 ({"event":"entity.mutation"}), so 4 KiB
// leaves about 150x headroom for an event Homebox has not invented yet.
//
// The cap exists because the length field is 64 bits wide and a header claiming
// 16 EiB costs the sender ten bytes: without it, a peer that is hostile or
// simply confused decides how much memory we allocate. Exceeding it is a
// protocol FAILURE, never a frame to skip -- a payload we refuse to read is a
// payload we cannot step over, so the only coherent response is to fail the
// connection and dial again.
const maxFramePayload = 4096

// errWSProtocol marks a peer that broke RFC 6455. It is kept distinct from a
// truncated read on purpose: a protocol error means reconnecting to this server
// will go wrong the same way and the bytes on the wire are worth logging, while
// io.EOF just means the connection ended and the watcher should redial.
var errWSProtocol = errors.New("websocket protocol error")

// frame is one WebSocket frame, de-framed but NOT reassembled: a fragmented
// message arrives as opText or opBinary with fin false, followed by
// opContinuation frames. Joining them is the caller's job, because the caller
// is what knows how large a message it is willing to hold.
type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// readFrame reads exactly one frame from br, refusing any payload longer than
// max bytes.
//
// The order below is the most important thing in this file. The length is
// parsed, and the payload consumed, BEFORE the frame is judged on anything
// else. A frame rejected without consuming its exact payload leaves the reader
// parked inside it, so the next "header" is whatever two bytes happened to land
// there and every frame after that is nonsense -- with no error at the moment
// it goes wrong, just a watcher that stops reporting mutations. Failing loudly
// one frame later, with the stream still sitting on a frame boundary, is what
// makes that debuggable.
//
// The errors are meant to be told apart: errors.Is(err, errWSProtocol) is a
// peer that is wrong, errors.Is(err, io.EOF) is a connection that ended
// cleanly on a frame boundary, and errors.Is(err, io.ErrUnexpectedEOF) is one
// that ended mid-frame. Use errors.Is, not ==; these are wrapped.
func readFrame(br *bufio.Reader, max int) (frame, error) {
	var hdr [2]byte
	// The only read in this function where io.EOF means "the connection ended"
	// rather than "the connection was cut in half"; see readBody.
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return frame{}, fmt.Errorf("websocket read frame header: %w", err)
	}
	fin := hdr[0]&0x80 != 0
	rsv := hdr[0] & 0x70
	opcode := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0

	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if err := readBody(br, ext[:]); err != nil {
			return frame{}, fmt.Errorf("websocket read 16-bit length: %w", err)
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if err := readBody(br, ext[:]); err != nil {
			return frame{}, fmt.Errorf("websocket read 64-bit length: %w", err)
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	// RFC 6455 also tells a SENDER to use the shortest length form that fits.
	// We do not enforce that on the way in: a non-minimal length is
	// unambiguous, so honouring it costs nothing, while rejecting it would be
	// one more way to drop an event from a server that is otherwise talking to
	// us perfectly. The cap below, not the encoding, is what bounds what we
	// allocate.
	if n > uint64(max) {
		// The one rejection that deliberately does NOT consume the payload,
		// because consuming it is exactly the allocation being refused. That is
		// safe only because this fails the connection: nothing reads from this
		// stream again.
		return frame{}, fmt.Errorf("websocket frame payload %d exceeds %d: %w", n, max, errWSProtocol)
	}

	// A server must never mask, but a masked frame still carries its 4-byte key
	// ahead of the payload, and skipping it would leave the reader four bytes
	// short of the next header. Consume it here, reject it below.
	if masked {
		var key [4]byte
		if err := readBody(br, key[:]); err != nil {
			return frame{}, fmt.Errorf("websocket read mask key: %w", err)
		}
	}
	payload := make([]byte, n)
	if err := readBody(br, payload); err != nil {
		return frame{}, fmt.Errorf("websocket read %d-byte payload: %w", n, err)
	}

	// Everything from here on is a judgement on a frame that has already been
	// consumed in full.
	if masked {
		return frame{}, fmt.Errorf("websocket server masked a frame: %w", errWSProtocol)
	}
	if rsv != 0 {
		// We advertise no extension in the handshake and Subscribe refuses a
		// 101 that names one anyway, so permessage-deflate -- the only thing
		// that legitimately sets RSV1 -- cannot have been negotiated. A
		// reserved bit set here means the payload is not what the opcode says
		// it is.
		return frame{}, fmt.Errorf("websocket reserved bit set (rsv %#02x): %w", rsv, errWSProtocol)
	}
	switch opcode {
	case opContinuation, opText, opBinary:
	case opClose, opPing, opPong:
		// RFC 6455 section 5.5: a control frame is never fragmented and never
		// exceeds 125 bytes, so that a peer can always answer one without
		// buffering. A control frame breaking either rule is not a large ping,
		// it is a peer we have misparsed or one that is lying.
		if !fin {
			return frame{}, fmt.Errorf("websocket fragmented control frame (opcode %#x): %w", opcode, errWSProtocol)
		}
		if n > 125 {
			return frame{}, fmt.Errorf("websocket control frame payload %d exceeds 125 (opcode %#x): %w", n, opcode, errWSProtocol)
		}
	default:
		// Section 5.2 again: an unrecognised opcode MUST fail the connection.
		// Guessing at one is how a subscriber starts inventing events.
		return frame{}, fmt.Errorf("websocket reserved opcode %#x: %w", opcode, errWSProtocol)
	}
	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

// readBody fills buf from INSIDE a frame, where a plain io.EOF is not the good
// kind. io.ReadFull reports io.EOF only when it read nothing at all, so a peer
// that disappears exactly on the boundary between the header and the payload
// it promised returns the same error as one that closed politely between
// frames -- and the watcher would log a tidy reconnect for a connection that
// was actually cut in half. Only the two header bytes may end a connection
// cleanly; everything after them is io.ErrUnexpectedEOF.
func readBody(br *bufio.Reader, buf []byte) error {
	if _, err := io.ReadFull(br, buf); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

// writeMasked writes one complete frame (fin set) with a fresh mask key.
//
// EVERY client-to-server frame is masked, control frames included: RFC 6455
// section 5.1 requires a SERVER to fail the connection on an unmasked client
// frame, so the pong that keeps this socket alive and the close that ends it
// are masked like anything else. The key is four fresh crypto/rand bytes per
// frame -- masking exists so that a client cannot be talked into emitting
// attacker-chosen bytes at a proxy or cache, and a reused or predictable key
// hands that property straight back.
//
// Single frame only. We never send an application message (see the file
// comment), so there is no continuation to emit and no writer state to keep.
// The frame is assembled whole and handed to w in ONE Write, so it reaches the
// wire in one piece instead of as a header some other goroutine's frame could
// land inside; ordering between concurrent callers is still theirs to arrange.
func writeMasked(w io.Writer, opcode byte, payload []byte) error {
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return fmt.Errorf("websocket mask key: %w", err)
	}

	buf := make([]byte, 0, len(payload)+14)
	buf = append(buf, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		buf = append(buf, 0x80|byte(n))
	case n <= 0xffff:
		buf = append(buf, 0x80|126)
		buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	default:
		// Unreachable for the frames Boxwright sends; here so the encoder is
		// not silently wrong on the day something does send a long one.
		buf = append(buf, 0x80|127)
		buf = binary.BigEndian.AppendUint64(buf, uint64(n))
	}
	buf = append(buf, key[:]...)
	// Mask into the outgoing buffer rather than in place: a pong echoes the
	// ping payload, which still belongs to the reader.
	for i, b := range payload {
		buf = append(buf, b^key[i%4])
	}

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("websocket write frame (opcode %#x, %d bytes): %w", opcode, len(payload), err)
	}
	return nil
}

// wsGUID is the fixed string RFC 6455 section 1.3 appends to the client key.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// acceptKey computes the Sec-WebSocket-Accept value a server owes for a given
// Sec-WebSocket-Key. Checking it is how a client establishes that it reached a
// WebSocket endpoint rather than a proxy or a cache replaying somebody's old
// 101, before it starts reading bytes as frames.
//
// SHA-1 here is not a security choice and is not ours to revisit: the handshake
// is defined in terms of this hash and this GUID, and it proves liveness, not
// secrecy. The credential travels in the Authorization header.
func acceptKey(clientKey string) string {
	sum := sha1.Sum([]byte(clientKey + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}
