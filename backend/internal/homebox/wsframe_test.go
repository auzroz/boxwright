package homebox

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// The two payloads this socket has actually carried, captured from a live
// v0.26.2 on 2026-09-10. The mutation event is the whole event: no id, no type,
// no operation. The length bytes hard-coded in the tables below (0x10 and 0x1b)
// are these two lengths, which is why they are asserted rather than trusted.
var (
	wsPingEvent     = []byte(`{"event":"ping"}`)            // 16 bytes
	wsMutationEvent = []byte(`{"event":"entity.mutation"}`) // 27 bytes
)

func TestMeasuredEventPayloadLengths(t *testing.T) {
	if got := len(wsPingEvent); got != 16 {
		t.Errorf("ping event is %d bytes, measured 16; the length bytes in these tables are wrong", got)
	}
	if got := len(wsMutationEvent); got != 27 {
		t.Errorf("mutation event is %d bytes, measured 27; the length bytes in these tables are wrong", got)
	}
}

// wire glues header bytes to a payload so a table entry reads header first,
// body second.
func wire(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// mask applies the RFC 6455 masking transform, which is its own inverse: the
// tests use it to build a (illegal) masked server frame and to undo what
// writeMasked produced.
func mask(key [4]byte, payload []byte) []byte {
	out := make([]byte, len(payload))
	for i, b := range payload {
		out[i] = b ^ key[i%4]
	}
	return out
}

func TestReadFrameParsesEveryLengthForm(t *testing.T) {
	atCap := bytes.Repeat([]byte("x"), maxFramePayload)

	tests := []struct {
		name    string
		in      []byte
		want    frame
		wantErr bool
	}{
		{
			name: "measured 10s keepalive, 7-bit length",
			in:   wire([]byte{0x81, 0x10}, wsPingEvent),
			want: frame{fin: true, opcode: opText, payload: wsPingEvent},
		},
		{
			name: "measured entity.mutation, 7-bit length",
			in:   wire([]byte{0x81, 0x1b}, wsMutationEvent),
			want: frame{fin: true, opcode: opText, payload: wsMutationEvent},
		},
		{
			// Same payload, spelled with the 16-bit length. Homebox does not
			// send it this way; a peer is allowed to.
			name: "126 marker, 16-bit length",
			in:   wire([]byte{0x81, 0x7e, 0x00, 0x1b}, wsMutationEvent),
			want: frame{fin: true, opcode: opText, payload: wsMutationEvent},
		},
		{
			// And with the 64-bit length. RFC 6455 asks a sender to use the
			// shortest form that fits, so this is non-minimal -- readFrame
			// accepts it anyway, because dropping an unambiguous event to
			// punish a sender helps nobody.
			name: "127 marker, 64-bit length",
			in:   wire([]byte{0x81, 0x7f, 0, 0, 0, 0, 0, 0, 0, 0x1b}, wsMutationEvent),
			want: frame{fin: true, opcode: opText, payload: wsMutationEvent},
		},
		{
			name: "payload exactly at the cap",
			in:   wire([]byte{0x81, 0x7e, 0x10, 0x00}, atCap),
			want: frame{fin: true, opcode: opText, payload: atCap},
		},
		{
			// The frame that must be answered within ~6 seconds or the peer
			// hangs up. Empty payload, so nothing to echo but the opcode.
			name: "control ping, empty payload",
			in:   []byte{0x89, 0x00},
			want: frame{fin: true, opcode: opPing, payload: nil},
		},
		{
			name: "control pong",
			in:   []byte{0x8a, 0x00},
			want: frame{fin: true, opcode: opPong, payload: nil},
		},
		{
			name: "close carrying status 1000",
			in:   []byte{0x88, 0x02, 0x03, 0xe8},
			want: frame{fin: true, opcode: opClose, payload: []byte{0x03, 0xe8}},
		},
		{
			// Reassembly lives in the stream layer, so the two halves come back
			// as two frames and the fin bit is what tells them apart.
			name: "first fragment of a text message",
			in:   wire([]byte{0x01, 0x08}, []byte(`{"event"`)),
			want: frame{fin: false, opcode: opText, payload: []byte(`{"event"`)},
		},
		{
			name: "final continuation fragment",
			in:   wire([]byte{0x80, 0x08}, []byte(`:"ping"}`)),
			want: frame{fin: true, opcode: opContinuation, payload: []byte(`:"ping"}`)},
		},
		{
			name: "binary frame",
			in:   wire([]byte{0x82, 0x03}, []byte{0x00, 0xff, 0x7f}),
			want: frame{fin: true, opcode: opBinary, payload: []byte{0x00, 0xff, 0x7f}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(tc.in))
			got, err := readFrame(br, maxFramePayload)
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if got.fin != tc.want.fin {
				t.Errorf("fin = %v, want %v", got.fin, tc.want.fin)
			}
			if got.opcode != tc.want.opcode {
				t.Errorf("opcode = %#x, want %#x", got.opcode, tc.want.opcode)
			}
			if !bytes.Equal(got.payload, tc.want.payload) {
				t.Errorf("payload = %q (%d bytes), want %q (%d bytes)",
					got.payload, len(got.payload), tc.want.payload, len(tc.want.payload))
			}
			// Exactly one frame, no more and no less: a reader left short of
			// the boundary or past it is the failure this file exists to catch.
			if n := br.Buffered(); n != 0 {
				t.Errorf("%d bytes left unconsumed after one frame", n)
			}
		})
	}
}

// A two-fragment message reassembles by concatenation, and readFrame hands back
// the parts in order for whoever does that. Asserted here so the contract the
// stream layer builds on is written down somewhere executable.
func TestReadFrameReturnsFragmentsInOrder(t *testing.T) {
	stream := wire(
		[]byte{0x01, 0x08}, []byte(`{"event"`),
		[]byte{0x80, 0x08}, []byte(`:"ping"}`),
	)
	br := bufio.NewReader(bytes.NewReader(stream))

	var joined []byte
	for i := 0; ; i++ {
		f, err := readFrame(br, maxFramePayload)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if i == 0 && f.opcode != opText {
			t.Errorf("first fragment opcode = %#x, want text", f.opcode)
		}
		if i > 0 && f.opcode != opContinuation {
			t.Errorf("fragment %d opcode = %#x, want continuation", i, f.opcode)
		}
		joined = append(joined, f.payload...)
		if f.fin {
			break
		}
	}
	if !bytes.Equal(joined, wsPingEvent) {
		t.Errorf("reassembled %q, want %q", joined, wsPingEvent)
	}
}

func TestReadFrameRejectsProtocolViolations(t *testing.T) {
	key := [4]byte{0xde, 0xad, 0xbe, 0xef}
	overCap := bytes.Repeat([]byte("x"), maxFramePayload+1)
	bigControl := bytes.Repeat([]byte("z"), 126)

	tests := []struct {
		name string
		in   []byte
		// resync says whether the reader must be left sitting on the next frame
		// boundary. It is true for every violation except the ones where
		// consuming the payload is the very thing being refused.
		resync bool
	}{
		{
			// A server that masks is either not a server or not talking
			// RFC 6455. Either way we cannot trust the rest of the stream.
			name:   "server masked a frame",
			in:     wire([]byte{0x81, 0x90}, key[:], mask(key, wsPingEvent)),
			resync: true,
		},
		{
			name:   "RSV1 set with no extension negotiated",
			in:     wire([]byte{0xc1, 0x10}, wsPingEvent),
			resync: true,
		},
		{
			name:   "RSV2 set",
			in:     wire([]byte{0xa1, 0x10}, wsPingEvent),
			resync: true,
		},
		{
			name:   "RSV3 set",
			in:     wire([]byte{0x91, 0x10}, wsPingEvent),
			resync: true,
		},
		{
			// 126 bytes cannot be spelled in the 7-bit field, so an oversized
			// control frame is only reachable through the extended length --
			// which makes this the case that fails outright if the extended
			// length is parsed after the rejection instead of before it.
			name:   "control frame payload over 125 bytes",
			in:     wire([]byte{0x89, 0x7e, 0x00, 0x7e}, bigControl),
			resync: true,
		},
		{
			name:   "fragmented control frame",
			in:     []byte{0x09, 0x00},
			resync: true,
		},
		{
			name:   "reserved data opcode",
			in:     []byte{0x83, 0x00},
			resync: true,
		},
		{
			name:   "reserved control opcode",
			in:     []byte{0x8b, 0x00},
			resync: true,
		},
		{
			name: "payload one byte over the cap",
			in:   wire([]byte{0x81, 0x7e, 0x10, 0x01}, overCap),
			// Not consumed on purpose: reading it is the allocation being
			// refused, and this failure closes the connection.
			resync: false,
		},
		{
			// Ten bytes of header claiming 16 EiB. Nothing is read, nothing is
			// allocated, and the test returns immediately -- which is the whole
			// point of checking the length against the cap before believing it.
			name:   "64-bit length claiming 16 EiB",
			in:     []byte{0x81, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			resync: false,
		},
	}

	sentinel := wire([]byte{0x81, 0x1b}, wsMutationEvent)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(wire(tc.in, sentinel)))

			_, err := readFrame(br, maxFramePayload)
			if !errors.Is(err, errWSProtocol) {
				t.Fatalf("err = %v, want errWSProtocol", err)
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Errorf("err = %v, which also reads as a truncated connection", err)
			}
			if !tc.resync {
				return
			}
			// The rejected frame was consumed to the byte, so the frame behind
			// it still parses. Get this wrong and there is no error at all,
			// just a watcher that goes quiet.
			next, err := readFrame(br, maxFramePayload)
			if err != nil {
				t.Fatalf("frame after the rejected one: %v (the stream desynchronised)", err)
			}
			if !bytes.Equal(next.payload, wsMutationEvent) {
				t.Errorf("next payload = %q, want %q", next.payload, wsMutationEvent)
			}
		})
	}
}

// A connection that ends is not a peer that is wrong, and the watcher treats
// them differently: one redials, the other is worth logging with the bytes.
func TestReadFrameDistinguishesTruncationFromProtocolError(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"nothing at all, a clean frame boundary", nil, io.EOF},
		{"header cut after one byte", []byte{0x81}, io.ErrUnexpectedEOF},
		{"16-bit length cut in half", []byte{0x81, 0x7e, 0x00}, io.ErrUnexpectedEOF},
		{"64-bit length cut short", []byte{0x81, 0x7f, 0, 0, 0}, io.ErrUnexpectedEOF},
		{"mask key cut short", []byte{0x81, 0x90, 0xde, 0xad}, io.ErrUnexpectedEOF},
		{"payload cut short", wire([]byte{0x81, 0x10}, wsPingEvent[:5]), io.ErrUnexpectedEOF},
		{"payload absent entirely", []byte{0x81, 0x10}, io.ErrUnexpectedEOF},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReader(bytes.NewReader(tc.in))
			_, err := readFrame(br, maxFramePayload)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, errWSProtocol) {
				t.Errorf("err = %v, which also reads as a protocol violation", err)
			}
		})
	}
}

// The cap is a parameter so a caller can tighten it; the constant is only the
// default. Both edges are checked against the same reader.
func TestReadFrameHonoursTheCapItIsGiven(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 32)
	in := wire([]byte{0x81, 0x20}, payload)

	if _, err := readFrame(bufio.NewReader(bytes.NewReader(in)), 32); err != nil {
		t.Errorf("a 32-byte payload at max=32: %v", err)
	}
	_, err := readFrame(bufio.NewReader(bytes.NewReader(in)), 31)
	if !errors.Is(err, errWSProtocol) {
		t.Errorf("a 32-byte payload at max=31: err = %v, want errWSProtocol", err)
	}
}

// The published vector from RFC 6455 section 1.3. If this drifts, the handshake
// check drifts with it and we start trusting a 101 that proves nothing.
func TestAcceptKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"RFC 6455 section 1.3 vector", "dGhlIHNhbXBsZSBub25jZQ==", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="},
		{"empty key still hashes the GUID", "", "Kfh9QIsMVZcl6xEPYxPHzW8SZ8w="},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := acceptKey(tc.key); got != tc.want {
				t.Errorf("acceptKey(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestWriteMaskedRoundTrips(t *testing.T) {
	tests := []struct {
		name    string
		opcode  byte
		payload []byte
	}{
		{"the pong that keeps the socket alive", opPong, nil},
		{"pong echoing a ping payload", opPong, []byte("keepalive")},
		{"close with status 1000", opClose, []byte{0x03, 0xe8}},
		{"a control payload at the 125-byte limit", opPing, bytes.Repeat([]byte("p"), 125)},
		{"16-bit length form", opText, bytes.Repeat([]byte("t"), 200)},
		{"64-bit length form", opText, bytes.Repeat([]byte("t"), 0x10000)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]byte(nil), tc.payload...)

			var buf bytes.Buffer
			if err := writeMasked(&buf, tc.opcode, tc.payload); err != nil {
				t.Fatalf("writeMasked: %v", err)
			}
			got := buf.Bytes()

			if want := byte(0x80 | tc.opcode); got[0] != want {
				t.Errorf("first byte = %#02x, want %#02x (fin set, opcode %#x)", got[0], want, tc.opcode)
			}
			if got[1]&0x80 == 0 {
				t.Fatal("mask bit clear: a server MUST fail the connection on an unmasked client frame")
			}

			n := uint64(got[1] & 0x7f)
			rest := got[2:]
			switch n {
			case 126:
				n = uint64(binary.BigEndian.Uint16(rest[:2]))
				rest = rest[2:]
			case 127:
				n = binary.BigEndian.Uint64(rest[:8])
				rest = rest[8:]
			}
			if n != uint64(len(tc.payload)) {
				t.Fatalf("encoded length = %d, want %d", n, len(tc.payload))
			}

			var key [4]byte
			copy(key[:], rest[:4])
			body := rest[4:]
			if uint64(len(body)) != n {
				t.Fatalf("body is %d bytes, header says %d", len(body), n)
			}
			if unmasked := mask(key, body); !bytes.Equal(unmasked, tc.payload) {
				t.Errorf("unmasked payload = %q, want %q", unmasked, tc.payload)
			}
			if !bytes.Equal(tc.payload, original) {
				t.Error("writeMasked masked the caller's slice in place")
			}
		})
	}
}

// readFrame and writeMasked have to agree on the layout, and the cheapest proof
// is that the reader gets all the way to the mask check on the writer's output
// with the stream still aligned -- which it only can if the header, the length
// form and the key are all where the other half expects them.
func TestReadFrameParsesWriteMaskedLayout(t *testing.T) {
	var buf bytes.Buffer
	if err := writeMasked(&buf, opText, wsMutationEvent); err != nil {
		t.Fatalf("writeMasked: %v", err)
	}
	buf.Write(wire([]byte{0x81, 0x1b}, wsMutationEvent))

	br := bufio.NewReader(bytes.NewReader(buf.Bytes()))
	if _, err := readFrame(br, maxFramePayload); !errors.Is(err, errWSProtocol) {
		t.Fatalf("err = %v, want errWSProtocol (a masked frame from the wrong direction)", err)
	}
	next, err := readFrame(br, maxFramePayload)
	if err != nil {
		t.Fatalf("frame after the masked one: %v (the writer's framing is off)", err)
	}
	if !bytes.Equal(next.payload, wsMutationEvent) {
		t.Errorf("next payload = %q, want %q", next.payload, wsMutationEvent)
	}
}

// A mask key reused across frames gives back the property masking exists for:
// the sender would be emitting a predictable transform of bytes it was handed.
// One collision in 2^32 is the flake budget here.
func TestWriteMaskedUsesAFreshKeyEachFrame(t *testing.T) {
	payload := []byte("keepalive")

	keyOf := func(t *testing.T) [4]byte {
		t.Helper()
		var buf bytes.Buffer
		if err := writeMasked(&buf, opPong, payload); err != nil {
			t.Fatalf("writeMasked: %v", err)
		}
		var key [4]byte
		copy(key[:], buf.Bytes()[2:6])
		return key
	}

	first, second := keyOf(t), keyOf(t)
	if first == second {
		t.Errorf("two frames shared mask key %x", first)
	}
}
