package api

import (
	"encoding/json"
	"fmt"
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

// twoHomeboxes serves two different inventories from one address, telling them
// apart by the bearer token -- which is exactly what a backend fronting
// several people's Homeboxes has to do.
type twoHomeboxes struct {
	calls atomic.Int32
}

func (f *twoHomeboxes) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		who := "anon"
		switch r.Header.Get("Authorization") {
		case "Bearer alice-key":
			who = "alice"
		case "Bearer bob-key":
			who = "bob"
		}
		q := r.URL.Query()

		if strings.HasPrefix(r.URL.Path, "/api/v1/entities/") {
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			json.NewEncoder(w).Encode(map[string]any{
				"id": id, "name": id, "fields": []any{chosen()},
				"entityType": map[string]any{"id": "loc-type", "isLocation": true},
			})
			return
		}
		if len(q["parentIds"]) > 0 {
			json.NewEncoder(w).Encode(page())
			return
		}
		if q.Get("isLocation") == "true" {
			json.NewEncoder(w).Encode(page(loc(who+"-box", who+"'s box", "", "")))
			return
		}
		json.NewEncoder(w).Encode(page())
	}
}

func boxesAs(t *testing.T, h http.Handler, url, token string) []placement.Box {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil)
	if url != "" {
		req.Header.Set(headerHomeboxURL, url)
	}
	if token != "" {
		req.Header.Set(headerHomeboxToken, token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	boxes, _ := decodeBoxes(t, rr.Body)
	return boxes
}

func multiTenantServer(t *testing.T, f *twoHomeboxes) (*Server, string) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	base := srv.URL + "/api"
	s := New(homebox.New(base, "operator-key"), mustProvider(t), time.Hour, discardLogger())
	s.AllowClientCredentials(true)
	return s, base
}

// The whole point. Two people, one backend: neither may ever be shown the
// other's containers, and neither's cache may answer the other's request.
func TestTwoCallersNeverShareABoxIndex(t *testing.T) {
	f := &twoHomeboxes{}
	s, base := multiTenantServer(t, f)
	h := s.Routes()

	alice := boxesAs(t, h, base, "alice-key")
	bob := boxesAs(t, h, base, "bob-key")

	if len(alice) != 1 || alice[0].Name != "alice's box" {
		t.Fatalf("alice got %+v", alice)
	}
	if len(bob) != 1 || bob[0].Name != "bob's box" {
		t.Fatalf("bob got %+v, want his own -- the cache was keyed on nothing", bob)
	}

	// Each is cached separately: a second read for either costs no upstream call.
	before := f.calls.Load()
	boxesAs(t, h, base, "alice-key")
	boxesAs(t, h, base, "bob-key")
	if spent := f.calls.Load() - before; spent != 0 {
		t.Errorf("re-reading two warm indexes spent %d upstream calls, want 0", spent)
	}
}

// Invalidating one person's index must not throw away another's.
func TestInvalidatingOneCallerLeavesTheOtherWarm(t *testing.T) {
	f := &twoHomeboxes{}
	s, base := multiTenantServer(t, f)
	h := s.Routes()

	boxesAs(t, h, base, "alice-key")
	boxesAs(t, h, base, "bob-key")

	s.instanceFrom(credentials{baseURL: base, token: "alice-key"}).invalidate()

	before := f.calls.Load()
	boxesAs(t, h, base, "bob-key")
	if spent := f.calls.Load() - before; spent != 0 {
		t.Errorf("bob's read spent %d upstream calls after ALICE was invalidated", spent)
	}
	before = f.calls.Load()
	boxesAs(t, h, base, "alice-key")
	if f.calls.Load() == before {
		t.Error("alice's read served a cache that had been invalidated")
	}
}

// The self-hosted product is the default: with no headers, every request goes
// to the operator's own Homebox.
func TestNoHeadersMeansTheOperatorsHomebox(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)
	got := containers(boxesAs(t, s.Routes(), "", ""))
	if len(got) != 2 {
		t.Fatalf("got %d containers, want the operator's own 2", len(got))
	}
}

// A backend that was not told to accept client credentials must never connect
// to a URL a client named. Refusing loudly, rather than ignoring the headers,
// so a misconfigured app is not silently writing into the operator's Homebox.
func TestClientCredentialsAreRefusedUnlessEnabled(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f) // AllowClientCredentials not called

	req := httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil)
	req.Header.Set(headerHomeboxURL, "http://evil.example/api")
	req.Header.Set(headerHomeboxToken, "whatever")
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "ALLOW_CLIENT_HOMEBOX") {
		t.Errorf("the refusal should name the setting that would allow it: %s", rr.Body)
	}
}

func TestCredentialsResolution(t *testing.T) {
	s := New(homebox.New("http://home.example/api", "operator-key"), nil, time.Hour, discardLogger())
	s.AllowClientCredentials(true)

	tests := []struct {
		name        string
		url, token  string
		wantURL     string
		wantToken   string
		wantErrPart string
	}{
		{name: "neither: the operator's own", wantURL: "http://home.example/api", wantToken: "operator-key"},
		{
			// The safer half: no host is named, so there is no request
			// forwarding at all -- just a different Homebox account.
			name:  "token alone reuses the operator's URL",
			token: "alice-key", wantURL: "http://home.example/api", wantToken: "alice-key",
		},
		{
			// Pairing a caller's URL with the OPERATOR's token would send the
			// operator's Homebox credential to a host the caller chose.
			name: "a URL with no token is refused",
			url:  "http://alice.example/api", wantErrPart: headerHomeboxToken,
		},
		{
			name: "both", url: "http://alice.example/api/", token: "alice-key",
			wantURL: "http://alice.example/api", wantToken: "alice-key",
		},
		{
			// url.Parse accepts these without complaint.
			name: "a non-http scheme is refused",
			url:  "file:///etc/passwd", token: "x", wantErrPart: "http or https",
		},
		{name: "a URL with no host is refused", url: "http:///api", token: "x", wantErrPart: "no host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil)
			if tc.url != "" {
				r.Header.Set(headerHomeboxURL, tc.url)
			}
			if tc.token != "" {
				r.Header.Set(headerHomeboxToken, tc.token)
			}
			got, err := s.credentialsFor(r)
			if tc.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.same(credentials{baseURL: tc.wantURL, token: tc.wantToken}) {
				t.Errorf("got %q/<token>, want %q", got.baseURL, tc.wantURL)
			}
		})
	}
}

// Different credentials must key different caches, and the key must not be the
// token itself -- it ends up in a map key that any future debug line could print.
func TestCacheKeysSeparateCredentialsWithoutCarryingTheToken(t *testing.T) {
	a := credentials{baseURL: "http://h/api", token: "alice"}
	b := credentials{baseURL: "http://h/api", token: "bob"}
	c := credentials{baseURL: "http://other/api", token: "alice"}

	if a.key() == b.key() {
		t.Error("two tokens against one Homebox share a cache key")
	}
	if a.key() == c.key() {
		t.Error("one token against two Homeboxes shares a cache key")
	}
	if a.key() != (credentials{baseURL: "http://h/api", token: "alice"}).key() {
		t.Error("the same credentials produced two keys, so nothing would ever hit the cache")
	}
	if strings.Contains(a.key(), "alice") {
		t.Errorf("the key contains the token: %q", a.key())
	}
}

// A backend serving many people must not grow one box index per person
// forever. The least recently used goes.
func TestInstancesAreBounded(t *testing.T) {
	s := New(homebox.New("http://home.example/api", "operator-key"), nil, time.Hour, discardLogger())
	for i := 0; i < maxInstances*2; i++ {
		s.instanceFrom(credentials{baseURL: "http://home.example/api", token: fmt.Sprintf("t%d", i)})
	}
	s.instMu.Lock()
	n := len(s.instances)
	s.instMu.Unlock()
	if n > maxInstances {
		t.Errorf("holding %d instances, want at most %d", n, maxInstances)
	}
}

// The change feed fires on our OWN writes, and the frame says nothing but
// "something changed" -- so a cataloguing session would answer its own echoes
// with a full rebuild per item unless they are muted while it writes.
//
// The overlapping case is the one that matters: it is precisely what a
// timestamp window cannot give you, because one slow entry -- CreateEntity,
// SetFields and a 10 MB attachment upload -- outlives any window stamped
// before it started.
func TestBeginWriteMutesWhileAnyWriteIsLive(t *testing.T) {
	inst := &instance{}

	if inst.muted(time.Now()) {
		t.Fatal("a fresh instance is muted, so the first foreign edit would be ignored")
	}

	first := inst.beginWrite()
	second := inst.beginWrite()
	if !inst.muted(time.Now()) {
		t.Error("not muted with two writes in flight")
	}

	// Far past any quiet window: only the live second write may be holding
	// the mute open now.
	first()
	if !inst.muted(time.Now().Add(10 * selfWriteQuiet)) {
		t.Error("the first write's release unmuted the instance while the second was still in flight; " +
			"that is the miscount a counter exists to make impossible")
	}

	second()
	if !inst.muted(time.Now()) {
		t.Error("unmuted the instant the last write returned, before its echo could arrive")
	}
	if inst.muted(time.Now().Add(selfWriteQuiet + time.Second)) {
		t.Error("still muted past the quiet window, so no foreign edit would ever be seen again")
	}
}

// A caller may defer the release and also call it early. Decrementing twice
// would drive the count negative, and a negative count mutes nothing ever
// again -- every subsequent session's echoes would rebuild the index.
func TestReleasingAWriteTwiceIsHarmless(t *testing.T) {
	inst := &instance{}

	done := inst.beginWrite()
	done()
	done()

	inst.writeMu.Lock()
	live := inst.writesLive
	inst.writeMu.Unlock()
	if live != 0 {
		t.Fatalf("writesLive = %d after a double release, want 0", live)
	}
	if !inst.muted(time.Now()) {
		t.Error("the release did not stamp the quiet window")
	}
}

// The watcher reads this state off its own goroutine while request handlers
// write it. Meaningful under -race.
func TestMutedIsSafeAgainstConcurrentWrites(t *testing.T) {
	inst := &instance{}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := inst.beginWrite()
			defer done()
			inst.muted(time.Now()) // the watcher's side of the same fields
		}()
	}
	wg.Wait()

	inst.writeMu.Lock()
	live := inst.writesLive
	inst.writeMu.Unlock()
	if live != 0 {
		t.Errorf("writesLive = %d once every write finished, want 0", live)
	}
}

// A background watcher holds a KEY, never a pointer. Resolving it must not
// bring an evicted cache back from the dead, and must not touch the LRU stamp:
// a goroutine refreshing that would pin its own instance and defeat the bound
// TestInstancesAreBounded checks.
func TestLookupInstanceResolvesWithoutCreatingOrPinning(t *testing.T) {
	s := New(homebox.New("http://home.example/api", "operator-key"), nil, time.Hour, discardLogger())

	s.instMu.Lock()
	before := len(s.instances)
	s.instMu.Unlock()

	if got := s.lookupInstance("a-key-nothing-was-built-for"); got != nil {
		t.Errorf("lookupInstance invented an instance for an unknown key: %+v", got)
	}
	s.instMu.Lock()
	after := len(s.instances)
	s.instMu.Unlock()
	if after != before {
		t.Errorf("the instance map grew from %d to %d; a watcher naming an evicted "+
			"instance would resurrect a cache no request asked for", before, after)
	}

	key := s.defaults.key()
	s.instMu.Lock()
	used := s.instances[key].used
	s.instMu.Unlock()

	if got := s.lookupInstance(key); got == nil {
		t.Fatal("lookupInstance did not find the operator's own instance")
	}
	s.instMu.Lock()
	moved := s.instances[key].used
	s.instMu.Unlock()
	if !moved.Equal(used) {
		t.Error("lookupInstance moved the eviction stamp; a background refresh would keep " +
			"its instance alive forever while nobody was using it")
	}
}
