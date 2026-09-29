package homebox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The fake Homebox in this file is a HIJACKING server: it writes the 101 by
// hand and then owns the raw connection, because half of what is being tested
// is bytes that no ResponseWriter would let a test produce -- a wrong accept
// key, a control ping, a frame in three pieces.
//
// CRITICAL, and the reason every observation below leaves on a BUFFERED
// channel or an atomic: httptest.Server.Close() returns WITHOUT waiting for a
// hijacked handler's goroutine. Close waits only for connections the server
// still tracks, and hijacking is precisely the act of handing one away. So a
// t.Errorf, t.Log or t.Fatalf from one of these goroutines can run after the
// test function has returned, and the testing package answers that by
// panicking the whole binary ("Log in goroutine after Test... has completed")
// -- one stray assertion takes down every other test's result with it.
// Buffered so that a goroutine outliving its test never parks forever on a
// send nobody is left to receive.

// Opcodes spelled as literals rather than borrowed from wsframe.go. A test
// server is a peer, and a peer that agreed with us about 0x9 by sharing our
// constant would prove nothing.
const (
	wsOpCont  = 0x0
	wsOpText  = 0x1
	wsOpClose = 0x8
	wsOpPing  = 0x9
	wsOpPong  = 0xA
)

// wsFrame builds one server frame: {fin|opcode, len} + payload, unmasked as a
// server's frames must be. Every payload in this file is under 126 bytes, so
// the 7-bit length form is the only one needed here (wsframe_test.go covers
// the others).
func wsFrame(fin bool, opcode byte, payload []byte) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	return append([]byte{first, byte(len(payload))}, payload...)
}

// wsFrameLong spells the same frame with the 16-bit length form, which
// wsFrame's 7-bit field cannot reach. Only the fragmentation-cap test needs a
// payload that large.
func wsFrameLong(fin bool, opcode byte, payload []byte) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	n := len(payload)
	return append([]byte{first, 126, byte(n >> 8), byte(n)}, payload...)
}

// wsText is the {0x81, len} + payload shape Homebox sends every event in.
func wsText(payload []byte) []byte { return wsFrame(true, wsOpText, payload) }

// wsTestAccept computes Sec-WebSocket-Accept independently of acceptKey, so
// that a handshake the client accepts is one where two separate
// implementations of RFC 6455 section 1.3 agreed, not one where the same three
// lines were compared with themselves.
func wsTestAccept(clientKey string) string {
	h := sha1.New()
	io.WriteString(h, clientKey+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ws101 bends the 101 the fake server writes, which is the only way to test a
// client's judgement of one. The zero value is the correct handshake.
type ws101 struct {
	// accept replaces the computed Sec-WebSocket-Accept.
	accept func(clientKey string) string
	// upgrade replaces the Upgrade header value; empty means "websocket".
	upgrade string
	// extra is appended verbatim before the blank line, each header ending in
	// CRLF. It is how a test announces something nobody offered.
	extra string
}

// wsUpgrade hijacks and writes the 101 that resp describes.
func wsUpgrade(w http.ResponseWriter, r *http.Request, resp ws101) (net.Conn, *bufio.Reader, bool) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "not hijackable", http.StatusInternalServerError)
		return nil, nil, false
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, false
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	value := wsTestAccept(key)
	if resp.accept != nil {
		value = resp.accept(key)
	}
	upgrade := resp.upgrade
	if upgrade == "" {
		upgrade = "websocket"
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: " + upgrade + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + value + "\r\n" +
		resp.extra + "\r\n")); err != nil {
		conn.Close()
		return nil, nil, false
	}
	return conn, brw.Reader, true
}

// wsFakeServer is an httptest.Server that speaks the handshake, plus the
// channel its handshake requests come out on.
type wsFakeServer struct {
	*httptest.Server
	reqs chan *http.Request
}

// newWSServer starts a fake Homebox that completes the handshake and then runs
// handle on the raw connection. A nil handle parks until the client goes away,
// which is what keeps the connection alive for the duration of a test that
// only cares about the handshake.
func newWSServer(t *testing.T, handle func(conn net.Conn, br *bufio.Reader)) *wsFakeServer {
	t.Helper()
	if handle == nil {
		handle = parkUntilClientLeaves
	}
	s := &wsFakeServer{reqs: make(chan *http.Request, 8)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.reqs <- r.Clone(context.Background())
		conn, br, ok := wsUpgrade(w, r, ws101{})
		if !ok {
			return
		}
		defer conn.Close()
		handle(conn, br)
	}))
	t.Cleanup(s.Close)
	return s
}

// parkUntilClientLeaves blocks on a read so the socket stays open until the
// client closes it. A handler that returned immediately would close the
// connection under a stream the test is still using.
func parkUntilClientLeaves(conn net.Conn, br *bufio.Reader) { _, _ = br.ReadByte() }

// clientFrame is a frame the fake server read from the CLIENT, unmasked. It
// travels to the test goroutine on a buffered channel; see the note at the top
// of this file.
type clientFrame struct {
	opcode  byte
	masked  bool
	payload []byte
	err     error
}

func readClientFrame(br *bufio.Reader) clientFrame {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return clientFrame{err: err}
	}
	f := clientFrame{opcode: hdr[0] & 0x0f, masked: hdr[1]&0x80 != 0}
	var key [4]byte
	if f.masked {
		if _, err := io.ReadFull(br, key[:]); err != nil {
			return clientFrame{err: err}
		}
	}
	body := make([]byte, int(hdr[1]&0x7f))
	if _, err := io.ReadFull(br, body); err != nil {
		return clientFrame{err: err}
	}
	if f.masked {
		for i := range body {
			body[i] ^= key[i%4]
		}
	}
	f.payload = body
	return f
}

// testOptions keeps every wait short enough that a hung test fails inside the
// suite rather than at the package timeout.
func testOptions() Options {
	return Options{HandshakeTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second}
}

func recvRequest(t *testing.T, ch <-chan *http.Request) *http.Request {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no handshake request reached the server")
		return nil
	}
}

func recvFrame(t *testing.T, ch <-chan clientFrame, what string) clientFrame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return clientFrame{}
	}
}

// The handshake is the part of this that talks to somebody else's deployment,
// so every element of it is pinned: the path, the credential, the three
// upgrade headers, and the nonce.
func TestSubscribeHandshakeRequest(t *testing.T) {
	srv := newWSServer(t, nil)

	keys := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
		if err != nil {
			t.Fatalf("Subscribe #%d: %v", i, err)
		}
		r := recvRequest(t, srv.reqs)

		if r.URL.Path != "/api/v1/ws/events" {
			t.Errorf("path = %q, want /api/v1/ws/events", r.URL.Path)
		}
		// The credential in the header and NOWHERE else. Homebox accepts a
		// token as a query parameter too, and that form would put the key in
		// its access log on every reconnect.
		if got := r.URL.RawQuery; got != "" {
			t.Errorf("query string = %q, want empty: the token must never reach Homebox's access log", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer hb_test-token" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer hb_test-token")
		}
		if got := r.Header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
			t.Errorf("Upgrade = %q, want websocket", got)
		}
		if got := r.Header.Get("Connection"); !strings.Contains(strings.ToLower(got), "upgrade") {
			t.Errorf("Connection = %q, want it to contain Upgrade", got)
		}
		if got := r.Header.Get("Sec-WebSocket-Version"); got != "13" {
			t.Errorf("Sec-WebSocket-Version = %q, want 13", got)
		}

		key := r.Header.Get("Sec-WebSocket-Key")
		nonce, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			t.Errorf("Sec-WebSocket-Key %q is not base64: %v", key, err)
		} else if len(nonce) != 16 {
			t.Errorf("Sec-WebSocket-Key decodes to %d bytes, want the 16 RFC 6455 requires", len(nonce))
		}
		keys = append(keys, key)

		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}

	// The nonce is what makes the accept value prove liveness rather than
	// replay, so a constant one would quietly defeat the check in Subscribe.
	if keys[0] == keys[1] {
		t.Errorf("both handshakes used Sec-WebSocket-Key %q; the nonce must be fresh per connection", keys[0])
	}
}

// The two upgrade headers are assigned into the request map RAW rather than
// through Header.Set, which would canonicalise them to "Sec-Websocket-Key" and
// "Sec-Websocket-Version". Nothing about that difference is visible through an
// httptest server -- Header.Get canonicalises both spellings to the same key,
// so the switch is invisible to every other test in this file and shows up as
// a bare 400 from behind somebody else's reverse proxy. This one reads the
// bytes off a raw listener instead.
func TestSubscribeSendsTheUpgradeHeadersUncanonicalised(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	// Buffered, and nothing in here touches t: this goroutine outlives the
	// test if the dial never happens. See the note at the top of the file.
	requests := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			requests <- ""
			return
		}
		defer conn.Close()
		var raw strings.Builder
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			raw.WriteString(line)
			if err != nil || line == "\r\n" {
				break
			}
		}
		io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
		requests <- raw.String()
	}()

	s, err := Subscribe(context.Background(), "http://"+ln.Addr().String()+"/api", "hb_test-token", testOptions())
	if err == nil {
		s.Close()
		t.Fatal("Subscribe accepted a 400")
	}

	var raw string
	select {
	case raw = <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never saw a request")
	}
	for _, want := range []string{"Sec-WebSocket-Key: ", "Sec-WebSocket-Version: "} {
		if !strings.Contains(raw, want) {
			t.Errorf("the request does not carry %q verbatim; a proxy that matches header "+
				"names case-sensitively answers a 400 that says nothing. Request was:\n%s", want, raw)
		}
	}
}

// A caller chooses its reconnect behaviour from these, so the three outcomes
// have to be told apart from each other and from a transport error: a revoked
// key must not be retried forever, and a proxy that ate the Upgrade header
// must not be mistaken for one.
func TestSubscribeClassifiesHandshakeFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    error // nil: an error that matches NEITHER sentinel
		wantMsg string
	}{
		{
			// Measured against v0.26.2: this is the body a bad hb_ key gets.
			name:    "rotated or revoked api key",
			status:  http.StatusUnauthorized,
			body:    `{"error":"valid authorization token is required"}`,
			want:    ErrEventsRefused,
			wantMsg: "valid authorization token is required",
		},
		{
			// And this is the body with no Authorization header at all.
			name:    "no credential sent",
			status:  http.StatusUnauthorized,
			body:    `{"error":"authorization header or query is required"}`,
			want:    ErrEventsRefused,
			wantMsg: "authorization header or query is required",
		},
		{
			name:    "key belongs to another group",
			status:  http.StatusForbidden,
			body:    `{"error":"forbidden"}`,
			want:    ErrEventsRefused,
			wantMsg: "forbidden",
		},
		{
			name:    "server predates the endpoint",
			status:  http.StatusNotFound,
			body:    "404 page not found",
			want:    ErrEventsUnsupported,
			wantMsg: "404 page not found",
		},
		{
			name:    "endpoint exists but not for GET",
			status:  http.StatusMethodNotAllowed,
			body:    "",
			want:    ErrEventsUnsupported,
			wantMsg: "405",
		},
		{
			// The common one for a self-hoster: nginx or Traefik dropped the
			// Upgrade header and passed a plain GET through.
			name:    "reverse proxy ate the upgrade",
			status:  http.StatusOK,
			body:    "<!doctype html><title>Homebox</title>",
			want:    ErrEventsUnsupported,
			wantMsg: "no upgrade",
		},
		{
			// Transport-shaped: worth retrying with backoff, so it must match
			// neither sentinel.
			name:    "gateway is down",
			status:  http.StatusBadGateway,
			body:    "bad gateway",
			want:    nil,
			wantMsg: "502",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)

			s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
			if err == nil {
				s.Close()
				t.Fatalf("Subscribe accepted a %d", tt.status)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Subscribe error = %v, want errors.Is(err, %v)", err, tt.want)
			}
			if tt.want == nil {
				if errors.Is(err, ErrEventsRefused) || errors.Is(err, ErrEventsUnsupported) {
					t.Errorf("Subscribe error = %v, want a plain transport error the caller will back off on", err)
				}
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Subscribe error = %v, want it to carry %q", err, tt.wantMsg)
			}
		})
	}
}

// A 101 is not by itself proof of a WebSocket. A proxy or a cache replaying
// somebody else's switch produces one, and reading its bytes as frames is how
// a subscriber starts inventing events.
func TestSubscribeRejectsAWrongAcceptKey(t *testing.T) {
	hungUp := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, br, ok := wsUpgrade(w, r, ws101{accept: func(string) string { return "AAAAAAAAAAAAAAAAAAAAAAAAAAA=" }})
		if !ok {
			return
		}
		defer conn.Close()
		// An event nobody is entitled to read.
		conn.Write(wsText(wsMutationEvent))
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := br.ReadByte()
		hungUp <- err
	}))
	t.Cleanup(srv.Close)

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err == nil {
		s.Close()
		t.Fatal("Subscribe accepted a 101 whose Sec-WebSocket-Accept answered a different key")
	}
	if !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
		t.Errorf("Subscribe error = %v, want it to name the header that was wrong", err)
	}

	select {
	case err := <-hungUp:
		if err == nil {
			t.Error("the client stayed on a connection whose accept key was wrong")
		}
	case <-time.After(5 * time.Second):
		t.Error("the client did not close a connection whose accept key was wrong")
	}
}

// RFC 6455 section 4.1: a client must fail the connection when the 101 names
// an extension or a subprotocol it never offered, and it must check what it
// was actually upgraded to. Both are rejected at the HANDSHAKE on purpose --
// a proxy that negotiated permessage-deflate for itself sends RSV1 frames, and
// left to wsframe.go the failure arrives one frame later as "websocket
// reserved bit set", naming the bit rather than the proxy that set it.
func TestSubscribeRejectsA101ItDidNotAskFor(t *testing.T) {
	tests := []struct {
		name    string
		resp    ws101
		wantMsg string
	}{
		{
			name:    "upgraded to something that is not websocket",
			resp:    ws101{upgrade: "h2c"},
			wantMsg: "not websocket",
		},
		{
			name:    "extension we never offered",
			resp:    ws101{extra: "Sec-WebSocket-Extensions: permessage-deflate\r\n"},
			wantMsg: "Sec-WebSocket-Extensions",
		},
		{
			name:    "subprotocol we never offered",
			resp:    ws101{extra: "Sec-WebSocket-Protocol: homebox-events\r\n"},
			wantMsg: "Sec-WebSocket-Protocol",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, br, ok := wsUpgrade(w, r, tt.resp)
				if !ok {
					return
				}
				defer conn.Close()
				// Bytes the client must never read as frames.
				conn.Write(wsText(wsMutationEvent))
				parkUntilClientLeaves(conn, br)
			}))
			t.Cleanup(srv.Close)

			s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
			if err == nil {
				s.Close()
				t.Fatal("Subscribe accepted a 101 it did not ask for")
			}
			if !errors.Is(err, errWSProtocol) {
				t.Errorf("Subscribe error = %v, want errWSProtocol", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Subscribe error = %v, want it to name %q so the cause is legible", err, tt.wantMsg)
			}
		})
	}
}

// A Bearer token must never be handed to a host we did not choose. net/http
// strips Authorization across a host change but keeps it when only the path or
// the port moves, so the redirect is refused outright rather than reasoned
// about.
func TestSubscribeDoesNotFollowARedirect(t *testing.T) {
	var elsewhereHits atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(elsewhere.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/v1/ws/events", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_secret-token", testOptions())
	if err == nil {
		s.Close()
		t.Fatal("Subscribe followed a 302")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("Subscribe error = %v, want it to report the redirect", err)
	}
	if strings.Contains(err.Error(), "hb_secret-token") {
		t.Errorf("the token is in the error string: %v", err)
	}
	if n := elsewhereHits.Load(); n != 0 {
		t.Errorf("the redirect target was contacted %d times; the token was re-sent to a host we did not choose", n)
	}
}

// The two payloads this socket has ever carried, end to end.
func TestNextDecodesTheMeasuredEvents(t *testing.T) {
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		conn.Write(wsText(wsPingEvent))
		conn.Write(wsText(wsMutationEvent))
		parkUntilClientLeaves(conn, br)
	})

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	for _, want := range []string{EventPing, EventEntityMutation} {
		got, err := s.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got.Name != want {
			t.Errorf("Next = %q, want %q", got.Name, want)
		}
	}
}

// The measured-mandatory one. Ignore the opcode-0x9 ping and the peer hangs up
// about six seconds later; answer it with a MASKED pong and the socket stays
// up indefinitely. Both halves matter -- a server MUST fail the connection on
// an unmasked client frame -- so the fake server asserts on the mask bit as
// well as the opcode.
func TestNextAnswersTheControlPingWithAMaskedPong(t *testing.T) {
	pongs := make(chan clientFrame, 4)
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		conn.Write(wsFrame(true, wsOpPing, []byte("keepalive")))
		pongs <- readClientFrame(br)
		// Only now something for Next to return, so that a Next which
		// swallowed the ping without answering it would hang instead of
		// passing.
		conn.Write(wsText(wsMutationEvent))
		parkUntilClientLeaves(conn, br)
	})

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	got, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got.Name != EventEntityMutation {
		t.Errorf("Next = %q, want the control ping handled transparently and %q returned", got.Name, EventEntityMutation)
	}

	f := recvFrame(t, pongs, "the client's pong")
	if f.err != nil {
		t.Fatalf("server never read a pong: %v", f.err)
	}
	if f.opcode != wsOpPong {
		t.Errorf("client answered the ping with opcode %#x, want %#x", f.opcode, wsOpPong)
	}
	if !f.masked {
		t.Error("the client's pong was NOT masked; RFC 6455 requires every client frame to be, and a server must fail the connection on one that is not")
	}
	if string(f.payload) != "keepalive" {
		t.Errorf("pong payload = %q, want the ping's own payload echoed", f.payload)
	}
}

// The pong has to go out when the ping ARRIVES, not when the caller next asks
// for an event. The measured grace is about six seconds (ping at t=54.0, peer
// closed at t=60.1) and one box-index rebuild is about twenty-one, so a
// keepalive driven by the caller's cadence is a socket that dies during nearly
// every rebuild an event triggers -- as io.EOF, which reads as a peer that
// closed politely.
//
// The server here reproduces that: it hangs up if the pong has not arrived
// within a grace period, scaled down so the test costs milliseconds. The
// caller does what a watcher does, which is spend all of it somewhere else.
func TestTheKeepaliveDoesNotWaitForTheCaller(t *testing.T) {
	const grace = 250 * time.Millisecond

	pongs := make(chan clientFrame, 4)
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		conn.Write(wsFrame(true, wsOpPing, []byte("keepalive")))
		conn.SetReadDeadline(time.Now().Add(grace))
		f := readClientFrame(br)
		pongs <- f
		if f.err != nil {
			return // hang up, exactly as the live server does
		}
		conn.SetReadDeadline(time.Time{})
		conn.Write(wsText(wsMutationEvent))
		parkUntilClientLeaves(conn, br)
	})

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	// Busy elsewhere, and NOT inside Next, for longer than the peer allows.
	time.Sleep(2 * grace)

	f := recvFrame(t, pongs, "the client's pong")
	if f.err != nil {
		t.Fatalf("the server gave up waiting for a pong (%v): the keepalive is only sent "+
			"from inside Next, so a caller busy for longer than the grace loses the socket", f.err)
	}
	if f.opcode != wsOpPong {
		t.Errorf("client answered the ping with opcode %#x, want %#x", f.opcode, wsOpPong)
	}

	got, err := s.Next()
	if err != nil {
		t.Fatalf("Next after a caller that was busy for %v: %v", 2*grace, err)
	}
	if got.Name != EventEntityMutation {
		t.Errorf("Next = %q, want %q", got.Name, EventEntityMutation)
	}
}

// Close is the only thing that ends this connection: the context that opened
// it was released the moment net/http handed the socket over. A watcher whose
// shutdown path does not work leaks a goroutine holding a socket per
// reconnect, and nothing reports it.
func TestCloseUnblocksAParkedNext(t *testing.T) {
	srv := newWSServer(t, nil)

	opts := testOptions()
	opts.IdleTimeout = time.Minute // long enough that only Close can end this
	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", opts)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Next()
		done <- err
	}()
	// Long enough for the goroutine above to actually reach the read. Closing
	// before it parks would still make the test pass, and would test less.
	time.Sleep(50 * time.Millisecond)

	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("Next returned no error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock a parked Next")
	}

	// Twice, from the caller's defer as well as its shutdown path.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// Close ends the connection; it does not wait to be polite about it. Against a
// peer whose receive window has shut, the pong blocks mid-write holding the
// write lock, and with one budget covering both frames Close inherited that
// pong's writeTimeout and then spent its own on top -- measured at 10.0
// seconds, and 20.0 when it queued behind a pong already in flight. A watcher's
// shutdown path would have waited that long to send two bytes nobody was going
// to read.
func TestCloseDoesNotWaitOutAWedgedPeer(t *testing.T) {
	// net.Pipe is synchronous and unbuffered, which is exactly the peer being
	// modelled: a write to it blocks until somebody reads, and nobody will.
	client, peer := net.Pipe()
	t.Cleanup(func() { peer.Close() })

	s := &EventStream{
		conn:   client,
		body:   client,
		br:     bufio.NewReader(client),
		idle:   time.Minute,
		events: make(chan Event, eventBacklog),
		done:   make(chan struct{}),
	}
	go s.readLoop()

	// A control ping, which the reader must answer into a peer that will never
	// read the answer. The write returns once the reader has taken the frame.
	if _, err := peer.Write([]byte{0x89, 0x00}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	// Long enough for the pong to be in flight and holding the write lock.
	time.Sleep(50 * time.Millisecond)

	started := time.Now()
	s.Close()
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("Close took %v against a peer that reads nothing; it must not wait out "+
			"writeTimeout (%v) for a courtesy frame, let alone twice", took, writeTimeout)
	}
	if _, err := s.Next(); err == nil {
		t.Error("Next returned no error after Close")
	}
}

// The other ordering of the same race, and the one the deadline stamp alone
// does not cover: a pong that takes the write lock just AFTER Close has stamped
// its grace on the socket, and stamps its own ten-second budget over it. Close
// is then parked on the lock for the whole of that budget -- measured at 11.0s
// against a peer that reads nothing, which is past Docker's ten-second SIGTERM
// grace, so a clean shutdown comes back as a SIGKILL.
//
// Driven by hand rather than by racing two goroutines: the window is the few
// instructions between taking the lock and stamping the deadline, so a test
// that raced for it would pass most of the time whatever the code did.
func TestAPongCannotOutlastACloseThatIsAlreadyUnderWay(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { peer.Close() })
	t.Cleanup(func() { client.Close() })

	s := &EventStream{
		conn:   client,
		body:   client,
		br:     bufio.NewReader(client),
		idle:   time.Minute,
		events: make(chan Event, eventBacklog),
		done:   make(chan struct{}),
	}
	// Exactly where Close has got to when it is waiting for the lock: the flag
	// is set and the grace is on the socket.
	s.closing.Store(true)
	if err := client.SetWriteDeadline(time.Now().Add(closeGraceTimeout)); err != nil {
		t.Fatalf("stamp the close grace: %v", err)
	}

	started := time.Now()
	err := s.write(opPong, nil)
	took := time.Since(started)
	if err == nil {
		t.Error("the pong into a peer that never reads returned no error")
	}
	if took >= writeTimeout {
		t.Errorf("the pong spent %v on a socket Close had already stamped for %v: it put its own "+
			"%v budget back on, and Close waits out every bit of it", took, closeGraceTimeout, writeTimeout)
	}
}

// The reader must never park on the caller. If it did, a caller that is slow
// or gone would stop the pong going out and the peer would hang up -- the same
// failure as a keepalive driven from inside Next, reached from the other end.
// So a full queue drops, and the socket stays up.
func TestAFullBacklogDoesNotStallTheKeepalive(t *testing.T) {
	pongs := make(chan clientFrame, 4)
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		for i := 0; i < 3*eventBacklog; i++ {
			if _, err := conn.Write(wsText(wsMutationEvent)); err != nil {
				return
			}
		}
		conn.Write(wsFrame(true, wsOpPing, []byte("keepalive")))
		pongs <- readClientFrame(br)
		parkUntilClientLeaves(conn, br)
	})

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	// Nobody has called Next, so the backlog is full and the surplus dropped.
	f := recvFrame(t, pongs, "the client's pong")
	if f.err != nil {
		t.Fatalf("no pong once the backlog filled (%v): the reader parked on a caller "+
			"that never came back, which is the peer hanging up ~6 seconds later", f.err)
	}
	if f.opcode != wsOpPong {
		t.Errorf("answered with opcode %#x, want %#x", f.opcode, wsOpPong)
	}
	// And what was queued is still there to collect.
	got, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got.Name != EventEntityMutation {
		t.Errorf("Next = %q, want %q", got.Name, EventEntityMutation)
	}
}

// A storage unit's signal drops without the TCP connection noticing, so a
// silent socket has to be a reported failure and not a permanent park.
func TestNextTimesOutOnASilentServer(t *testing.T) {
	srv := newWSServer(t, nil)

	opts := testOptions()
	opts.IdleTimeout = 100 * time.Millisecond
	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", opts)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	done := make(chan error, 1)
	go func() {
		_, err := s.Next()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("Next error = %v, want a deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next never gave up on a server that said nothing")
	}
}

// The upgrade path. A Homebox that grows an event, or a proxy that injects
// something, must cost us that message and nothing else -- dropping the
// connection here would turn "Boxwright does not know about the new event"
// into "Boxwright stopped noticing changes" for everyone who upgrades.
func TestNextSurvivesGarbageAndUnknownEvents(t *testing.T) {
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		conn.Write(wsText([]byte("not json at all")))
		conn.Write(wsText([]byte(`{"event":"tags.mutation"}`)))
		conn.Write(wsText([]byte(`{"event":`))) // truncated JSON
		conn.Write(wsText([]byte(`{}`)))        // valid JSON, no name
		conn.Write(wsText([]byte(`{"event":"quantum.mutation","extra":{"x":1}}`)))
		conn.Write(wsText(wsMutationEvent))
		parkUntilClientLeaves(conn, br)
	})

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	want := []string{EventTagsMutation, "quantum.mutation", EventEntityMutation}
	for i, w := range want {
		got, err := s.Next()
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		if got.Name != w {
			t.Errorf("Next #%d = %q, want %q", i, got.Name, w)
		}
	}
}

// Homebox has never fragmented one of these -- they are 16 and 27 bytes -- but
// readFrame hands back frames rather than messages, so a peer that does
// fragment has to be reassembled here or the event is lost as three
// undecodable pieces.
func TestNextReassemblesAFragmentedMessage(t *testing.T) {
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		conn.Write(wsFrame(false, wsOpText, []byte(`{"event"`)))
		conn.Write(wsFrame(false, wsOpCont, []byte(`:"entity.`)))
		conn.Write(wsFrame(true, wsOpCont, []byte(`mutation"}`)))
		parkUntilClientLeaves(conn, br)
	})

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	got, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got.Name != EventEntityMutation {
		t.Errorf("Next = %q, want %q reassembled from three frames", got.Name, EventEntityMutation)
	}
}

// readFrame caps each FRAME; nothing caps how many fragments a peer may send,
// so the reassembled message needs the same bound. Without it a peer -- or a
// proxy -- that fragments forever decides how much memory we hold: measured at
// 10,903,904 bytes of heap growth from 2000 continuation frames, against
// 16,640 bytes and an immediate refusal with the bound in place.
func TestNextRefusesAnUnboundedFragmentedMessage(t *testing.T) {
	fragment := bytes.Repeat([]byte("x"), 4000)
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		conn.Write(wsFrameLong(false, wsOpText, fragment))
		conn.Write(wsFrameLong(true, wsOpCont, fragment))
		parkUntilClientLeaves(conn, br)
	})

	opts := testOptions()
	// So that a bound which never fires fails here as a timeout in half a
	// second rather than parking the suite.
	opts.IdleTimeout = 500 * time.Millisecond
	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", opts)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	_, err = s.Next()
	if !errors.Is(err, errWSProtocol) {
		t.Fatalf("Next error = %v, want errWSProtocol: 8000 bytes of fragments is past the %d-byte cap",
			err, maxFramePayload)
	}
	if !strings.Contains(err.Error(), "fragmented message") {
		t.Errorf("Next error = %v, want it to say which bound was hit", err)
	}
}

// A close frame is the connection ending politely, so it reports as io.EOF --
// the same thing a watcher redials on -- and not as a peer that is wrong.
func TestNextReportsAServerClose(t *testing.T) {
	srv := newWSServer(t, func(conn net.Conn, br *bufio.Reader) {
		conn.Write(wsFrame(true, wsOpClose, []byte{0x03, 0xe8}))
		parkUntilClientLeaves(conn, br)
	})

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", testOptions())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next error = %v, want it to satisfy io.EOF", err)
	} else if !strings.Contains(err.Error(), "1000") {
		t.Errorf("Next error = %v, want it to carry the close status", err)
	}
}

// The guarantee, over TLS, on a deployment neither CI nor a dev machine runs:
// an h2 connection cannot be handed back as a byte stream, so an operator who
// terminates TLS would get a 101 that is not assertable to io.ReadWriteCloser
// and no clue why. httptest's EnableHTTP2 server advertises "h2" and nothing
// else, which is the harshest version of that deployment.
//
// What this does NOT prove is that Subscribe's own latches are what achieve
// it: net/http forces HTTP/1 for an Upgrade request by itself (see the comment
// beside them), so deleting every one of them leaves this test green. It
// asserts the observable guarantee instead -- no ALPN offer of "h2" reaches
// the wire, and the connection carries events -- which is the thing that would
// have to keep holding if net/http's behaviour ever changed.
func TestSubscribeOverTLSNeverNegotiatesHTTP2(t *testing.T) {
	protos := make(chan string, 4)
	offered := make(chan []string, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protos <- r.Proto
		conn, br, ok := wsUpgrade(w, r, ws101{})
		if !ok {
			return
		}
		defer conn.Close()
		conn.Write(wsText(wsMutationEvent))
		parkUntilClientLeaves(conn, br)
	}))
	srv.EnableHTTP2 = true
	// StartTLS clones this and fills in its certificate and NextProtos, so the
	// hook survives and the server is still the h2-only one. Returning a nil
	// config means "carry on with your own".
	srv.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			offered <- append([]string(nil), hello.SupportedProtos...)
			return nil, nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	opts := testOptions()
	// httptest signs with its own CA, which is also the case Options.
	// TLSClientConfig exists for: a self-hosted Homebox behind a private CA.
	opts.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig

	s, err := Subscribe(context.Background(), srv.URL+"/api", "hb_test-token", opts)
	if err != nil {
		t.Fatalf("Subscribe over TLS: %v", err)
	}
	defer s.Close()

	got, err := s.Next()
	if err != nil {
		t.Fatalf("Next over TLS: %v", err)
	}
	if got.Name != EventEntityMutation {
		t.Errorf("Next = %q, want %q", got.Name, EventEntityMutation)
	}

	select {
	case p := <-protos:
		if p != "HTTP/1.1" {
			t.Errorf("server saw %s; the upgrade only exists over HTTP/1.1", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the TLS server")
	}

	select {
	case alpn := <-offered:
		for _, p := range alpn {
			if p == "h2" {
				t.Errorf("the ClientHello offered ALPN %v; an h2 connection cannot be handed "+
					"back as a byte stream at all", alpn)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ClientHello reached the TLS server")
	}

	// The caller's config is cloned, never edited: overwriting NextProtos on
	// the value a caller handed in would change how their OTHER connections
	// negotiate.
	if n := len(opts.TLSClientConfig.NextProtos); n != 0 {
		t.Errorf("caller's TLSClientConfig.NextProtos was modified to %v", opts.TLSClientConfig.NextProtos)
	}
}
