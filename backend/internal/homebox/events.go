// The connection half of Homebox's live-events feed: one HTTP request that
// turns into a WebSocket, one goroutine that reads its frames, and a Next that
// hands over what those frames turned into. It knows nothing about what an
// event MEANS -- no cache, no box index, no placement -- because the thing that
// decides what is now stale is the API layer, and all this file can honestly
// report is that something changed.
//
// And that really is all it can report. Measured against a live v0.26.2 on
// 2026-09-10: the mutation frame is 27 bytes long and its entire content is
// {"event":"entity.mutation"}. No id, no entity type, no operation, no
// timestamp, no group. Nothing here can ever be an incremental update.
//
// So the interesting failures are not decoding failures. They are a pong that
// went out too late, a read deadline that fired in the middle of a frame, an
// h2 negotiation that made the connection unhijackable, and a Close that could
// not wake the reader -- each of which presents as "the watcher stopped
// noticing changes" and none of which announces itself. Every one has its
// comment below.

package homebox

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one message from the events socket. Name is the whole event; see
// the file comment for why there is nothing else to carry.
type Event struct {
	Name string
}

// The event names this socket has been observed to carry. Homebox is free to
// add more and Next hands an unrecognised name straight to the caller, so
// these are the names worth writing down rather than the names allowed.
const (
	// EventPing is the 10-second application-level keepalive, and it arrives as
	// an ordinary TEXT frame. It needs no reply, and answering it would not
	// help: it is NOT what holds the socket open. See the control ping in Next.
	EventPing = "ping"

	// EventEntityMutation fires on any entity change, ITEMS included -- a plain
	// POST /v1/entities produced one and the DELETE produced a second -- so a
	// caller that only cares about locations still has to decide per event
	// whether anything it holds is affected.
	EventEntityMutation = "entity.mutation"

	// The rest share the socket. They are named so that a caller ignoring them
	// is ignoring something, rather than discarding an event it never knew
	// existed.
	EventTagsMutation   = "tags.mutation"
	EventUserMutation   = "user.mutation"
	EventExportMutation = "export.mutation"
	EventImportMutation = "import.mutation"
)

// ErrEventsRefused marks a handshake the server actively turned down: 401 or
// 403. For Boxwright that means the API key was rotated or revoked, and
// redialling with the same credential will be refused in exactly the same way,
// so a caller should stop and say so rather than back off and retry forever.
var ErrEventsRefused = errors.New("homebox refused the events subscription")

// ErrEventsUnsupported marks a server that has no live-events endpoint at this
// address: 404 or 405, or a 2xx with no upgrade, which is what a reverse proxy
// that dropped the Upgrade header produces. Both are permanent for that
// deployment, and the answer is to keep running on the TTL rather than to
// reconnect in a loop.
var ErrEventsUnsupported = errors.New("homebox has no events endpoint")

const (
	// defaultHandshakeTimeout bounds the HTTP half and nothing else. A
	// subscription that has not been accepted in ten seconds is not going to
	// be.
	defaultHandshakeTimeout = 10 * time.Second

	// defaultIdleTimeout is how long a silent socket may stay silent.
	//
	// Measured: the server sends {"event":"ping"} every 10 seconds AND a
	// control ping every 54. Thirty seconds would be a fine bound on the first
	// of those and a reconnect loop on a Homebox that ever stops sending it,
	// so this is set above the slower of the two clocks with room for one
	// missed round. It is a liveness bound, not a latency budget: an event
	// arrives when it arrives.
	defaultIdleTimeout = 90 * time.Second

	// writeTimeout bounds the pong, which is under ten bytes and fits in any
	// socket buffer. It exists so that a peer which has wedged fails the
	// connection instead of holding the reader in a write forever, not as a
	// tuning knob, which is why it is a constant and not an Option.
	writeTimeout = 10 * time.Second

	// closeGraceTimeout is writeTimeout's much shorter counterpart for the
	// goodbye frame, because Close's job is to END the connection and the frame
	// is only a courtesy. Measured against a net.Pipe peer that never reads:
	// with one budget for both, Close took 10.0 seconds, and 20.0 when it
	// queued behind a pong already in flight -- so a shutdown path waited
	// twenty seconds to send two bytes nobody was going to read.
	closeGraceTimeout = time.Second

	// eventBacklog is how many decoded events may wait for a caller that is
	// busy elsewhere. Sized for the burst rather than the average: a Homebox
	// import fires entity.mutation per entity, and 64 of them covers one while
	// the caller is inside a rebuild. What happens past it is in deliver.
	eventBacklog = 64
)

// Options tunes one subscription. The zero value is valid.
type Options struct {
	// HandshakeTimeout bounds the HTTP request that becomes the socket. It does
	// NOT bound the socket afterwards.
	HandshakeTimeout time.Duration

	// IdleTimeout bounds how long the reader waits for a frame before
	// reporting the connection dead. It is a bound on the SOCKET going quiet,
	// not on how long a caller may take between Next calls.
	IdleTimeout time.Duration

	// TLSClientConfig is handed to the transport for an https:// baseURL. A
	// self-hosted Homebox behind a private CA is the ordinary case, and this
	// is the only way to trust one -- Boxwright will not offer an "ignore the
	// certificate" switch.
	//
	// It is cloned, and its NextProtos is overwritten: see Subscribe.
	TLSClientConfig *tls.Config
}

func (o Options) withDefaults() Options {
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = defaultHandshakeTimeout
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	return o
}

// EventStream is one open socket.
//
// The frames are read by a goroutine of the stream's own, NOT by Next, and
// that is the single most important thing about this type. The control ping
// arrives every 54 seconds and a client that has not answered it within about
// six is hung up on; the caller this socket exists for spends ~21 seconds
// inside one box-index rebuild. A Next that owned the read side would send the
// pong only when the caller happened to be back inside it, so the socket would
// die during nearly every rebuild an event triggered -- and it would die as
// io.EOF, which reads as a peer that closed politely.
//
// Next and Close are both safe from any goroutine. Two concurrent Nexts simply
// split the events between them, which is legal and almost never what anyone
// wants.
type EventStream struct {
	// conn is the dialled socket, kept for deadlines and teardown ONLY. Under
	// TLS it is the plaintext socket underneath, so writing to it would send
	// unencrypted bytes into an encrypted stream; every read and write goes
	// through body instead.
	conn net.Conn
	body io.ReadWriteCloser
	br   *bufio.Reader
	idle time.Duration

	// events carries what readLoop decoded, and done is closed when readLoop
	// stops. stopErr says why it stopped and is safe to read ONLY after done is
	// closed -- that channel close is the entire synchronisation between the
	// reader and Next.
	events  chan Event
	done    chan struct{}
	stopErr error

	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
	// closing is set the instant Close starts, before it stamps its grace on
	// the socket and before it goes for writeMu. It is what stops a pong
	// racing that stamp from replacing it with its own much longer budget; see
	// write.
	closing atomic.Bool
}

// dialedConn captures the net.Conn the transport dials.
//
// The 101 response body cannot be asserted to net.Conn -- net/http wraps it --
// and a read deadline is the only way to notice a server that has stopped
// talking, so the connection has to be caught on the way past. The mutex is
// not decoration: DialContext runs on the transport's own goroutine.
type dialedConn struct {
	mu     sync.Mutex
	conn   net.Conn
	dialer net.Dialer
}

func (d *dialedConn) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := d.dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conn = c
	d.mu.Unlock()
	return c, nil
}

func (d *dialedConn) get() net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn
}

// Subscribe opens the events socket and returns a stream positioned before the
// first frame. baseURL is the same value New takes, INCLUDING the /api prefix;
// the endpoint is /v1/ws/events beneath it. token is sent as a Bearer
// credential and may be a session token or a static hb_ API key.
//
// ctx bounds the HANDSHAKE ONLY. Once the connection has switched protocols,
// net/http stops tracking the request and cancelling ctx does nothing to the
// socket -- see Close, which is the only thing that ends it.
//
// The caller owns the returned stream and must Close it.
func Subscribe(ctx context.Context, baseURL, token string, opts Options) (*EventStream, error) {
	opts = opts.withDefaults()

	// http:// and https:// are kept exactly as they are. ws:// and wss:// are
	// the scheme names a BROWSER is given; net/http's Transport rejects them
	// outright, so rewriting to one turns a perfectly good URL into
	// `unsupported protocol scheme "ws"`.
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/ws/events"

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("homebox subscribe: websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	tlsCfg := opts.TLSClientConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	// Advertise HTTP/1.1 and nothing else. RFC 6455's upgrade does not exist
	// over HTTP/2, and more to the point an h2 connection cannot be handed to
	// the caller as a byte stream at all -- net/http would return a normal
	// response body and the io.ReadWriteCloser assertion below would fail.
	// Go's TLS server treats an http/1.1-only client against an h2-only server
	// as an ALPN-less client rather than an error, so pinning this is safe
	// even in front of a server configured for h2 alone.
	//
	// Belt and braces, and it matters which is which: net/http already forces
	// HTTP/1 for THIS request without being asked. Request.requiresHTTP1
	// (GOROOT/src/net/http/request.go) is true for any request carrying
	// Connection: upgrade with Upgrade: websocket, and persistConn.addTLS then
	// sets cfg.NextProtos = nil for it -- measured with a GetConfigForClient
	// hook, the ClientHello offers NO ALPN at all and the pin below never
	// reaches the wire. So deleting the pin breaks no test, which is exactly
	// why that is written down here: it is behaviour, not documented contract,
	// and if it ever changes the symptom is an operator who terminates TLS
	// getting a 101 that cannot be asserted to io.ReadWriteCloser.
	tlsCfg.NextProtos = []string{"http/1.1"}

	dialed := &dialedConn{}
	tr := &http.Transport{
		DialContext:     dialed.dial,
		TLSClientConfig: tlsCfg,
		// Setting DialContext already stops net/http from auto-configuring
		// HTTP/2 on this transport, and NextProtos above stops the server from
		// choosing it. This is the third latch on the same door because the
		// failure it prevents appears ONLY for operators who terminate TLS:
		// neither CI nor a plain-http dev setup would ever reach it.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	handedOff := false
	defer func() {
		// A failed handshake otherwise leaves this one-shot transport's
		// connection sitting in its idle pool with nothing to evict it: this
		// transport sets no IdleConnTimeout, and net/http only arms the idle
		// timer when that field is non-zero (transport.go's tryPutIdleConn), so
		// the 90 seconds of http.DefaultTransport do not apply here. Without
		// this the connection and its reader goroutine would be held for the
		// life of the process -- and the caller of a failed Subscribe is by
		// definition something that is about to try again.
		if !handedOff {
			tr.CloseIdleConnections()
		}
	}()

	// Deliberately NOT the package Client's http.Client. That one carries a 30
	// second Timeout, and a non-zero Timeout is fatal here twice over: net/http
	// wraps the response body in an unexported *cancelTimerBody, so the
	// io.ReadWriteCloser assertion below fails at RUNTIME with nothing to catch
	// it at compile time -- and even if it did not, the timer would kill a
	// socket that is meant to stay open for days. The handshake gets its bound
	// from the context instead.
	hc := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// net/http strips the Authorization header when a redirect changes
			// host, but keeps it when only the path or the port changes, and
			// neither is a hop worth taking silently with a credential in hand.
			// The 302 is reported instead.
			return http.ErrUseLastResponse
		},
	}

	hctx, cancel := context.WithTimeout(ctx, opts.HandshakeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(hctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("homebox subscribe: %w", err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	// Assigned straight into the map rather than through Header.Set, which
	// canonicalises these to "Sec-Websocket-Key" and "Sec-Websocket-Version".
	// Servers and proxies are supposed to match header names
	// case-insensitively; the ones that do not answer with a 400 that says
	// nothing, from the far side of somebody else's reverse proxy. Do not
	// "tidy" these into Set.
	req.Header["Sec-WebSocket-Key"] = []string{key}
	req.Header["Sec-WebSocket-Version"] = []string{"13"}
	if token != "" {
		// The credential goes in the header and ONLY in the header. Homebox
		// also accepts it as a query parameter (measured), and that form puts
		// the key in a URL, where any request log between here and Homebox
		// keeps it.
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("homebox subscribe: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		// Capped read of the body in Client.do's style: Homebox's error text is
		// one short line, and it is the only thing that distinguishes a rotated
		// key from a server started without HBOX_AUTH_API_KEY_PEPPER.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		body := strings.TrimSpace(string(b))
		switch {
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			return nil, fmt.Errorf("homebox subscribe: status %d: %s: %w", resp.StatusCode, body, ErrEventsRefused)
		case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed:
			return nil, fmt.Errorf("homebox subscribe: status %d: %s: %w", resp.StatusCode, body, ErrEventsUnsupported)
		case resp.StatusCode/100 == 2:
			// A 2xx here is not an older Homebox. It is a reverse proxy that
			// ate the Upgrade header and passed a plain GET through, which is
			// the single most common way this feature fails for someone
			// running behind nginx or Traefik.
			return nil, fmt.Errorf("homebox subscribe: status %d with no upgrade: %s: %w", resp.StatusCode, body, ErrEventsUnsupported)
		}
		return nil, fmt.Errorf("homebox subscribe: status %d: %s", resp.StatusCode, body)
	}

	// A 101 is not yet proof of a WebSocket. Checking the accept value is how a
	// client establishes it reached the endpoint it asked for rather than a
	// cache or a proxy replaying somebody else's switch, BEFORE it starts
	// reading bytes as frames.
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		resp.Body.Close()
		return nil, fmt.Errorf("homebox subscribe: Sec-WebSocket-Accept %q does not answer our key: %w", got, errWSProtocol)
	}
	if got := resp.Header.Get("Upgrade"); !strings.EqualFold(got, "websocket") {
		resp.Body.Close()
		return nil, fmt.Errorf("homebox subscribe: 101 upgraded to %q, not websocket: %w", got, errWSProtocol)
	}
	// RFC 6455 section 4.1: a client MUST fail the connection when the 101
	// names an extension or a subprotocol it did not offer. We offer neither,
	// so any value at all is one. Refusing it HERE is the point: a proxy that
	// negotiates permessage-deflate on its own account then sends RSV1 frames,
	// and left to wsframe.go the failure arrives one frame later as "websocket
	// reserved bit set", which names the bit and not the proxy that set it.
	if got := resp.Header.Get("Sec-WebSocket-Extensions"); got != "" {
		resp.Body.Close()
		return nil, fmt.Errorf("homebox subscribe: 101 negotiated Sec-WebSocket-Extensions %q, which we did not offer: %w", got, errWSProtocol)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "" {
		resp.Body.Close()
		return nil, fmt.Errorf("homebox subscribe: 101 negotiated Sec-WebSocket-Protocol %q, which we did not offer: %w", got, errWSProtocol)
	}

	// net/http hands the switched-protocol connection back as Response.Body,
	// which on a 101 also implements io.Writer (documented at
	// net/http/response.go since Go 1.12). This assertion is the entire reason
	// the client above has no Timeout.
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("homebox subscribe: 101 body is %T, which is not writable", resp.Body)
	}
	conn := dialed.get()
	if conn == nil {
		resp.Body.Close()
		return nil, fmt.Errorf("homebox subscribe: no dialled connection was captured")
	}

	handedOff = true
	s := &EventStream{
		conn:   conn,
		body:   rwc,
		br:     bufio.NewReader(rwc),
		idle:   opts.IdleTimeout,
		events: make(chan Event, eventBacklog),
		done:   make(chan struct{}),
	}
	// The reader starts here rather than on the first Next, because the ping
	// that has to be answered starts arriving here too.
	go s.readLoop()
	return s, nil
}

// Next returns the next event the reader decoded.
//
// It blocks until one arrives, the idle timeout expires, the peer goes away or
// Close is called. Control frames, unparseable payloads and events this package
// has never heard of never surface here: an error from Next means the
// connection is finished and the caller should redial.
//
// The caller may take as long as it likes between calls. The socket is held
// open by readLoop, not by this.
func (s *EventStream) Next() (Event, error) {
	select {
	case ev := <-s.events:
		return ev, nil
	case <-s.done:
		// A server that sends a mutation and then closes delivers both in the
		// same breath, and the select above would pick between them at random.
		// Events the reader did decode are handed over before the error that
		// stopped it.
		select {
		case ev := <-s.events:
			return ev, nil
		default:
		}
		return Event{}, s.stopErr
	}
}

// deliver queues one event for Next, and DROPS it rather than wait when the
// caller is eventBacklog events behind.
//
// Parking here would put the keepalive back on the caller's clock, which is
// the one thing readLoop exists to prevent. Dropping is sound because of what
// these events do not carry (see the file comment): each means no more than
// "something changed, go and look", and the queue is full only when the caller
// still has 64 unread notifications saying exactly that. Whatever it does in
// response to those runs after the dropped frame arrived, so it observes the
// change that frame was announcing.
//
// The select must therefore stay non-blocking for a second reason as well:
// Close waits for readLoop to stop, and a reader parked on a caller that never
// came back is a Close that never returns.
func (s *EventStream) deliver(ev Event) {
	select {
	case s.events <- ev:
	default:
	}
}

// readLoop owns the read side for the life of the connection: it answers the
// control ping when the ping ARRIVES, reassembles fragmented messages, and
// hands finished events to Next. It exits on the first error, which is what
// Next reports from then on.
func (s *EventStream) readLoop() {
	defer close(s.done)

	var partial []byte
	assembling := false

	for {
		f, err := s.nextFrame()
		if err != nil {
			s.stopErr = err
			return
		}

		var msg []byte
		switch f.opcode {
		case opPing:
			// MANDATORY, and measured rather than assumed. The control ping
			// arrives every 54 seconds and a client that does not answer it is
			// disconnected about 6 seconds later (ping at t=54.0, peer closed
			// at t=60.1); a client that answers with a masked pong stayed
			// connected across three pings to t=190. The 10-second
			// {"event":"ping"} TEXT frame is NOT what keeps this socket alive,
			// so "traffic is flowing" is not a reason to skip this.
			if err := s.write(opPong, f.payload); err != nil {
				s.stopErr = fmt.Errorf("homebox events: pong: %w", err)
				return
			}
			continue
		case opPong:
			// We never ping, so this is unsolicited and RFC 6455 section 5.5.3
			// says to ignore it.
			continue
		case opClose:
			code := 0
			if len(f.payload) >= 2 {
				code = int(binary.BigEndian.Uint16(f.payload[:2]))
			}
			// Wrapped around io.EOF because that is what it is: the peer ended
			// the conversation politely. Our own close frame goes out from
			// Close, which the caller reaches through its defer.
			s.stopErr = fmt.Errorf("homebox events: server closed the stream (status %d): %w", code, io.EOF)
			return
		case opText, opBinary:
			if assembling {
				s.stopErr = fmt.Errorf("homebox events: data frame inside a fragmented message: %w", errWSProtocol)
				return
			}
			if !f.fin {
				assembling = true
				partial = append(partial, f.payload...)
				continue
			}
			msg = f.payload
		case opContinuation:
			if !assembling {
				s.stopErr = fmt.Errorf("homebox events: continuation frame with no message to continue: %w", errWSProtocol)
				return
			}
			// readFrame caps each FRAME; nothing caps the number of fragments,
			// so the assembled message needs the same bound for the same
			// reason. Homebox has never fragmented one of these (they are 16
			// and 27 bytes), so reaching this is already a surprise.
			if len(partial)+len(f.payload) > maxFramePayload {
				s.stopErr = fmt.Errorf("homebox events: fragmented message exceeds %d bytes: %w", maxFramePayload, errWSProtocol)
				return
			}
			partial = append(partial, f.payload...)
			if !f.fin {
				continue
			}
			msg, partial, assembling = partial, nil, false
		}

		var got struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(msg, &got); err != nil || got.Event == "" {
			// NEVER fatal, and never even reported. A payload shape we do not
			// recognise is a Homebox that has grown something, and dropping the
			// connection over it would turn "Boxwright does not know about the
			// new event" into "Boxwright stopped noticing any changes at all"
			// on the day someone upgrades.
			continue
		}
		// Delivered whatever the name is, including one that matches none of
		// the constants above. Deciding which events matter belongs to the
		// caller's switch, where a default case is free; deciding it here would
		// mean discarding an event nobody can see was discarded.
		s.deliver(Event{Name: got.Event})
	}
}

// nextFrame reads one frame under the idle deadline.
func (s *EventStream) nextFrame() (frame, error) {
	if err := s.conn.SetReadDeadline(time.Now().Add(s.idle)); err != nil {
		return frame{}, fmt.Errorf("homebox events: set read deadline: %w", err)
	}
	// Park on the first byte separately, so the deadline that bounds "the
	// server has gone quiet" is not the same deadline that bounds "this frame
	// is still arriving".
	if _, err := s.br.Peek(1); err != nil {
		return frame{}, fmt.Errorf("homebox events: read: %w", err)
	}
	// A byte has landed, so a frame is on its way: give the REST of it a fresh
	// budget instead of whatever was left of the old one. A deadline that fires
	// between the header and the payload leaves the parser sitting inside a
	// frame, and every "frame" after that is read out of the middle of a
	// payload -- the silent desync wsframe.go exists to avoid, reintroduced one
	// layer up and invisible because the reads that follow all succeed.
	if err := s.conn.SetReadDeadline(time.Now().Add(s.idle)); err != nil {
		return frame{}, fmt.Errorf("homebox events: extend read deadline: %w", err)
	}
	f, err := readFrame(s.br, maxFramePayload)
	if err != nil {
		return frame{}, fmt.Errorf("homebox events: %w", err)
	}
	return f, nil
}

// write emits one masked frame under the write deadline.
//
// The mutex orders a pong against a concurrent Close. writeMasked does exactly
// one Write, but two of them can still interleave, and a close frame landing
// inside a pong is a framing error of our own making -- the one kind this
// package would have no excuse for.
func (s *EventStream) write(opcode byte, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("homebox events: set write deadline: %w", err)
	}
	// Checked AFTER that stamp, and the ordering is the whole of it. Close sets
	// closing and stamps its short grace on the socket before it goes for the
	// lock, so a pong that took the lock first has just overwritten the grace
	// with writeTimeout -- and Close then waits out the pong's ten seconds
	// against a peer whose receive window has shut. Measured at 11.0s, which is
	// past Docker's ten-second SIGTERM grace and so a clean shutdown answered
	// with a SIGKILL. Either this load sees the flag, or Close's own stamp
	// landed after this one and already wins.
	if s.closing.Load() {
		_ = s.conn.SetWriteDeadline(time.Now().Add(closeGraceTimeout))
	}
	return writeMasked(s.body, opcode, payload)
}

// Close ends the stream. It is safe to call from any goroutine, including one
// racing a Next that is already parked, and it is the ONLY thing that unblocks
// such a goroutine.
//
// Cancelling the context passed to Subscribe does not, and that is the trap:
// net/http hands a switched-protocol connection to the caller and clears the
// request's canceller before it delivers the 101, so the context that opened
// this socket has no way left to shut it. A watcher that relies on ctx to stop
// leaks a goroutine per reconnect, and it leaks it holding a socket.
func (s *EventStream) Close() error {
	s.closeOnce.Do(func() {
		// The short deadline goes on the connection BEFORE the write lock is
		// taken, and that ordering is the whole trick: a pong already in flight
		// against a peer whose receive window has shut holds writeMu for its
		// own writeTimeout, and stamping a nearer deadline on the socket is
		// what makes that write give up so this one can have the lock.
		//
		// The flag covers the other side of the same race -- a pong that takes
		// the lock just AFTER this stamp and replaces it with writeTimeout --
		// and is set first so that write cannot miss it and then outlast the
		// stamp below. See write.
		s.closing.Store(true)
		_ = s.conn.SetWriteDeadline(time.Now().Add(closeGraceTimeout))
		s.writeMu.Lock()
		// Best effort, and only that: the close frame is a courtesy to a server
		// that logs half-closed sockets, and it is also our reply to a close
		// frame the reader has just reported. Whether it lands changes nothing
		// about the teardown below, so its error is not the one worth
		// returning. 0x03e8 is status 1000, normal closure, big-endian as RFC
		// 6455 section 5.5.1 requires.
		_ = s.conn.SetWriteDeadline(time.Now().Add(closeGraceTimeout))
		_ = writeMasked(s.body, opClose, []byte{0x03, 0xe8})
		s.writeMu.Unlock()

		s.closeErr = s.body.Close()
		// The captured conn is the same socket the body closes on a 101, so
		// this second Close normally reports "use of closed network connection"
		// and is ignored. It is here because closing the CONNECTION is what
		// wakes the reader: a future net/http handing back a body whose Closer
		// is not the connection would otherwise leave readLoop parked until the
		// idle deadline, or forever if there is none.
		_ = s.conn.Close()
		// And waiting for it is what turns "the goroutine will exit shortly"
		// into "the goroutine and its socket are gone", which is what a
		// watcher's shutdown path needs to hear before it redials. This is
		// bounded only because deliver never blocks; see the note there.
		<-s.done
	})
	return s.closeErr
}
