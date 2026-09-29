// The cache's side of Homebox's live change feed.
//
// Before this, a container edited in the Homebox UI stayed wrong in the box
// index for a whole BOX_CACHE_TTL, and the motivating case is not a corner:
// somebody rearranges a shelf on the laptop, picks up the phone, and the
// recommendation they get is scored against what was there before.
//
// What arrives is measured, and it is almost nothing. Against a live v0.26.2
// on 2026-09-10 the mutation frame is 27 bytes whose entire content is
// {"event":"entity.mutation"} -- no id, no entity type, no operation, no
// group -- so nothing here can be an incremental update. Every event means
// exactly "go and look", and the only two things this file does about one are
// the two the request path already does: inst.invalidate() and inst.boxIndex().
// There is deliberately no second implementation of the commit protocol; an
// earlier design grew its own refreshNow() and silently lost an event whenever
// its background rebuild failed.
//
// Three things about that feed shape drive the rest of this file:
//
//  1. It fires for our OWN writes. A ten-item cataloguing session would answer
//     each of its own echoes with a full rebuild and undo the ~4.3s per item
//     patchBox exists to save, so the instance is MUTED while it writes (see
//     beginWrite in instance.go) and this checks that at both ends.
//  2. It is per-group. Measured with two live sockets in both directions: a
//     change in another account's group produced a frame on that account's
//     socket only. A shared Homebox therefore costs no spurious rebuilds.
//  3. It has no resume cursor. So the TTL is not raised because "we have push
//     now": a silent disconnect would otherwise become a permanently stale
//     index reporting stale=false.

package api

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"boxwright/internal/homebox"
)

// eventStream is the half of *homebox.EventStream this package uses, and the
// seam every policy test drives: a scripted in-process stream costs no socket,
// no goroutine timing and no port, and lets a test say "now deliver a frame
// that is our own echo" exactly.
type eventStream interface {
	Next() (homebox.Event, error)
	Close() error
}

// subscribeFunc opens one stream. Signature-compatible with homebox.Subscribe
// minus its Options, which are the watcher's tuning and not a caller's.
type subscribeFunc func(ctx context.Context, baseURL, token string) (eventStream, error)

// liveSubscribe is the real socket.
func liveSubscribe(tune watchTuning) subscribeFunc {
	return func(ctx context.Context, baseURL, token string) (eventStream, error) {
		st, err := homebox.Subscribe(ctx, baseURL, token, homebox.Options{
			HandshakeTimeout: tune.handshake,
			IdleTimeout:      tune.idle,
		})
		if err != nil {
			// Returned as an untyped nil on purpose: a (*EventStream)(nil)
			// inside this interface is NOT nil, so a caller that checks the
			// interface would go on to Close a stream that was never opened.
			return nil, err
		}
		return st, nil
	}
}

// watchTuning is every clock the watcher runs on, in one place.
//
// There are deliberately NO environment knobs for any of it. An operator has
// no basis on which to choose these numbers, each one would be another place
// for the defaults to drift, and a wrong value either suppresses foreign edits
// forever or reintroduces the per-item rebuild measured above. Tests override
// the struct wholesale, which is also the clock seam this package otherwise
// lacks.
type watchTuning struct {
	// settle coalesces a burst. Homebox fires one frame per entity, so an
	// import or a bulk edit arrives as a stream of them, and each one costs a
	// ~21s rebuild if taken at face value.
	settle time.Duration
	// minInterval is the floor between two refreshes. A secondary bound only:
	// the settler blocks for its own pre-warm, so the real rate limit is how
	// long a rebuild takes on THIS inventory rather than a number tuned
	// against an 80-location one.
	minInterval time.Duration
	// quiet is how long a fired-but-muted burst waits before asking again.
	// The mute is held by writes that are still running, so re-checking at the
	// settle window would be four wake-ups a second through a cataloguing
	// session, all reaching the same answer. It is selfWriteQuiet because that
	// is the window it is waiting out; the value lives in instance.go because
	// beginWrite is called from request handlers that have no watcher.
	quiet time.Duration
	// backoffMin and backoffMax bound reconnection. The ceiling is minutes
	// because the failure it covers -- a storage unit with no signal, a
	// Homebox being upgraded -- lasts minutes, and because a rotated API key
	// is answered by retrying at the ceiling rather than by giving up.
	backoffMin time.Duration
	backoffMax time.Duration
	// handshake and idle are homebox.Options. idle is above the 54-second
	// CONTROL ping rather than the 10-second application one: 30s would look
	// fine today and become a reconnect loop the day Homebox stops sending
	// {"event":"ping"}.
	handshake time.Duration
	idle      time.Duration
	// stableAfter is how long a session must last before the backoff resets.
	// Not "a successful dial" and not "a frame arrived": the server pings
	// every 10 seconds, so both of those reset on precisely the failure the
	// backoff exists to bound -- a socket that accepts, greets and dies.
	stableAfter time.Duration
}

func defaultWatchTuning() watchTuning {
	return watchTuning{
		settle:      750 * time.Millisecond,
		minInterval: 5 * time.Second,
		quiet:       selfWriteQuiet,
		backoffMin:  time.Second,
		backoffMax:  5 * time.Minute,
		handshake:   10 * time.Second,
		idle:        90 * time.Second,
		stableAfter: time.Minute,
	}
}

// feedState is what /healthz reports. Three actionable words and nothing else:
// no counters, no timestamps, and nothing naming a credential or a URL, because
// /healthz is exempt from the API token gate and so anything it prints is
// public.
type feedState int32

const (
	feedDisabled feedState = iota
	feedDegraded
	feedConnected
)

func (f feedState) String() string {
	switch f {
	case feedConnected:
		return "connected"
	case feedDegraded:
		return "degraded"
	default:
		return "disabled"
	}
}

// watcher is one subscription to one Homebox: the operator's own.
//
// Guest instances (ALLOW_CLIENT_HOMEBOX) stay on the TTL plus patchBox
// deliberately. A per-instance socket would turn a request-scoped SSRF surface
// into a persistent one -- a long-lived outbound connection to a host a caller
// named, held open with no request in flight and long after that caller has
// gone. The seam below would extend; this does not.
type watcher struct {
	srv   *Server
	creds credentials
	// key, never an *instance. Instances are evicted at maxInstances, and a
	// background goroutine holding a pointer would keep a cache alive that no
	// request has asked for since -- so this is resolved through
	// lookupInstance, which returns nil the moment eviction lands and creates
	// nothing.
	key       string
	tune      watchTuning
	subscribe subscribeFunc
	log       *slog.Logger

	// ctx is the watcher's OWN, not main's signal context. That is what makes
	// the shutdown ordering true: main closes this AFTER httpSrv.Shutdown
	// returns, so requests still draining keep getting cache updates.
	ctx    context.Context
	cancel context.CancelFunc

	// mutations wakes the settler. Buffered ONE and sent to without blocking:
	// the frames carry no information, so a second pending wake-up says exactly
	// what the first one already says, and a reader that could block here would
	// stop answering the control ping -- which is a disconnect about six
	// seconds later.
	mutations chan struct{}

	state atomic.Int32
	// crashed records that one of the two goroutines stopped permanently. They
	// fail independently: a settler that has panicked leaves the reader
	// reconnecting quite happily, and each of those reconnects would otherwise
	// publish "connected" for a feed whose events now reach nothing.
	crashed atomic.Bool

	readerDone  chan struct{}
	settlerDone chan struct{}
	stopOnce    sync.Once

	// The counters below are how a test tells a DROPPED event from one that
	// never arrived -- without them "nothing happened" and "the frame is still
	// in flight" are the same observation. They are also what the debug line at
	// the end of a refresh reports.
	frames    atomic.Int64 // events dispatched, whatever their name
	accepted  atomic.Int64 // foreign entity.mutations that armed the coalescer
	refreshed atomic.Int64 // refreshes STARTED, so a parked one is visible
	// attempt is the consecutive-failure count the next reconnection backs off
	// with, published for the same reason. Full jitter means a single delay is
	// consistent with any window, so the count is the only thing a test can
	// read the "reset only after a stable session" rule off.
	attempt atomic.Int64

	// settled receives one value per settler decision, including the ones that
	// refresh nothing. Read only by tests; the non-blocking send costs a
	// select when nobody is.
	settled chan struct{}
}

// StartWatching subscribes to the operator's own Homebox change feed, if there
// is one.
//
// Failure here is never fatal and never reaches main: every path degrades to
// exactly the TTL-plus-patch behaviour that came before it, which is why this
// returns nothing and takes no configuration. A storage unit with no signal
// must not become os.Exit(1).
func (s *Server) StartWatching() { s.startWatching(nil, defaultWatchTuning()) }

// startWatching is StartWatching with the two seams tests need. It returns the
// watcher, or nil when there is nothing to watch.
func (s *Server) startWatching(subscribe subscribeFunc, tune watchTuning) *watcher {
	if s.defaults.baseURL == "" {
		// No Homebox of its own: the deployment where every request brings its
		// own credentials, and the tests that exercise the cache with no
		// upstream at all. Nothing to subscribe to.
		return nil
	}
	if subscribe == nil {
		subscribe = liveSubscribe(tune)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{
		srv:         s,
		creds:       s.defaults,
		key:         s.defaults.key(),
		tune:        tune,
		subscribe:   subscribe,
		log:         s.log,
		ctx:         ctx,
		cancel:      cancel,
		mutations:   make(chan struct{}, 1),
		readerDone:  make(chan struct{}),
		settlerDone: make(chan struct{}),
		settled:     make(chan struct{}, 8),
	}
	// Degraded until the first session connects: nothing is listening yet, and
	// saying "connected" before the handshake would be a lie a probe acts on.
	w.setState(feedDegraded)
	if !s.watch.CompareAndSwap(nil, w) {
		cancel()
		return s.watch.Load()
	}
	go w.run()
	go w.settle()
	return w
}

// Close stops the change feed. Safe to call with none running.
//
// It returns an error to keep the shape callers expect of a Close; there is
// nothing here that can fail, because a watcher failing is by design a thing
// that degrades rather than a thing that is reported.
func (s *Server) Close() error {
	if w := s.watch.Load(); w != nil {
		w.stop()
	}
	return nil
}

// changeFeed is what /healthz reports.
func (s *Server) changeFeed() string {
	w := s.watch.Load()
	if w == nil {
		return feedDisabled.String()
	}
	return w.currentState().String()
}

// setState publishes what /healthz reports, and will not raise a watcher that
// has already stopped permanently back to connected: the reader outlives a
// panicking settler and keeps redialling, and each of those reconnects would
// otherwise report a feed that can no longer refresh anything as healthy.
func (w *watcher) setState(s feedState) {
	if s == feedConnected && w.crashed.Load() {
		return
	}
	w.state.Store(int32(s))
}

func (w *watcher) currentState() feedState { return feedState(w.state.Load()) }
func (w *watcher) signalSettled()          { trySend(w.settled) }
func (w *watcher) wake()                   { trySend(w.mutations) }

// stop ends both goroutines and waits for them, so that a caller which is
// shutting down knows the socket is gone rather than going.
func (w *watcher) stop() {
	w.stopOnce.Do(func() {
		w.cancel()
		<-w.readerDone
		<-w.settlerDone
		// Not degraded: nothing is trying any more, and degraded would tell a
		// probe to wait for a recovery that is not coming.
		w.setState(feedDisabled)
	})
}

// run is the dial/read/backoff supervisor.
func (w *watcher) run() {
	defer close(w.readerDone)
	defer w.recoverPanic("reader")

	attempt := 0
	for w.ctx.Err() == nil {
		startedAt := time.Now()
		err := w.session()
		if w.ctx.Err() != nil {
			return
		}
		if time.Since(startedAt) >= w.tune.stableAfter {
			attempt = 0
		}
		w.setState(feedDegraded)
		delay := w.backoffFor(err, attempt)
		attempt++
		w.attempt.Store(int64(attempt))
		w.log.Warn("homebox change feed dropped; the box index is back on its cache TTL until this reconnects",
			"err", err, "retryIn", delay)
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// session runs one connection until it ends, and returns why.
func (w *watcher) session() error {
	dialCtx, cancelDial := context.WithTimeout(w.ctx, w.tune.handshake)
	stream, err := w.subscribe(dialCtx, w.creds.baseURL, w.creds.token)
	cancelDial()
	if err != nil {
		return err
	}

	// One goroutine per SESSION, and it exits with the session so nothing
	// accumulates per reconnect. It exists because cancelling the context that
	// opened the socket does nothing to it: net/http clears a request's
	// canceller when it hands back a 101, so Close is the only thing that ends
	// a read, and something has to call it while Next is parked.
	ended := make(chan struct{})
	var closer sync.WaitGroup
	closer.Add(1)
	go func() {
		defer closer.Done()
		select {
		case <-w.ctx.Done():
		case <-ended:
		}
		_ = stream.Close()
	}()

	// NEVER invalidate here, on this connect or on any reconnect. The TTL kept
	// running through the gap, so nothing is more stale than it was -- and a
	// flapping socket that invalidated on every attempt would be a rebuild
	// loop, which is the same ~21 seconds of upstream calls over and over.
	w.setState(feedConnected)

	for {
		ev, nextErr := stream.Next()
		if nextErr != nil {
			close(ended)
			closer.Wait()
			return nextErr
		}
		w.handle(ev)
	}
}

// handle dispatches one event on its EXACT name.
//
// ping, the four other mutation feeds sharing this socket, whatever Homebox
// adds next and anything malformed enough to reach here all fall to the
// default and cost zero invalidations. Matched exactly rather than by prefix,
// so "entity.mutation " with a trailing space is a name we do not know rather
// than one we do.
func (w *watcher) handle(ev homebox.Event) {
	// Counted AFTER the decision, so that a test waiting for the frame is
	// waiting for something already acted on. Without it, "the event was
	// correctly ignored" and "the event has not arrived yet" are the same
	// observation, and a dropped event is not observable at all.
	defer w.frames.Add(1)

	if ev.Name != homebox.EventEntityMutation {
		return
	}
	// Resolved per frame rather than held: see the note on key. instMu is a
	// map lookup and is never held across a rebuild, so this cannot queue
	// behind one.
	inst := w.srv.lookupInstance(w.key)
	if inst == nil {
		return
	}
	if inst.muted(time.Now()) {
		// Our own echo. Checked HERE as well as at fire so that a cataloguing
		// session never even arms the coalescer.
		return
	}
	w.accepted.Add(1)
	w.wake()
}

// settle coalesces bursts and is the ONLY thing in this file that touches the
// cache. Strictly sequential: it blocks for the whole of its own pre-warm, so
// the next refresh cannot start until the previous rebuild has committed, on
// any size of inventory.
func (w *watcher) settle() {
	defer close(w.settlerDone)
	defer w.recoverPanic("settler")

	var deadline time.Time // zero means nothing is pending
	var lastRun time.Time

	for {
		if deadline.IsZero() {
			select {
			case <-w.ctx.Done():
				return
			case <-w.mutations:
				deadline = time.Now().Add(w.tune.settle)
			}
			continue
		}
		if wait := time.Until(deadline); wait > 0 {
			// Deliberately NOT selecting on w.mutations. Everything arriving
			// inside this window is answered by the refresh at the end of it,
			// so it belongs in the one-slot buffer -- where the drain below
			// discards it -- rather than in a wake-up that would only reach
			// the same decision again. The deadline never moves either, so a
			// Homebox import firing one frame per entity cannot postpone the
			// refresh for as long as it keeps arriving.
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}

		deadline = time.Time{}
		now := time.Now()
		switch inst := w.srv.lookupInstance(w.key); {
		case inst == nil:
			// Evicted between arming and firing. Nothing here may create it:
			// resurrecting a cache no request has asked for is what
			// lookupInstance exists to prevent.
			w.signalSettled()
		case inst.muted(now):
			// A write started after the frame arrived. Postponed, never
			// dropped -- as far as anything here can tell the event was
			// foreign, and dropping it would lose a real edit.
			deadline = now.Add(w.tune.quiet)
		case now.Sub(lastRun) < w.tune.minInterval:
			deadline = lastRun.Add(w.tune.minInterval)
		default:
			// The burst that armed this window is answered by the refresh
			// about to happen, so what it left in the buffer must not arm a
			// second one -- that would be a whole extra ~21s rebuild for one
			// edit. A frame arriving from HERE on lands in the buffer
			// afterwards and correctly arms the next window: the rebuild below
			// started before it, so what it announces may not be in what that
			// rebuild reads. TestTheSettlerSerialisesItsPreWarms is what pins
			// that second half.
			drain(w.mutations)
			lastRun = now
			w.refresh(inst)
			w.signalSettled()
		}
	}
}

// refresh is the watcher's whole effect on the cache.
func (w *watcher) refresh(inst *instance) {
	// Counted before the work, so that a refresh parked inside a rebuild is
	// distinguishable from one that has not begun.
	w.refreshed.Add(1)
	inst.invalidate()

	// invalidate() alone makes nothing fresher: it relocates a measured ~21s
	// rebuild onto the next request, which in the case this feature exists for
	// -- edit on the laptop, pick up the phone -- is the request somebody is
	// staring at. So the index is rebuilt here instead, and the result thrown
	// away: boxIndex has already committed it, and the commit is the point.
	//
	// The read-recency gate is what keeps that from becoming an idle backend
	// making unprompted upstream calls. A Homebox nobody has read within the
	// TTL is left invalidated and rebuilt by whoever asks next.
	if !inst.readRecently(w.srv.cacheTTL) {
		return
	}
	// boxIndexUnstamped, NOT boxIndex: this is not somebody reading. Going
	// through the stamping entry point re-armed the gate just checked, so once
	// one real request had opened it, foreign edits arriving faster than the
	// TTL kept an idle backend rebuilding for as long as they kept coming.
	if _, _, err := inst.boxIndexUnstamped(w.ctx, w.srv.cacheTTL); err != nil {
		// Not retried here. The index keeps its contents as a stale fallback,
		// the next request rebuilds, and a Homebox that has gone away must not
		// turn this into a retry loop against it.
		w.log.Warn("change-feed pre-warm failed; serving the cached box index until the next request",
			"err", err)
		return
	}
	w.log.Debug("box index rebuilt from a homebox change event",
		"frames", w.frames.Load(), "accepted", w.accepted.Load(), "refreshes", w.refreshed.Load())
}

// backoffFor is exponential with full jitter, and both sentinels short-circuit
// to the ceiling.
//
// ErrEventsRefused is a rotated or revoked API key and ErrEventsUnsupported is
// a Homebox with no events endpoint at this address -- a reverse proxy eating
// the Upgrade header, most often. Both are permanent for the deployment and
// neither is permanent for the process: somebody fixes the key or the proxy,
// and retrying at the ceiling means that self-heals without a restart, while
// retrying at the floor would hammer a server that has already said no.
func (w *watcher) backoffFor(err error, attempt int) time.Duration {
	window := w.tune.backoffMin
	if errors.Is(err, homebox.ErrEventsRefused) || errors.Is(err, homebox.ErrEventsUnsupported) {
		window = w.tune.backoffMax
	} else {
		// The second condition is also the overflow guard: doubling stops as
		// soon as the ceiling is passed, so a long-running process with a
		// large attempt count cannot shift its way to a negative duration.
		for i := 0; i < attempt && window < w.tune.backoffMax; i++ {
			window *= 2
		}
	}
	if window > w.tune.backoffMax {
		window = w.tune.backoffMax
	}
	if window <= 0 {
		return 0
	}
	// Full jitter over the whole window rather than a fixed delay with a
	// wobble on it: several backends restarted together must not come back in
	// step, and this is the cheapest way to say so.
	return time.Duration(rand.Int64N(int64(window)))
}

// recoverPanic stops a watcher goroutine permanently rather than taking the
// process with it. Losing the change feed costs a TTL of freshness; losing the
// process costs somebody standing in a storage unit their whole session.
func (w *watcher) recoverPanic(what string) {
	if r := recover(); r != nil {
		// Before setState, which reads it: from here on nothing may report this
		// feed as connected again, however many times the other goroutine
		// reconnects.
		w.crashed.Store(true)
		w.setState(feedDegraded)
		w.log.Error("the homebox change-feed "+what+" panicked and has stopped; the box index is "+
			"back on its cache TTL until this process restarts",
			"panic", r, "stack", string(debug.Stack()))
	}
}

// trySend delivers a wake-up unless one is already waiting. Never blocks: both
// channels it is used on are read by a goroutine that may be inside a ~21
// second rebuild, or may have stopped altogether.
func trySend(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
