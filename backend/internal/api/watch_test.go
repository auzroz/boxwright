package api

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// Everything below drives the watcher through its subscribeFunc seam, with a
// scripted in-process stream instead of a socket -- except
// TestARealSocketReachesTheCache, which wires the genuine homebox.Subscribe to
// a hijacking server so that the one place transport error shapes meet backoff
// classification is verified by running rather than by reading.
//
// Two rules hold throughout, and both are about the same failure. First,
// nothing sleeps hoping a frame was processed: every wait is waitFor on a
// counter or a receive on the settler's own signal, because a sleep long
// enough to be safe on a loaded machine is a suite nobody runs. Second, a fake
// never calls t.Errorf: its goroutine can outlive the test (httptest.Server's
// Close does not wait for a hijacked handler), and the testing package answers
// a log from there by panicking the whole binary.

const watchDeadline = 2 * time.Second

// waitFor polls until cond holds and fails LOUDLY otherwise, naming what it
// was waiting for -- a test that hangs to the package timeout tells you
// nothing about which of twenty assertions never came true.
func waitFor(t *testing.T, deadline time.Duration, what string, cond func() bool) {
	t.Helper()
	until := time.Now().Add(deadline)
	for !cond() {
		if time.Now().After(until) {
			t.Fatalf("timed out after %s waiting for %s", deadline, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// testTuning keeps every wait short enough that a stuck watcher fails inside
// the suite rather than at the package timeout. quiet is the re-arm interval,
// not the mute itself: selfWriteQuiet is a const in instance.go and stays 3
// seconds even here, which is why the muted tests observe counters instead of
// waiting the mute out.
func testTuning() watchTuning {
	return watchTuning{
		settle:      20 * time.Millisecond,
		minInterval: 5 * time.Millisecond,
		quiet:       10 * time.Millisecond,
		backoffMin:  2 * time.Millisecond,
		backoffMax:  40 * time.Millisecond,
		handshake:   time.Second,
		idle:        time.Second,
		stableAfter: time.Hour, // never resets: the flapping test counts dials
	}
}

// scriptStream is an eventStream a test pushes frames into by hand.
type scriptStream struct {
	events    chan homebox.Event
	closed    chan struct{}
	closeOnce sync.Once
	// closes counts EVERY call, not every effective one, so that a second
	// Close from a session teardown is visible rather than swallowed by the
	// Once below.
	closes atomic.Int32
}

func newScriptStream() *scriptStream {
	return &scriptStream{events: make(chan homebox.Event, 256), closed: make(chan struct{})}
}

func (s *scriptStream) push(names ...string) {
	for _, n := range names {
		s.events <- homebox.Event{Name: n}
	}
}

func (s *scriptStream) Next() (homebox.Event, error) {
	select {
	case ev := <-s.events:
		return ev, nil
	case <-s.closed:
		return homebox.Event{}, errors.New("stream closed")
	}
}

func (s *scriptStream) Close() error {
	s.closes.Add(1)
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

// feedScript is the subscribeFunc side: it counts dials, records the
// credentials each one was made with, and answers from a function the test
// supplied.
type feedScript struct {
	dials  atomic.Int32
	answer func(dial int) (eventStream, error)

	mu    sync.Mutex
	dialt []credentials
}

func (f *feedScript) subscribe() subscribeFunc {
	return func(_ context.Context, baseURL, token string) (eventStream, error) {
		n := int(f.dials.Add(1))
		f.mu.Lock()
		f.dialt = append(f.dialt, credentials{baseURL: baseURL, token: token})
		f.mu.Unlock()
		return f.answer(n)
	}
}

func (f *feedScript) dialledWith() []credentials {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]credentials{}, f.dialt...)
}

// watchRig is a cacheHomebox, a Server pointed at it, and a watcher fed by a
// scripted stream.
type watchRig struct {
	f    *cacheHomebox
	s    *Server
	h    http.Handler
	w    *watcher
	st   *scriptStream
	feed *feedScript

	// writes counts the POSTs and PUTs to /api/v1/entities the fake actually
	// served, which is how many entity.mutation frames a real Homebox would
	// have echoed back. reqs counts everything, for the assertions that say
	// no upstream call was made at all.
	writes atomic.Int32
	reqs   atomic.Int32
}

func newWatchRig(t *testing.T, ttl time.Duration, tune watchTuning) *watchRig {
	t.Helper()
	rig := &watchRig{f: newCacheHomebox(), st: newScriptStream()}
	rig.start(t, ttl, tune)
	return rig
}

func (rig *watchRig) start(t *testing.T, ttl time.Duration, tune watchTuning) {
	t.Helper()
	inner := rig.f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.reqs.Add(1)
		if strings.HasPrefix(r.URL.Path, "/api/v1/entities") &&
			(r.Method == http.MethodPost || r.Method == http.MethodPut) {
			rig.writes.Add(1)
		}
		inner(w, r)
	}))
	t.Cleanup(srv.Close)

	rig.s = New(homebox.New(srv.URL+"/api", "tok"), mustProvider(t), ttl, discardLogger())
	rig.h = rig.s.Routes()
	rig.feed = &feedScript{answer: func(int) (eventStream, error) { return rig.st, nil }}
	rig.w = rig.s.startWatching(rig.feed.subscribe(), tune)
	if rig.w == nil {
		t.Fatal("startWatching returned nothing for a server that has a Homebox")
	}
	// Registered after the httptest cleanup so it runs BEFORE it: the watcher
	// must be stopped while its upstream still answers.
	t.Cleanup(func() { rig.s.Close() })
}

// newWatcherOnly is the rig for the tests that never reach Homebox at all --
// dialling, backoff and health. The index is never read, so the pre-warm gate
// declines every refresh and the URL below is never fetched.
func newWatcherOnly(t *testing.T, tune watchTuning, answer func(dial int) (eventStream, error)) (*Server, *watcher, *feedScript) {
	t.Helper()
	s := New(homebox.New("http://homebox.invalid/api", "tok"), nil, time.Hour, discardLogger())
	feed := &feedScript{answer: answer}
	w := s.startWatching(feed.subscribe(), tune)
	if w == nil {
		t.Fatal("startWatching returned nothing for a server that has a Homebox")
	}
	t.Cleanup(func() { s.Close() })
	return s, w, feed
}

func (r *watchRig) inst() *instance { return r.s.defaultInstance() }

func generationOf(inst *instance) uint64 {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return inst.generation
}

func cachedBox(inst *instance, id string) (placement.Box, bool) {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return findBox(inst.boxes, id)
}

// awaitSettled waits for one settler decision, refreshing or not.
func awaitSettled(t *testing.T, w *watcher, what string) {
	t.Helper()
	select {
	case <-w.settled:
	case <-time.After(watchDeadline):
		t.Fatalf("the settler reached no decision within %s: %s", watchDeadline, what)
	}
}

// drainSettled clears decisions an earlier phase of the test caused, so that
// the next awaitSettled is waiting for the one it means.
func drainSettled(w *watcher) { drain(w.settled) }

func fetchedAt(inst *instance) time.Time {
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return inst.fetched
}

// THE load-bearing one. Homebox's change feed fires on our OWN writes, and the
// frame says nothing but "something changed" -- so a watcher that believed it
// would answer every entity a cataloguing session files with the full rebuild
// patchBox exists to avoid, at a measured ~4.3s per item.
//
// The frames counter is what makes that observable at all: without it, "the
// echo was correctly ignored" and "the echo has not arrived yet" are the same
// reading, and the test would pass against a watcher that never got the frame.
func TestCataloguingIsUndisturbedByItsOwnEchoes(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())
	inst := rig.inst()

	if code, boxes, _ := getBoxes(rig.h); code != http.StatusOK || len(containers(boxes)) != 2 {
		t.Fatalf("warm-up: status %d, %d containers, want 200 and the two totes", code, len(containers(boxes)))
	}
	if rig.f.indexReads.Load() == 0 {
		t.Fatal("the warm-up made no upstream calls, so this test proves nothing")
	}
	waitFor(t, watchDeadline, "the feed to connect", func() bool {
		return rig.w.currentState() == feedConnected
	})

	writesBefore := rig.writes.Load()
	framesBefore := rig.w.frames.Load()

	if code := postCatalog(rig.h, catalogEntry{
		Item:  placement.ItemDraft{Name: "socket adapter", Category: "tools", SizeBucket: "S", Quantity: 3},
		BoxID: "fresno",
	}); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}

	// One frame per entity write the fake actually served: that is what the
	// live server echoes, item writes included.
	echoes := int(rig.writes.Load() - writesBefore)
	if echoes == 0 {
		t.Fatal("the catalog wrote nothing upstream, so there are no echoes to suppress")
	}
	for i := 0; i < echoes; i++ {
		rig.st.push(homebox.EventEntityMutation)
	}
	waitFor(t, watchDeadline, fmt.Sprintf("all %d echoes to be dispatched", echoes), func() bool {
		return rig.w.frames.Load()-framesBefore >= int64(echoes)
	})

	if n := rig.w.accepted.Load(); n != 0 {
		t.Errorf("%d of our own echoes were taken for foreign edits", n)
	}
	if n := rig.w.refreshed.Load(); n != 0 {
		t.Fatalf("the watcher invalidated %d times for our own writes", n)
	}
	if fetchedAt(inst).IsZero() {
		t.Fatal("fetched is zero: the index was invalidated by an echo of our own write")
	}

	before := rig.f.indexReads.Load()
	code, rec := postRecommend(rig.h, placement.ItemDraft{
		Name: "socket set", Category: "tools", SizeBucket: "M", Quantity: 1,
	})
	if code != http.StatusOK {
		t.Fatalf("recommend status %d, want 200", code)
	}
	if spent := rig.f.indexReads.Load() - before; spent != 0 {
		t.Errorf("the recommendation after a catalog spent %d upstream index calls, want 0", spent)
	}
	fresno, ok := candidateBox(rec, "fresno")
	if !ok {
		t.Fatalf("Fresno is not a candidate after three tools were filed there: %+v", rec)
	}
	// The patch's own figures, not a rebuild's.
	if fresno.Categories["tools"] != 3 || fresno.ItemCount != 3 {
		t.Errorf("cached Fresno = categories %v, itemCount %d; want tools:3 and 3 items",
			fresno.Categories, fresno.ItemCount)
	}
}

// The same thing at session length. Ten items is an ordinary shelf, and every
// one of them echoes.
func TestFilingTenItemsWithTheFeedLiveSpendsNoIndexCalls(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	waitFor(t, watchDeadline, "the feed to connect", func() bool {
		return rig.w.currentState() == feedConnected
	})

	readsBefore := rig.f.indexReads.Load()
	const items = 10
	for i := 0; i < items; i++ {
		writesBefore := rig.writes.Load()
		if code := postCatalog(rig.h, catalogEntry{
			Item: placement.ItemDraft{
				Name: fmt.Sprintf("drill bit %d", i), Category: "tools", SizeBucket: "S", Quantity: 1,
			},
			BoxID: "fresno",
		}); code != http.StatusCreated {
			t.Fatalf("item %d: catalog status %d, want 201", i, code)
		}
		for n := int32(0); n < rig.writes.Load()-writesBefore; n++ {
			rig.st.push(homebox.EventEntityMutation)
		}
	}
	waitFor(t, watchDeadline, "every echo of the session to be dispatched", func() bool {
		return rig.w.frames.Load() >= int64(rig.writes.Load())
	})

	if spent := rig.f.indexReads.Load() - readsBefore; spent != 0 {
		t.Errorf("a ten-item session spent %d index calls, want 0", spent)
	}
	before := rig.f.indexReads.Load()
	if code, _ := postRecommend(rig.h, placement.ItemDraft{
		Name: "socket set", Category: "tools", SizeBucket: "M",
	}); code != http.StatusOK {
		t.Fatalf("recommend status %d", code)
	}
	if spent := rig.f.indexReads.Load() - before; spent != 0 {
		t.Errorf("the recommendation after the session spent %d index calls, want 0", spent)
	}
	fresno, ok := cachedBox(rig.inst(), "fresno")
	if !ok {
		t.Fatalf("Fresno left the cache")
	}
	if fresno.Categories["tools"] != items {
		t.Errorf("cached Fresno = %v, want tools:%d -- every item was patched in", fresno.Categories, items)
	}
}

// Only the exact name invalidates. Everything else on this socket -- the
// 10-second ping, the four other mutation feeds, whatever Homebox adds next --
// costs nothing, and neither does a payload that never parsed.
//
// Each case is followed by a sentinel event that MUST be acted on, so a
// watcher that ignored everything would fail here just as loudly as one that
// invalidated on everything.
func TestOnlyAnExactEntityMutationInvalidates(t *testing.T) {
	tests := []struct {
		name   string
		events []string
	}{
		{"the 10-second application ping", []string{homebox.EventPing, homebox.EventPing, homebox.EventPing}},
		{"tags.mutation", []string{homebox.EventTagsMutation}},
		{"user.mutation", []string{homebox.EventUserMutation}},
		{"export.mutation", []string{homebox.EventExportMutation}},
		{"import.mutation", []string{homebox.EventImportMutation}},
		{"a name this version has never heard of", []string{"entity.reindexed"}},
		{"an empty name", []string{""}},
		{"a trailing space", []string{"entity.mutation "}},
		// The real stream drops an unparseable payload before Next ever sees
		// it (measured in events_test.go), so this is the belt-and-braces
		// case: even handed one, the dispatcher must let it fall through.
		{"a payload that never parsed", []string{`{"event":`, "<html>502 Bad Gateway</html>"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWatchRig(t, time.Hour, testTuning())
			drainSettled(rig.w)

			rig.st.push(tc.events...)
			rig.st.push(homebox.EventEntityMutation)
			awaitSettled(t, rig.w, "the sentinel event")

			if got := rig.w.accepted.Load(); got != 1 {
				t.Errorf("accepted %d events as foreign edits, want only the sentinel", got)
			}
			if got := rig.w.refreshed.Load(); got != 1 {
				t.Errorf("ran %d refreshes, want exactly 1", got)
			}
			if got := generationOf(rig.inst()); got != 1 {
				t.Errorf("generation = %d, want 1 invalidation", got)
			}
			// And the socket is still up: dropping it over an event we do not
			// understand would turn "Boxwright has not heard of that one" into
			// "Boxwright stopped noticing changes at all".
			if dials := rig.feed.dials.Load(); dials != 1 {
				t.Errorf("%d dials, want 1: the connection was dropped over the event", dials)
			}
			if state := rig.w.currentState(); state != feedConnected {
				t.Errorf("feed state = %v, want connected", state)
			}
		})
	}
}

// A foreign edit does both halves. invalidate() alone makes nothing fresher:
// it relocates the measured ~21s rebuild onto the next request, which in the
// case this feature exists for -- edit on the laptop, pick up the phone -- is
// the request somebody is waiting on.
func TestAForeignMutationInvalidatesAndPreWarms(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	drainSettled(rig.w)
	readsBefore := rig.f.indexReads.Load()

	rig.st.push(homebox.EventEntityMutation)
	awaitSettled(t, rig.w, "the foreign edit")

	if spent := rig.f.indexReads.Load() - readsBefore; spent == 0 {
		t.Fatal("the watcher invalidated and stopped there, so the rebuild is now owed by the " +
			"next request instead of being done while nobody was waiting")
	}

	before := rig.f.indexReads.Load()
	code, boxes, stale := getBoxes(rig.h)
	if code != http.StatusOK {
		t.Fatalf("boxes status %d, want 200", code)
	}
	if spent := rig.f.indexReads.Load() - before; spent != 0 {
		t.Errorf("the read after the pre-warm spent %d index calls, want 0", spent)
	}
	if stale {
		t.Error("stale = true straight after a pre-warm; the rebuild did not commit")
	}
	if n := len(containers(boxes)); n != 2 {
		t.Errorf("served %d containers after the pre-warm, want 2", n)
	}
}

// The gate in front of the pre-warm. An idle backend must make no unprompted
// upstream calls: a Homebox nobody has read within the TTL is marked due and
// left for whoever asks next.
func TestAnUnreadInstanceIsInvalidatedButNotPreWarmed(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())
	inst := rig.inst()

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	// Nobody has touched this Homebox for longer than the TTL.
	inst.lastRead.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	drainSettled(rig.w)
	genBefore := generationOf(inst)
	readsBefore := rig.f.indexReads.Load()

	rig.st.push(homebox.EventEntityMutation)
	awaitSettled(t, rig.w, "the event on an idle instance")

	if spent := rig.f.indexReads.Load() - readsBefore; spent != 0 {
		t.Errorf("an idle backend spent %d upstream calls on an event nobody asked about, want 0", spent)
	}
	if got := generationOf(inst) - genBefore; got != 1 {
		t.Errorf("generation moved by %d, want 1: the edit still has to invalidate", got)
	}
	if !fetchedAt(inst).IsZero() {
		t.Error("fetched is not zero, so the next request would serve the pre-edit index for a whole TTL")
	}
}

// The other side of that gate, and the side that was wrong: the pre-warm must
// not count as a read of its own. Reaching the cache through boxIndex stamped
// lastRead on every rebuild, so ONE genuine request opened the gate and every
// foreign edit arriving within a TTL of the previous pre-warm held it open --
// an idle backend paying a ~21s rebuild over and over for a client that had
// gone hours before.
func TestThePreWarmDoesNotRearmItsOwnGate(t *testing.T) {
	const ttl = 600 * time.Millisecond
	rig := newWatchRig(t, ttl, testTuning())
	inst := rig.inst()

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	// Somebody read this Homebox 350ms ago: the gate is open, and closes 250ms
	// from now. Stamped by hand rather than slept out, so the two waits below
	// are the only clock this test spends.
	inst.lastRead.Store(time.Now().Add(-ttl + 250*time.Millisecond).UnixNano())
	drainSettled(rig.w)

	stampBefore := inst.lastRead.Load()
	readsBefore := rig.f.indexReads.Load()
	rig.st.push(homebox.EventEntityMutation)
	awaitSettled(t, rig.w, "the edit that lands while somebody is still working")
	if spent := rig.f.indexReads.Load() - readsBefore; spent == 0 {
		t.Fatal("the first event did not pre-warm at all, so this test proves nothing")
	}
	if got := inst.lastRead.Load(); got != stampBefore {
		t.Errorf("the pre-warm moved lastRead forward by %v; it re-armed the gate it is gated on",
			time.Duration(got-stampBefore))
	}

	// Past the last REQUEST's window, and comfortably inside a TTL of the
	// pre-warm above -- which is exactly the gap the bug lived in.
	time.Sleep(450 * time.Millisecond)

	readsBefore = rig.f.indexReads.Load()
	genBefore := generationOf(inst)
	rig.st.push(homebox.EventEntityMutation)
	awaitSettled(t, rig.w, "the edit that lands after everyone has gone")

	if spent := rig.f.indexReads.Load() - readsBefore; spent != 0 {
		t.Errorf("an idle backend spent %d upstream calls on an edit made %v after the last "+
			"request, with a %v TTL", spent, ttl-250*time.Millisecond+450*time.Millisecond, ttl)
	}
	if got := generationOf(inst) - genBefore; got != 1 {
		t.Errorf("generation moved by %d, want 1: the edit still has to invalidate", got)
	}
}

// An event arriving while the pre-warm it triggered is still running is not
// swallowed by it: the rebuild started before the change, so what the change
// announces may not be in what it read.
func TestAnEventDuringAnInFlightRebuildIsNotLost(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())
	inst := rig.inst()

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	drainSettled(rig.w)

	// Park the pre-warm on its first location detail, AFTER its listing has
	// been served -- so a location added while it is parked is invisible to
	// this rebuild and visible to the next.
	release := rig.f.armGate()
	rig.st.push(homebox.EventEntityMutation)
	waitFor(t, watchDeadline, "the pre-warm to reach the gate", func() bool {
		return rig.f.gateHits.Load() >= 1
	})

	rig.f.mu.Lock()
	rig.f.locs = append(rig.f.locs, loc("stockton", "Stockton", "california", "California"))
	rig.f.fields["stockton"] = []any{chosen()}
	rig.f.mu.Unlock()

	rig.st.push(homebox.EventEntityMutation)
	waitFor(t, watchDeadline, "the second event to be accepted", func() bool {
		return rig.w.accepted.Load() >= 2
	})
	close(release)

	waitFor(t, watchDeadline, "the second refresh to run", func() bool {
		return rig.w.refreshed.Load() >= 2
	})
	waitFor(t, watchDeadline, "the new location to reach the cache", func() bool {
		_, ok := cachedBox(inst, "stockton")
		return ok
	})
}

// Strictly sequential. The settler blocks for the whole of its own pre-warm,
// which is what makes the refresh rate self-calibrating -- a floor measured in
// how long a rebuild takes on THIS inventory rather than a constant tuned
// against somebody's eighty locations.
func TestTheSettlerSerialisesItsPreWarms(t *testing.T) {
	tune := testTuning()
	rig := newWatchRig(t, time.Hour, tune)

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	drainSettled(rig.w)

	release := rig.f.armGate()
	rig.st.push(homebox.EventEntityMutation)
	waitFor(t, watchDeadline, "the first pre-warm to reach the gate", func() bool {
		return rig.f.gateHits.Load() >= 1
	})

	// A second burst while the first rebuild is parked. Every frame is
	// dispatched and accepted, and none of them may start anything.
	for i := 0; i < 5; i++ {
		rig.st.push(homebox.EventEntityMutation)
	}
	waitFor(t, watchDeadline, "the second burst to be accepted", func() bool {
		return rig.w.accepted.Load() >= 6
	})
	if got := rig.w.refreshed.Load(); got != 1 {
		t.Fatalf("%d refreshes have started while one is still parked in its rebuild, want 1", got)
	}

	close(release)
	awaitSettled(t, rig.w, "the parked pre-warm to commit")
	awaitSettled(t, rig.w, "the second burst's refresh")
	if got := rig.w.refreshed.Load(); got != 2 {
		t.Errorf("%d refreshes, want 2: the five frames of the second burst are one burst", got)
	}
	// And nothing else follows: the burst coalesced rather than queued.
	select {
	case <-rig.w.settled:
		t.Errorf("a third refresh ran; the second burst was answered more than once")
	case <-time.After(4 * tune.settle):
	}
}

// A burst is one rebuild. Homebox fires a frame per entity, so an import or a
// bulk edit in the UI arrives as a stream of them -- fifty rebuilds would be
// most of an hour of upstream calls for one change.
func TestABurstCoalescesToOneRebuild(t *testing.T) {
	tune := testTuning()
	tune.settle = 60 * time.Millisecond // comfortably longer than 50 channel sends
	rig := newWatchRig(t, time.Hour, tune)
	inst := rig.inst()
	drainSettled(rig.w)

	const burst = 50
	for i := 0; i < burst; i++ {
		rig.st.push(homebox.EventEntityMutation)
	}
	waitFor(t, watchDeadline, "the whole burst to be accepted", func() bool {
		return rig.w.accepted.Load() >= burst
	})
	awaitSettled(t, rig.w, "the burst's refresh")
	// And nothing follows it. The frames that arrived while the window was
	// running are answered by the refresh it armed, so a second one here would
	// be another whole ~21s rebuild for the same edit.
	select {
	case <-rig.w.settled:
		t.Error("a second refresh followed the burst; the frames it left behind armed another window")
	case <-time.After(4 * tune.settle):
	}

	if got := rig.w.refreshed.Load(); got != 1 {
		t.Errorf("%d refreshes for one burst of %d frames, want 1", got, burst)
	}
	if got := generationOf(inst); got != 1 {
		t.Errorf("generation = %d after a burst of %d, want 1", got, burst)
	}
}

// The floor between two refreshes, which every other test in this file runs
// with a settle window wider than -- so nothing exercised it. It is a secondary
// bound (the settler blocking on its own pre-warm is the real rate limit), but
// it decides the same thing the mute does at fire time: an event that arrives
// too soon is POSTPONED, never dropped. Dropping one loses a real edit for a
// whole TTL.
func TestTheRateFloorPostponesRatherThanDrops(t *testing.T) {
	tune := testTuning()
	tune.settle = 5 * time.Millisecond
	tune.minInterval = 200 * time.Millisecond
	rig := newWatchRig(t, time.Hour, tune)
	inst := rig.inst()

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	drainSettled(rig.w)

	rig.st.push(homebox.EventEntityMutation)
	awaitSettled(t, rig.w, "the first edit")
	if got := rig.w.refreshed.Load(); got != 1 {
		t.Fatalf("%d refreshes for the first edit, want 1", got)
	}

	// A second edit hard on its heels, well inside the floor.
	rig.st.push(homebox.EventEntityMutation)
	select {
	case <-rig.w.settled:
		t.Fatal("the second edit was answered immediately; there is no floor between refreshes")
	case <-time.After(tune.minInterval / 2):
	}

	awaitSettled(t, rig.w, "the postponed second edit")
	if got := rig.w.refreshed.Load(); got != 2 {
		t.Errorf("%d refreshes, want 2: the edit inside the floor was dropped, not postponed", got)
	}
	if got := generationOf(inst); got != 2 {
		t.Errorf("generation = %d, want 2 invalidations", got)
	}
}

// The offline case, which is the one this product is built for. An event
// arrives, the pre-warm it triggers cannot reach Homebox, and /boxes still
// answers from the cache rather than 502-ing at somebody in a storage unit.
func TestAFailedPreWarmStillServesTheCachedIndex(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())

	if code, boxes, _ := getBoxes(rig.h); code != http.StatusOK || len(containers(boxes)) != 2 {
		t.Fatalf("warm-up: status %d, %d containers", code, len(containers(boxes)))
	}
	drainSettled(rig.w)
	rig.f.goOffline()

	rig.st.push(homebox.EventEntityMutation)
	awaitSettled(t, rig.w, "the refresh against an unreachable Homebox")

	code, boxes, stale := getBoxes(rig.h)
	if code != http.StatusOK || !stale {
		t.Fatalf("status %d stale %v, want 200 and stale=true", code, stale)
	}
	if n := len(containers(boxes)); n != 2 {
		t.Errorf("served %d containers offline, want the 2 the cache is holding", n)
	}
}

// One backend, two Homeboxes, one socket. The feed is per-group -- measured
// with two live sockets in both directions on 2026-09-10 -- and the watcher
// subscribes to the operator's own Homebox only, so an event on it must not
// throw away anybody else's index.
func TestAnEventOnOneHomeboxLeavesTheOtherWarm(t *testing.T) {
	f := &twoHomeboxes{}
	s, base := multiTenantServer(t, f)
	h := s.Routes()
	st := newScriptStream()
	feed := &feedScript{answer: func(int) (eventStream, error) { return st, nil }}
	w := s.startWatching(feed.subscribe(), testTuning())
	if w == nil {
		t.Fatal("startWatching returned nothing")
	}
	t.Cleanup(func() { s.Close() })

	if got := boxesAs(t, h, "", ""); len(got) != 1 || got[0].Name != "anon's box" {
		t.Fatalf("the operator got %+v", got)
	}
	if got := boxesAs(t, h, base, "bob-key"); len(got) != 1 || got[0].Name != "bob's box" {
		t.Fatalf("bob got %+v", got)
	}
	drainSettled(w)

	before := f.calls.Load()
	st.push(homebox.EventEntityMutation)
	awaitSettled(t, w, "the event on the operator's Homebox")
	if f.calls.Load() == before {
		t.Fatal("the event cost no upstream calls at all, so nothing was refreshed and this " +
			"test cannot tell the two instances apart")
	}

	before = f.calls.Load()
	if got := boxesAs(t, h, base, "bob-key"); len(got) != 1 || got[0].Name != "bob's box" {
		t.Fatalf("bob got %+v after an event on the operator's feed", got)
	}
	if spent := f.calls.Load() - before; spent != 0 {
		t.Errorf("bob's read spent %d upstream calls after an event on somebody else's Homebox", spent)
	}
	before = f.calls.Load()
	if got := boxesAs(t, h, "", ""); len(got) != 1 {
		t.Fatalf("the operator got %+v", got)
	}
	if spent := f.calls.Load() - before; spent != 0 {
		t.Errorf("the operator's own read spent %d calls, want 0: the pre-warm did not commit", spent)
	}
}

// The watcher holds a key, not a pointer, precisely so that this is a no-op.
// Resurrecting an evicted instance would rebuild a cache no request has asked
// for since -- and would defeat maxInstances, since the resurrected entry is
// one nothing is going to evict for another sixty-four callers.
func TestAnEvictedInstanceIsNotResurrected(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())
	key := rig.s.defaults.key()

	for i := 0; i < maxInstances; i++ {
		rig.s.instanceFrom(credentials{baseURL: "http://elsewhere.example/api", token: fmt.Sprintf("t%d", i)})
	}
	if rig.s.lookupInstance(key) != nil {
		t.Fatal("the operator's instance survived sixty-four other callers, so nothing was evicted")
	}
	rig.s.instMu.Lock()
	sizeBefore := len(rig.s.instances)
	rig.s.instMu.Unlock()
	reqsBefore := rig.reqs.Load()
	framesBefore := rig.w.frames.Load()

	rig.st.push(homebox.EventEntityMutation)
	waitFor(t, watchDeadline, "the event to be dispatched", func() bool {
		return rig.w.frames.Load() > framesBefore
	})

	rig.s.instMu.Lock()
	sizeAfter := len(rig.s.instances)
	rig.s.instMu.Unlock()
	if sizeAfter != sizeBefore {
		t.Errorf("the instance map went from %d to %d entries: the watcher rebuilt an evicted cache",
			sizeBefore, sizeAfter)
	}
	if spent := rig.reqs.Load() - reqsBefore; spent != 0 {
		t.Errorf("%d upstream calls followed an event for an evicted instance, want 0", spent)
	}
	if got := rig.w.refreshed.Load(); got != 0 {
		t.Errorf("%d refreshes ran for an instance that no longer exists", got)
	}
}

// Reconnecting is not a reason to invalidate. The TTL kept running through the
// gap, so nothing became more stale while the socket was down -- and a
// flapping socket that invalidated on every attempt would be a rebuild loop,
// which is the same ~21 seconds of upstream calls over and over.
func TestReconnectingNeverInvalidatesAndBacksOff(t *testing.T) {
	_, w, feed := newWatcherOnly(t, testTuning(), func(int) (eventStream, error) {
		// Upgrades, then hangs up immediately: the shape a proxy in front of
		// Homebox produces, and the one that "reset the backoff on a
		// successful dial" would mistake for progress.
		st := newScriptStream()
		st.Close()
		return st, nil
	})

	// A fixed window, because what is being measured is a RATE. Linear retries
	// would be thousands of dials in this window; doubling from 2ms with a 40ms
	// ceiling is a couple of dozen at most.
	time.Sleep(200 * time.Millisecond)

	dials := feed.dials.Load()
	if dials < 2 {
		t.Fatalf("%d dials in 200ms: the watcher gave up rather than reconnecting", dials)
	}
	if dials > 40 {
		t.Errorf("%d dials in 200ms with a 2ms floor and a 40ms ceiling: the backoff is not "+
			"doubling, so a Homebox that is down is being hammered", dials)
	}
	if got := w.refreshed.Load(); got != 0 {
		t.Errorf("%d invalidations from reconnecting alone, want 0", got)
	}
	if got := generationOf(newTestInstanceOf(t, w)); got != 0 {
		t.Errorf("generation = %d after a flapping socket, want 0", got)
	}
}

// newTestInstanceOf resolves the instance a watcher is watching, so a test can
// read the cache state it would have touched.
func newTestInstanceOf(t *testing.T, w *watcher) *instance {
	t.Helper()
	inst := w.srv.lookupInstance(w.key)
	if inst == nil {
		t.Fatal("the watcher's instance is gone")
	}
	return inst
}

// A rotated API key and a Homebox with no events endpoint are permanent for
// the deployment and temporary for the process: somebody fixes the key or the
// reverse proxy, and this has to notice without a restart. So the sentinels go
// to the CEILING rather than stopping forever.
func TestARefusedSubscriptionBacksOffToTheCeilingAndStillRecovers(t *testing.T) {
	tune := testTuning()
	live := newScriptStream()
	_, w, feed := newWatcherOnly(t, tune, func(dial int) (eventStream, error) {
		if dial <= 2 {
			return nil, fmt.Errorf("homebox subscribe: status 401: %w", homebox.ErrEventsRefused)
		}
		return live, nil
	})

	waitFor(t, watchDeadline, "the watcher to reconnect after being refused twice", func() bool {
		return w.currentState() == feedConnected
	})
	if got := feed.dials.Load(); got < 3 {
		t.Fatalf("%d dials, want at least 3", got)
	}

	drainSettled(w)
	live.push(homebox.EventEntityMutation)
	awaitSettled(t, w, "an event on the recovered socket")
	if got := generationOf(newTestInstanceOf(t, w)); got != 1 {
		t.Errorf("generation = %d after recovery, want 1: the recovered socket delivers nothing", got)
	}
}

// The reset half of the backoff rule, which nothing used to pin: the flapping
// test above sets stableAfter to an hour so it can count dials, so every test
// in this file ran with a reset that could never happen.
//
// It resets only after a session has SURVIVED for a while -- not on a
// successful dial and not on "a frame arrived", because Homebox pings every ten
// seconds and both of those reset on precisely the failure the backoff exists
// to bound. Without a reset, a backend that is up for months answers the first
// edit after any blip minutes later, having climbed to the ceiling one Wi-Fi
// drop at a time.
//
// The attempt count is what a test can read that off. Full jitter means any
// single delay is consistent with any window, so the delays themselves prove
// nothing.
func TestASessionThatSurvivesResetsTheBackoff(t *testing.T) {
	tune := testTuning()
	tune.stableAfter = 150 * time.Millisecond

	const flaps = 3
	_, w, _ := newWatcherOnly(t, tune, func(dial int) (eventStream, error) {
		st := newScriptStream()
		if dial <= flaps {
			// Accepts, greets and hangs up, well inside stableAfter.
			st.Close()
			return st, nil
		}
		// This one lives long enough to count as stable, and then dies too.
		time.AfterFunc(2*tune.stableAfter, func() { st.Close() })
		return st, nil
	})

	waitFor(t, watchDeadline, "three instant failures to climb the backoff", func() bool {
		return w.attempt.Load() >= flaps
	})
	// Back to one: the session that survived cleared the count, and the failure
	// that ended it is the first of a new run rather than the fourth of the old.
	waitFor(t, watchDeadline, "the surviving session to reset the backoff", func() bool {
		return w.attempt.Load() == 1
	})
}

func TestBackoffFor(t *testing.T) {
	w := &watcher{tune: watchTuning{backoffMin: time.Second, backoffMax: 5 * time.Minute}}

	tests := []struct {
		name    string
		err     error
		attempt int
		max     time.Duration
	}{
		{name: "the first failure draws from the floor window", attempt: 0, max: time.Second},
		{name: "the fourth doubles three times", attempt: 3, max: 8 * time.Second},
		// The overflow case. Doubling an int64 duration 100 times is a
		// negative delay and a select that returns instantly, which is the
		// hot loop the backoff exists to prevent.
		{name: "an attempt count that would overflow saturates", attempt: 100, max: 5 * time.Minute},
		{name: "a refused subscription goes straight to the ceiling", err: homebox.ErrEventsRefused, max: 5 * time.Minute},
		{name: "an absent endpoint goes straight to the ceiling", err: homebox.ErrEventsUnsupported, max: 5 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Full jitter means a single draw proves nothing about the window
			// it came from, so the window is measured across many.
			var high time.Duration
			for i := 0; i < 500; i++ {
				got := w.backoffFor(tc.err, tc.attempt)
				if got < 0 || got > tc.max {
					t.Fatalf("backoffFor = %v, outside [0, %v]", got, tc.max)
				}
				if got > high {
					high = got
				}
			}
			// Half the window in 500 uniform draws is a certainty; anything
			// less means the delay is not drawn from the window claimed.
			if high < tc.max/2 {
				t.Errorf("the largest of 500 draws was %v, want something near %v", high, tc.max)
			}
		})
	}
}

// The sentinels are wrapped errors from the homebox package, so this pins that
// the classification is by errors.Is rather than by string matching.
func TestBackoffForClassifiesWrappedSentinels(t *testing.T) {
	w := &watcher{tune: watchTuning{backoffMin: time.Millisecond, backoffMax: time.Second}}
	wrapped := fmt.Errorf("session 3: %w", fmt.Errorf("homebox subscribe: status 403: %w", homebox.ErrEventsRefused))

	var high time.Duration
	for i := 0; i < 500; i++ {
		if got := w.backoffFor(wrapped, 0); got > high {
			high = got
		}
	}
	if high < 500*time.Millisecond {
		t.Errorf("a doubly wrapped ErrEventsRefused drew at most %v, so it was treated as an "+
			"ordinary failure and retried at the floor", high)
	}
}

// The TTL is not made conditional on the socket. This feed has no resume
// cursor, so "we have push now, raise the TTL" turns one silent disconnect
// into a permanently stale index reporting stale=false.
func TestTheTTLStillRefreshesOnASilentSocket(t *testing.T) {
	rig := newWatchRig(t, 40*time.Millisecond, testTuning())

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	waitFor(t, watchDeadline, "the feed to connect", func() bool {
		return rig.w.currentState() == feedConnected
	})

	before := rig.f.indexReads.Load()
	// The TTL is the thing under test, so this waits it out rather than
	// polling for something another goroutine is about to do.
	time.Sleep(80 * time.Millisecond)

	if _, _, _ = getBoxes(rig.h); rig.f.indexReads.Load() == before {
		t.Error("no refresh past the TTL with the socket connected; a missed event would leave " +
			"the index wrong forever")
	}
	if got := rig.w.refreshed.Load(); got != 0 {
		t.Errorf("the watcher ran %d refreshes on a silent socket", got)
	}
}

// Shutdown. Both goroutines are accounted for by their own done channels
// rather than by runtime.NumGoroutine, which counts the test binary's and
// tells you nothing about whose.
func TestCloseStopsBothGoroutinesAndTheStream(t *testing.T) {
	rig := newWatchRig(t, time.Hour, testTuning())
	waitFor(t, watchDeadline, "the feed to connect", func() bool {
		return rig.w.currentState() == feedConnected
	})

	done := make(chan error, 1)
	go func() { done <- rig.s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(watchDeadline):
		t.Fatalf("Close did not return within %s; a shutdown path that waits on a parked read "+
			"holds the process open for its idle timeout", watchDeadline)
	}

	if !closed(rig.w.readerDone) {
		t.Error("the reader goroutine is still running after Close returned")
	}
	if !closed(rig.w.settlerDone) {
		t.Error("the settler goroutine is still running after Close returned")
	}
	if got := rig.st.closes.Load(); got != 1 {
		t.Errorf("the stream was closed %d times, want exactly 1", got)
	}
	if got := rig.s.changeFeed(); got != "disabled" {
		t.Errorf("changeFeed = %q after Close, want disabled", got)
	}
	// The deferred cleanup calls this again, so a second Close must be a no-op
	// rather than a second wait on channels that are already closed.
	if err := rig.s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A panic in a watcher goroutine costs a TTL of freshness. Taking the process
// with it costs somebody standing in a storage unit their whole session, so it
// is recovered, logged with its stack, and stopped permanently.
func TestAPanickingReaderDegradesRatherThanCrashing(t *testing.T) {
	s, w, _ := newWatcherOnly(t, testTuning(), func(int) (eventStream, error) {
		panic("the dial exploded")
	})

	waitFor(t, watchDeadline, "the reader to stop", func() bool { return closed(w.readerDone) })
	if got := w.currentState(); got != feedDegraded {
		t.Errorf("feed state = %v after a panic, want degraded", got)
	}
	// And the rest of the process is still shutdownable.
	done := make(chan struct{})
	go func() { defer close(done); s.Close() }()
	select {
	case <-done:
	case <-time.After(watchDeadline):
		t.Fatal("Close hung after a goroutine panicked")
	}
}

// The settler's half of the same contract, and the half the reader cannot
// stand in for: the two goroutines fail independently, so a settler that has
// died leaves a reader happily reconnecting to a feed whose events nothing can
// act on any more. Those reconnects must not report themselves as healthy.
//
// The panic is manufactured -- an instance whose client is gone -- because what
// is pinned here is the recover and what follows it, not any particular way of
// reaching it. It is still the real path: refresh, boxIndexUnstamped,
// fetchBoxes.
func TestAPanickingSettlerDegradesForGood(t *testing.T) {
	first, second := newScriptStream(), newScriptStream()
	s := New(homebox.New("http://homebox.invalid/api", "tok"), nil, time.Hour, discardLogger())
	inst := s.defaultInstance()
	inst.hb = nil
	// The pre-warm gate has to be open, or the refresh returns before it can
	// reach the fetch at all.
	inst.lastRead.Store(time.Now().UnixNano())

	feed := &feedScript{answer: func(dial int) (eventStream, error) {
		if dial == 1 {
			return first, nil
		}
		return second, nil
	}}
	w := s.startWatching(feed.subscribe(), testTuning())
	if w == nil {
		t.Fatal("startWatching returned nothing for a server that has a Homebox")
	}
	t.Cleanup(func() { s.Close() })
	waitFor(t, watchDeadline, "the feed to connect", func() bool {
		return w.currentState() == feedConnected
	})

	first.push(homebox.EventEntityMutation)
	waitFor(t, watchDeadline, "the settler to stop", func() bool { return closed(w.settlerDone) })
	if got := w.currentState(); got != feedDegraded {
		t.Errorf("feed state = %v once the settler panicked, want degraded", got)
	}
	if got := w.refreshed.Load(); got != 1 {
		t.Fatalf("%d refreshes ran, want the 1 that panicked", got)
	}

	// An ordinary drop follows -- Homebox restarting, a wifi blip -- and the
	// reader, which never panicked, reconnects.
	first.Close()
	waitFor(t, watchDeadline, "the reader to reconnect", func() bool { return feed.dials.Load() >= 2 })

	// Pushed on the NEW socket, and waited for: the reader only reaches this
	// loop after the session it belongs to has published itself as connected,
	// so what the health check below reads is a state that reconnect set.
	second.push(homebox.EventEntityMutation)
	waitFor(t, watchDeadline, "the event on the reconnected socket", func() bool {
		return w.accepted.Load() >= 2
	})
	if got := w.refreshed.Load(); got != 1 {
		t.Errorf("%d refreshes after the settler died; something else is committing to the cache", got)
	}
	if got := s.changeFeed(); got != "degraded" {
		t.Errorf("changeFeed = %q on a socket that is up with a dead settler, want degraded: "+
			"every event it accepts from here on reaches nothing", got)
	}

	// And the process still shuts down.
	done := make(chan struct{})
	go func() { defer close(done); s.Close() }()
	select {
	case <-done:
	case <-time.After(watchDeadline):
		t.Fatal("Close hung after the settler panicked")
	}
}

// /healthz is exempt from the API token gate, so everything it prints is
// public. Three words, and nothing that names where this Homebox is or what
// reaches it.
func TestHealthReportsTheChangeFeed(t *testing.T) {
	const (
		homeboxURL = "http://homebox.internal.example:7745/api"
		homeboxKey = "hb_a_very_private_key"
		apiToken   = "shared-api-secret"
	)

	// Deliberately with no Authorization header: a reachability probe must not
	// need a credential to ask.
	health := func(t *testing.T, s *Server) (int, string, map[string]string) {
		t.Helper()
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode /healthz body %q: %v", rr.Body.String(), err)
		}
		return rr.Code, rr.Body.String(), body
	}

	t.Run("no homebox of its own", func(t *testing.T) {
		s := New(nil, nil, time.Hour, discardLogger())
		s.RequireToken(apiToken)
		s.StartWatching()
		if got := s.watch.Load(); got != nil {
			t.Fatal("StartWatching started a watcher for a server with no Homebox to watch")
		}
		code, _, body := health(t, s)
		if code != http.StatusOK || body["changeFeed"] != "disabled" {
			t.Errorf("status %d changeFeed %q, want 200 and disabled", code, body["changeFeed"])
		}
		// And shutting down a server that never started one is not a special
		// case: main calls Close unconditionally.
		if err := s.Close(); err != nil {
			t.Errorf("Close with no watcher running: %v", err)
		}
	})

	t.Run("connected", func(t *testing.T) {
		s := New(homebox.New(homeboxURL, homeboxKey), nil, time.Hour, discardLogger())
		s.RequireToken(apiToken)
		st := newScriptStream()
		feed := &feedScript{answer: func(int) (eventStream, error) { return st, nil }}
		w := s.startWatching(feed.subscribe(), testTuning())
		t.Cleanup(func() { s.Close() })
		waitFor(t, watchDeadline, "the feed to connect", func() bool {
			return w.currentState() == feedConnected
		})

		code, raw, body := health(t, s)
		if code != http.StatusOK {
			t.Fatalf("status %d without an API token; a probe must not need one", code)
		}
		if body["status"] != "ok" || body["changeFeed"] != "connected" {
			t.Errorf("body = %v, want status ok and changeFeed connected", body)
		}
		for _, secret := range []string{homeboxURL, homeboxKey, apiToken, "homebox.internal.example", "http"} {
			if strings.Contains(raw, secret) {
				t.Errorf("/healthz body %q contains %q; this endpoint is unauthenticated", raw, secret)
			}
		}

		s.Close()
		if _, _, body := health(t, s); body["changeFeed"] != "disabled" {
			t.Errorf("changeFeed = %q after Close, want disabled", body["changeFeed"])
		}
	})

	t.Run("degraded", func(t *testing.T) {
		s := New(homebox.New(homeboxURL, homeboxKey), nil, time.Hour, discardLogger())
		feed := &feedScript{answer: func(int) (eventStream, error) {
			return nil, fmt.Errorf("homebox subscribe: status 404: %w", homebox.ErrEventsUnsupported)
		}}
		w := s.startWatching(feed.subscribe(), testTuning())
		t.Cleanup(func() { s.Close() })
		waitFor(t, watchDeadline, "the feed to report itself degraded", func() bool {
			return w.currentState() == feedDegraded && feed.dials.Load() > 0
		})
		if _, _, body := health(t, s); body["changeFeed"] != "degraded" {
			t.Errorf("changeFeed = %q with a Homebox that has no events endpoint, want degraded",
				body["changeFeed"])
		}
	})
}

// wsAccept computes Sec-WebSocket-Accept independently of the client's own
// acceptKey, so that a handshake the client accepts below is two separate
// implementations of RFC 6455 section 1.3 agreeing rather than one comparing
// itself.
func wsAccept(clientKey string) string {
	h := sha1.New()
	io.WriteString(h, clientKey+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsTextFrame is the {0x81, len} + payload shape Homebox sends every event in.
// The measured frames are 16 and 27 bytes, so the 7-bit length form is all
// this needs.
func wsTextFrame(payload string) []byte {
	return append([]byte{0x81, byte(len(payload))}, payload...)
}

// The one test that runs the real transport: homebox.Subscribe against a
// hijacking server, so the seam where transport error shapes meet this
// package's dispatch and backoff is verified by running rather than by
// reading. Everything else in this file drives the subscribeFunc seam.
//
// The fake server's goroutine reports through a buffered channel and never
// touches t: httptest.Server.Close does not wait for a hijacked handler, so a
// t.Errorf from there can run after the test has finished, and the testing
// package answers that by panicking the whole binary.
func TestARealSocketReachesTheCache(t *testing.T) {
	type handshake struct{ path, auth, version string }
	shakes := make(chan handshake, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shakes <- handshake{
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			version: r.Header.Get("Sec-WebSocket-Version"),
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "not hijackable", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAccept(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n")); err != nil {
			return
		}
		// A payload that never parses, then the real thing. Dropping the
		// connection over the first would turn "Boxwright has not heard of
		// that event" into "Boxwright stopped noticing changes at all" on the
		// day somebody upgrades.
		_, _ = conn.Write(wsTextFrame(`{"event":`))
		_, _ = conn.Write(wsTextFrame(`{"event":"entity.mutation"}`))
		// Park until the client goes away. A handler that returned here would
		// close the socket under a stream the test is still using.
		_, _ = brw.Reader.ReadByte()
	}))
	t.Cleanup(srv.Close)

	tune := testTuning()
	// The only two durations that reach a real socket. Long enough that a
	// loaded machine does not fail the handshake, short enough to fail inside
	// the suite.
	tune.handshake = 5 * time.Second
	tune.idle = 5 * time.Second

	s := New(homebox.New(srv.URL+"/api", "hb_realsocket"), nil, time.Hour, discardLogger())
	// nil subscribe: the genuine homebox.Subscribe, wired exactly as
	// StartWatching wires it.
	w := s.startWatching(nil, tune)
	if w == nil {
		t.Fatal("startWatching returned nothing")
	}
	t.Cleanup(func() { s.Close() })

	awaitSettled(t, w, "the mutation frame from a real socket")

	if got := generationOf(newTestInstanceOf(t, w)); got != 1 {
		t.Errorf("generation = %d, want 1: a frame that crossed a real socket did not reach the cache", got)
	}
	if got := w.frames.Load(); got != 1 {
		t.Errorf("%d events reached the dispatcher, want 1: the unparseable payload should never "+
			"have got past the stream", got)
	}
	if got := w.accepted.Load(); got != 1 {
		t.Errorf("%d events were accepted as foreign edits, want 1", got)
	}
	if got := w.currentState(); got != feedConnected {
		t.Errorf("feed state = %v; the socket was dropped over a frame it could not parse", got)
	}

	select {
	case hs := <-shakes:
		if hs.path != "/api/v1/ws/events" {
			t.Errorf("subscribed to %q, want /api/v1/ws/events", hs.path)
		}
		if hs.auth != "Bearer hb_realsocket" {
			t.Errorf("Authorization = %q, want the operator's own bearer token", hs.auth)
		}
		if hs.version != "13" {
			t.Errorf("Sec-WebSocket-Version = %q, want 13", hs.version)
		}
	default:
		t.Fatal("no handshake reached the server")
	}
	select {
	case hs := <-shakes:
		t.Errorf("a second handshake arrived (%+v): the connection was dropped and redialled", hs)
	default:
	}
}

// The mute is checked at receipt AND at fire, and this is the case only the
// second one covers: a frame that arrived while nothing was being written, and
// a write that started inside the settle window behind it. Refreshing there
// would rebuild the index in the middle of a cataloguing session, against a
// Homebox that is still being written to.
//
// Postponed, never dropped: as far as anything here can tell the event was
// foreign, so it has to survive the mute rather than be discarded by it.
func TestAWriteInsideTheSettleWindowPostponesTheRefresh(t *testing.T) {
	tune := testTuning()
	// A wide settle window on purpose: the write below has to start inside it,
	// and a 20ms one would make this test a race against a loaded machine
	// rather than against the mechanism.
	tune.settle = 200 * time.Millisecond
	rig := newWatchRig(t, time.Hour, tune)
	inst := rig.inst()

	if code, _, _ := getBoxes(rig.h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}
	drainSettled(rig.w)

	rig.st.push(homebox.EventEntityMutation)
	waitFor(t, watchDeadline, "the event to arm the coalescer", func() bool {
		return rig.w.accepted.Load() >= 1
	})
	release := inst.beginWrite()

	select {
	case <-rig.w.settled:
		t.Fatal("refreshed while one of our own writes was still in flight")
	case <-time.After(2 * tune.settle):
	}
	if got := rig.w.refreshed.Load(); got != 0 {
		t.Fatalf("%d refreshes ran with a write live, want 0", got)
	}

	release()
	// The quiet window after a write is selfWriteQuiet, a const in
	// instance.go that no tuning can shorten, so it is spent here rather than
	// waited out for three seconds.
	inst.writeMu.Lock()
	inst.muteUntil = time.Now()
	inst.writeMu.Unlock()

	awaitSettled(t, rig.w, "the postponed refresh once the write finished")
	if got := rig.w.refreshed.Load(); got != 1 {
		t.Errorf("%d refreshes after the mute lifted, want 1: the event was dropped, not postponed", got)
	}
}
