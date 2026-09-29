package api

import (
	"bytes"
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

// cacheHomebox serves both the reads a box-index rebuild makes and the writes
// /catalog makes, so a test can drive a whole file-then-recommend cycle
// against one instance and count exactly what the second half sends upstream.
//
// The Garage > California > {Fresno, Modesto} shape matches fakeHomebox, and
// only the two totes carry boxwrightPlacement, so only they are candidates.
type cacheHomebox struct {
	mu       sync.Mutex
	locs     []map[string]any // the isLocation=true listing; new containers join it
	items    []map[string]any // the isLocation=false listing; created items join it
	fields   map[string][]any // entity id -> custom fields, updated by SetFields
	children map[string][]map[string]any
	created  []homebox.CreateEntityRequest
	tags     []homebox.Tag
	nextID   int

	// indexReads counts ONLY requests a rebuild makes. The whole point of the
	// patch is that a /recommend following a /catalog adds none.
	indexReads atomic.Int32

	// ignoreFieldFilter makes the fake behave like a server with no support
	// for the fields= filter: every query comes back with everything.
	ignoreFieldFilter bool

	// failPuts makes every field write to these entity ids fail, as a
	// Homebox that went away mid-request would.
	failPuts map[string]bool

	// detailGate, once armed, holds the FIRST detail fetch open so a test can
	// land a write while a rebuild is in flight. Guarded by mu because the
	// handler runs on the httptest server's own goroutines.
	//
	// gateHits counts arrivals rather than a sync.Once holding the gate open,
	// because Once.Do blocks EVERY caller until the first f returns: it froze
	// every concurrent detail read in the fake, not just the first, so a test
	// with two rebuilds in flight deadlocked instead of running.
	detailGate chan struct{}
	gateHits   atomic.Int32

	// unreachable makes every request fail, so a test can see what boxIndex
	// serves once Homebox has gone away -- the storage-unit-with-no-signal
	// case the stale fallback exists for.
	unreachable bool
}

func newCacheHomebox() *cacheHomebox {
	return &cacheHomebox{
		locs: []map[string]any{
			loc("garage", "Garage", "", ""),
			loc("california", "California", "garage", "Garage"),
			loc("fresno", "Fresno", "california", "California"),
			loc("modesto", "Modesto", "california", "California"),
		},
		// Only the two totes are opted in to placement. Nothing about their
		// position in the hierarchy says so any more; this does.
		fields: map[string][]any{
			"fresno":  {chosen()},
			"modesto": {chosen()},
		},
		children: map[string][]map[string]any{},
	}
}

// fieldsOf returns one entity's custom fields.
func (f *cacheHomebox) fieldsOf(id string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]any{}, f.fields[id]...)
	return out
}

// applyFieldFilter narrows a listing the way the live server does, or leaves
// it whole when no usable pair was sent -- which is the trap: `fields=Name`
// with no "=" is silently ignored and everything comes back.
func (f *cacheHomebox) applyFieldFilter(rows []map[string]any, pairs []string) []map[string]any {
	usable := false
	for _, p := range pairs {
		if strings.Contains(p, "=") {
			usable = true
		}
	}
	f.mu.Lock()
	ignoring := f.ignoreFieldFilter
	f.mu.Unlock()
	if !usable || ignoring {
		return rows
	}
	kept := []map[string]any{}
	for _, r := range rows {
		if id, _ := r["id"].(string); f.matchesFields(id, pairs) {
			kept = append(kept, r)
		}
	}
	return kept
}

// matchesFields models the live fields=Name=Value filter: exact, text only,
// repeated parameters OR'd, and a bare name with no "=" silently ignored.
func (f *cacheHomebox) matchesFields(id string, pairs []string) bool {
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		for _, raw := range f.fieldsOf(id) {
			fld, _ := raw.(map[string]any)
			if fld["name"] == name && fld["type"] == "text" && fld["textValue"] == value {
				return true
			}
		}
	}
	return false
}

// armGate makes the next rebuild block on its first detail fetch, and ONLY
// that one: every other read, including a concurrent rebuild's, runs straight
// through. Close the returned channel to let the parked one finish.
func (f *cacheHomebox) armGate() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateHits.Store(0)
	f.detailGate = make(chan struct{})
	return f.detailGate
}

func (f *cacheHomebox) gate() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detailGate
}

// goOffline makes every subsequent request fail upstream.
func (f *cacheHomebox) goOffline() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreachable = true
}

func (f *cacheHomebox) offline() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unreachable
}

func (f *cacheHomebox) isLocationID(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.locs {
		if l["id"] == id {
			return true
		}
	}
	return false
}

func (f *cacheHomebox) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f.offline() {
			http.Error(w, "homebox is unreachable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()

		switch {
		case r.URL.Path == "/api/v1/entity-types":
			json.NewEncoder(w).Encode([]homebox.EntityType{
				{ID: "item-type", Name: "Item"},
				{ID: "loc-type", Name: "Location", IsLocation: true},
			})

		case r.URL.Path == "/api/v1/tags" && r.Method == http.MethodGet:
			f.mu.Lock()
			tags := append([]homebox.Tag{}, f.tags...)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(tags)

		case r.URL.Path == "/api/v1/tags" && r.Method == http.MethodPost:
			var req homebox.TagCreateRequest
			json.NewDecoder(r.Body).Decode(&req)
			t := homebox.Tag{ID: "tag-" + strings.ToLower(req.Name), Name: req.Name}
			f.mu.Lock()
			f.tags = append(f.tags, t)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(t)

		case r.URL.Path == "/api/v1/entities" && r.Method == http.MethodPost:
			var req homebox.CreateEntityRequest
			json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.nextID++
			id := fmt.Sprintf("e%d", f.nextID)
			f.created = append(f.created, req)
			if req.EntityTypeID == "loc-type" {
				// A newly created container must show up in the hierarchy the
				// NEXT rebuild reads, or the fallback cannot be observed.
				f.locs = append(f.locs, loc(id, req.Name, req.ParentID, ""))
			} else {
				// Created items have to be findable too, or a dedupe lookup
				// can never see what a previous request filed.
				f.items = append(f.items, map[string]any{
					"id": id, "name": req.Name,
					"entityType": map[string]any{"id": "item-type", "isLocation": false},
				})
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"id": id, "name": req.Name})

		case strings.HasPrefix(r.URL.Path, "/api/v1/entities/") && r.Method == http.MethodPut:
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			// SetFields is a full replace, so the fields it sends become the
			// entity's fields. Recording them is what lets a container created
			// mid-test be opted in to placement the way the real one is.
			var body struct {
				Fields []any `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			if f.failPuts[id] {
				f.mu.Unlock()
				http.Error(w, "upstream went away", http.StatusBadGateway)
				return
			}
			f.fields[id] = body.Fields
			f.mu.Unlock()
			// The live server answers a PUT with the entity, fields included.
			json.NewEncoder(w).Encode(map[string]any{"id": id, "fields": body.Fields})

		case strings.HasPrefix(r.URL.Path, "/api/v1/entities/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			// SetFields reads the entity back before its full-replace PUT, so
			// not every detail fetch belongs to a rebuild. Only a LOCATION
			// read is index traffic; counting or gating the item read would
			// make the call counts meaningless and deadlock the gate.
			if f.isLocationID(id) {
				f.indexReads.Add(1)
				if g := f.gate(); g != nil && f.gateHits.Add(1) == 1 {
					<-g
				}
				json.NewEncoder(w).Encode(map[string]any{
					"id": id, "name": id, "fields": f.fieldsOf(id),
					"entityType": map[string]any{"id": "loc-type", "isLocation": true},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": id, "name": id, "fields": f.fieldsOf(id),
				"entityType": map[string]any{"id": "item-type", "isLocation": false},
			})

		case len(q["parentIds"]) > 0:
			f.indexReads.Add(1)
			f.mu.Lock()
			kids := f.children[q["parentIds"][0]]
			f.mu.Unlock()
			json.NewEncoder(w).Encode(page(kids...))

		case q.Get("isLocation") == "true":
			f.indexReads.Add(1)
			f.mu.Lock()
			locs := append([]map[string]any{}, f.locs...)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(page(f.applyFieldFilter(locs, q["fields"])...))

		default:
			f.mu.Lock()
			items := append([]map[string]any{}, f.items...)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(page(f.applyFieldFilter(items, q["fields"])...))
		}
	}
}

func cacheTestServer(t *testing.T, f *cacheHomebox, ttl time.Duration) *Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return New(homebox.New(srv.URL+"/api", "tok"), mustProvider(t), ttl, discardLogger())
}

// The next three helpers deliberately take no *testing.T: the concurrency test
// calls them from goroutines, where t.Fatalf is not allowed.

func getBoxes(h http.Handler) (int, []placement.Box, bool) {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	var out struct {
		Boxes []placement.Box `json:"boxes"`
		Stale bool            `json:"stale"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	return rr.Code, out.Boxes, out.Stale
}

// postCatalog files one request's worth of entries. Variadic because the point
// of most of these tests is one entry, and of a couple of them is several.
func postCatalog(h http.Handler, entries ...catalogEntry) int {
	b, _ := json.Marshal(catalogRequest{Entries: entries})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/catalog", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr.Code
}

// postRecommend places ONE item: the wire is an array, and a photo of a single
// item is a one-element one.
func postRecommend(h http.Handler, item placement.ItemDraft) (int, placement.Recommendation) {
	code, recs := postRecommendBatch(h, item)
	if len(recs) == 0 {
		return code, placement.Recommendation{}
	}
	return code, recs[0]
}

func postRecommendBatch(h http.Handler, items ...placement.ItemDraft) (int, []placement.Recommendation) {
	b, _ := json.Marshal(recommendRequest{Items: items})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/recommend", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	var out struct {
		Recommendations []placement.Recommendation `json:"recommendations"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	return rr.Code, out.Recommendations
}

func findBox(boxes []placement.Box, id string) (placement.Box, bool) {
	for _, b := range boxes {
		if b.ID == id {
			return b, true
		}
	}
	return placement.Box{}, false
}

func candidateBox(rec placement.Recommendation, id string) (placement.Box, bool) {
	for _, c := range rec.Candidates {
		if c.Box.ID == id {
			return c.Box, true
		}
	}
	return placement.Box{}, false
}

// The headline cost fix. A rebuild is one list call plus two per chosen
// location -- 18 seconds for 80 of them on the live instance -- and
// handleCatalog used to
// invalidate, so EVERY item filed in a session paid it on the next
// recommendation. The write tells us exactly what changed, so the cache is
// patched and the next recommendation goes nowhere near Homebox.
func TestRecommendAfterCatalogMakesNoUpstreamCallsAndSeesTheItem(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	if code, boxes, _ := getBoxes(h); code != http.StatusOK || len(containers(boxes)) != 2 {
		t.Fatalf("warm-up: status %d, %d containers, want 200 and the two totes",
			code, len(containers(boxes)))
	}
	if f.indexReads.Load() == 0 {
		t.Fatal("the warm-up made no upstream calls, so this test proves nothing")
	}

	// Three small tools into Fresno.
	if code := postCatalog(h, catalogEntry{
		Item: placement.ItemDraft{
			Name: "socket adapter", Category: "tools", SizeBucket: "S", Quantity: 3,
		},
		BoxID: "fresno",
	}); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}

	before := f.indexReads.Load()
	code, rec := postRecommend(h, placement.ItemDraft{
		Name: "socket set", Category: "tools", SizeBucket: "M", Quantity: 1,
	})
	if code != http.StatusOK {
		t.Fatalf("recommend status %d, want 200", code)
	}
	if spent := f.indexReads.Load() - before; spent != 0 {
		t.Errorf("the recommendation after a catalog spent %d upstream index calls, want 0: "+
			"that is the full rebuild this change exists to avoid", spent)
	}

	fresno, ok := candidateBox(rec, "fresno")
	if !ok {
		t.Fatalf("Fresno is not a candidate for a tools item after three tools were filed there: %+v", rec)
	}
	if fresno.Categories["tools"] != 3 {
		t.Errorf("Fresno categories = %v, want tools:3 -- quantity is per capture, not per request",
			fresno.Categories)
	}
	if fresno.ItemCount != 3 {
		t.Errorf("Fresno itemCount = %d, want 3", fresno.ItemCount)
	}
	// The delta must land on the box that was written to and nowhere else.
	if modesto, ok := candidateBox(rec, "modesto"); ok && (modesto.ItemCount != 0 || len(modesto.Categories) != 0) {
		t.Errorf("Modesto was touched by a write to Fresno: %+v", modesto)
	}
}

// A brand-new container cannot be expressed as a delta: it is not in the index
// to patch, and a location entering the chosen set is not something a per-item
// delta can say. Fall back to the full rebuild.
func TestCatalogIntoANewContainerFallsBackToAFullRebuild(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	getBoxes(h)

	if code := postCatalog(h, catalogEntry{
		Item: placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"},
		NewContainer: &newContainerRequest{
			Label: "Tools 1", ParentID: "california", SizeBucket: "L", Access: "easy",
		},
	}); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}

	// Snapshot AFTER the write: creating a container reads it back to merge
	// custom fields, and that read is not rebuild traffic.
	before := f.indexReads.Load()
	code, boxes, stale := getBoxes(h)
	if code != http.StatusOK || stale {
		t.Fatalf("status %d stale %v", code, stale)
	}
	if f.indexReads.Load() == before {
		t.Fatal("served the cache; a new container is invisible to it and needs a full rebuild")
	}
	if _, ok := findBox(boxes, "e1"); !ok {
		t.Errorf("the container just created is missing from the index: %+v", boxes)
	}
}

// Same fallback for a box the index has never heard of -- an id the client
// held over a rebuild that dropped it, say. Better a slow correct answer than
// a fast invented one.
func TestCatalogIntoAnUnknownBoxFallsBackToAFullRebuild(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	getBoxes(h)

	if code := postCatalog(h, catalogEntry{
		Item:  placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"},
		BoxID: "somewhere-else",
	}); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}
	before := f.indexReads.Load()
	if _, _, _ = getBoxes(h); f.indexReads.Load() == before {
		t.Error("served the cache after writing to a box it does not model")
	}
}

// The generation counter, seen from the other side. A rebuild that started
// before the patch is holding a pre-patch snapshot; committing it resurrects
// state the write already corrected and the just-filed item vanishes for a
// whole TTL. This is the lost-invalidation bug (see the generation counter
// commit) wearing a different hat, so the patch bumps the same counter.
func TestPatchDuringAnInFlightRebuildIsNotLost(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	if code, _, _ := getBoxes(h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}

	// Park the next rebuild on its first detail fetch, then make one due.
	release := f.armGate()
	s.defaultInstance().invalidate()

	done := make(chan struct{})
	go func() {
		defer close(done)
		getBoxes(h)
	}()

	// Let the rebuild get as far as the gate, then file an item into a box it
	// has already read and is about to overwrite.
	time.Sleep(50 * time.Millisecond)
	if code := postCatalog(h, catalogEntry{
		Item:  placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M", Quantity: 1},
		BoxID: "fresno",
	}); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}
	close(release)
	<-done

	inst := s.defaultInstance()
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	fresno, ok := findBox(inst.boxes, "fresno")
	if !ok {
		t.Fatalf("Fresno left the cache entirely: %+v", inst.boxes)
	}
	if fresno.Categories["tools"] != 1 || fresno.ItemCount != 1 {
		t.Errorf("cached Fresno = categories %v, itemCount %d; want tools:1 and 1 item. "+
			"The in-flight rebuild committed its pre-patch snapshot and erased the write",
			fresno.Categories, fresno.ItemCount)
	}
}

// awaitRebuildAtGate blocks until a rebuild has reached the armed gate, so a
// test can supersede one that is definitely already in flight. Polled rather
// than slept, because a sleep would decide by timing the very thing under
// test: whether the generation moved before or after the rebuild recorded it.
func awaitRebuildAtGate(t *testing.T, f *cacheHomebox, reads int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.indexReads.Load() < reads {
		if time.Now().After(deadline) {
			t.Fatalf("no rebuild reached the gate: %d index reads, want %d", f.indexReads.Load(), reads)
		}
		time.Sleep(time.Millisecond)
	}
}

// The generation counter on a COLD cache. Dropping a superseded result is
// right for a warm one, but on the very FIRST fetch it also left `loaded`
// false, and that is the flag two other things key off: patchBox refuses an
// index that has never loaded, so every catalog write in the session falls
// back to the full rebuild the patch exists to avoid, and boxIndex has no
// stale data to serve, so the next upstream failure answers 502 instead of the
// boxes we are holding. A /catalog landing while the first rebuild after
// startup is still in flight is all it takes to reach both.
func TestFirstRebuildSupersededStillPublishesItsSnapshot(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	// Park the first rebuild there has ever been on its first detail fetch:
	// one list call plus that detail is two index reads.
	release := f.armGate()
	var code int
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, _, _ = getBoxes(h)
	}()
	awaitRebuildAtGate(t, f, 2)

	// Supersede it while it is parked -- a write landing mid-rebuild -- then
	// let it finish.
	inst := s.defaultInstance()
	inst.invalidate()
	close(release)
	<-done

	if code != http.StatusOK {
		t.Fatalf("the parked rebuild answered %d, want 200", code)
	}
	inst.mu.RLock()
	loaded, cached, fetchedAt := inst.loaded, inst.boxes, inst.fetched
	inst.mu.RUnlock()
	if !loaded {
		t.Fatal("the cache is still cold after a successful first fetch: patchBox will refuse " +
			"every catalog write and there is nothing to fall back on when Homebox goes away")
	}
	if !fetchedAt.IsZero() {
		t.Errorf("fetched = %v, want zero: this snapshot was already superseded when it landed, "+
			"so it must stay due for immediate refresh", fetchedAt)
	}
	if _, ok := findBox(cached, "fresno"); !ok {
		t.Fatalf("published without its boxes, which is the same as not publishing: %+v", cached)
	}

	// 1. The write path stops paying for a full rebuild every item.
	if !inst.patchBox(boxDelta{boxID: "fresno", category: "tools", quantity: 1, litres: 8}) {
		t.Error("patchBox refuses the index, so every catalog write falls back to a full rebuild")
	}

	// 2. The offline fallback exists. Without it this is an empty slice and a
	// 502 -- in a storage unit with no signal, for a cache we had filled.
	f.goOffline()
	code, boxes, stale := getBoxes(h)
	if code != http.StatusOK || !stale {
		t.Fatalf("with Homebox unreachable: status %d stale %v, want 200 and stale=true", code, stale)
	}
	if n := len(containers(boxes)); n != 2 {
		t.Errorf("served %d containers offline, want the 2 the cache is holding", n)
	}
}

// The warm case is unchanged: with an index already loaded, a generation bump
// during a rebuild still drops the result, because what the cache holds is
// newer -- and `fetched` stays where invalidate() left it rather than marking
// a superseded read fresh for a whole TTL.
func TestSupersededRebuildOnAWarmCacheIsStillDropped(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	if code, boxes, _ := getBoxes(h); code != http.StatusOK || len(containers(boxes)) != 2 {
		t.Fatalf("warm-up: status %d, %d containers", code, len(containers(boxes)))
	}

	inst := s.defaultInstance()
	warmReads := f.indexReads.Load()
	release := f.armGate()
	inst.invalidate()

	done := make(chan struct{})
	go func() {
		defer close(done)
		getBoxes(h)
	}()
	awaitRebuildAtGate(t, f, warmReads+2)

	// Give the parked rebuild something to carry that the cache does not have,
	// so a committed result would be plainly visible: Fresno's contents are
	// read after the gate. Then supersede it.
	f.mu.Lock()
	f.children["fresno"] = []map[string]any{item("hammer", "hammer", "")}
	f.mu.Unlock()
	inst.invalidate()
	close(release)
	<-done

	inst.mu.RLock()
	defer inst.mu.RUnlock()
	if !inst.fetched.IsZero() {
		t.Errorf("fetched = %v, want zero: a superseded rebuild marked the cache fresh", inst.fetched)
	}
	fresno, ok := findBox(inst.boxes, "fresno")
	if !ok {
		t.Fatalf("Fresno left the cache: %+v", inst.boxes)
	}
	if fresno.UsedUnits != 0 || len(fresno.Categories) != 0 {
		t.Errorf("cached Fresno = categories %v, usedUnits %d; want empty -- the superseded "+
			"rebuild committed over an index that was already newer",
			fresno.Categories, fresno.UsedUnits)
	}
}

// Which cache boxIndex falls back on is decided AFTER the fetch fails, not on
// the snapshot it took ~21 seconds earlier. The app fires /boxes and
// /recommend together at startup, so "another request filled the cache while
// mine was in flight" is what an ordinary cold start looks like rather than a
// corner of one. Deciding on the pre-fetch copy answered 502 with no boxes for
// a whole rebuild's width, while a complete index sat in the instance.
func TestARebuildThatStartedColdServesWhatLandedWhileItRan(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	// A request against a cold instance, parked on its first location detail.
	release := f.armGate()
	var (
		code   int
		boxes  []placement.Box
		stale  bool
		parked = make(chan struct{})
	)
	go func() {
		defer close(parked)
		code, boxes, stale = getBoxes(h)
	}()
	awaitRebuildAtGate(t, f, 2)

	// A second request runs a whole rebuild past it and publishes a full
	// index. Nothing about the first one has changed: its own fetch is still
	// in flight, and its snapshot still says the cache is cold.
	if c, b, _ := getBoxes(h); c != http.StatusOK || len(containers(b)) != 2 {
		t.Fatalf("the concurrent rebuild answered %d with %d containers, want 200 and 2",
			c, len(containers(b)))
	}

	// Then the signal drops, and the parked request's remaining reads fail.
	f.goOffline()
	close(release)
	<-parked

	if code != http.StatusOK {
		t.Fatalf("the request that started cold answered %d: it decided on its pre-fetch "+
			"snapshot and threw away the index another request had already published", code)
	}
	if !stale {
		t.Error("stale = false on an answer served from cache after the refresh failed")
	}
	if n := len(containers(boxes)); n != 2 {
		t.Errorf("served %d containers, want the 2 the instance is holding", n)
	}
}

// boxIndex hands callers the cached slice itself and drops the lock before the
// handler ranges over it, so patching a Box -- and above all its Categories
// map -- in place would be a data race against every concurrent reader.
// Run under -race, this fails immediately without copy-on-write.
//
// It doubles as a lost-update check: every patch is applied under the write
// lock, so all 80 writes must be visible at the end.
func TestConcurrentRecommendWhileCatalogPatchesTheCache(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	if code, _, _ := getBoxes(h); code != http.StatusOK {
		t.Fatalf("warm-up status %d", code)
	}

	const writers, readers, each = 8, 8, 10
	var wg sync.WaitGroup
	var writeErrs, readErrs atomic.Int32

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < each; n++ {
				// Recommend both ranges over the boxes and reads every
				// Categories map, which is exactly the shared state a patch
				// would otherwise mutate underneath it.
				if code, _ := postRecommend(h, placement.ItemDraft{
					Name: "socket set", Category: "tools", SizeBucket: "M",
				}); code != http.StatusOK {
					readErrs.Add(1)
				}
			}
		}()
	}
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < each; n++ {
				if code := postCatalog(h, catalogEntry{
					Item:  placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M", Quantity: 1},
					BoxID: "fresno",
				}); code != http.StatusCreated {
					writeErrs.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if n := readErrs.Load(); n != 0 {
		t.Errorf("%d recommendations failed", n)
	}
	if n := writeErrs.Load(); n != 0 {
		t.Fatalf("%d catalog writes failed", n)
	}

	inst := s.defaultInstance()
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	fresno, ok := findBox(inst.boxes, "fresno")
	if !ok {
		t.Fatalf("Fresno left the cache: %+v", inst.boxes)
	}
	if want := writers * each; fresno.Categories["tools"] != want {
		t.Errorf("Fresno tools = %d, want %d: patches were lost to each other",
			fresno.Categories["tools"], want)
	}
	if want := writers * each; fresno.ItemCount != want {
		t.Errorf("Fresno itemCount = %d, want %d", fresno.ItemCount, want)
	}
}

// The delta is only safe to apply when there is something to apply it to.
func TestPatchRefusesWhatItCannotApply(t *testing.T) {
	inst := New(nil, nil, time.Minute, discardLogger()).defaultInstance()

	if inst.patchBox(boxDelta{boxID: "fresno", category: "tools", quantity: 1, litres: 8}) {
		t.Error("patched an index that has never loaded")
	}
	inst.mu.Lock()
	inst.loaded = true
	inst.boxes = []placement.Box{{ID: "fresno", Name: "Fresno"}} // no Categories map yet
	inst.mu.Unlock()

	if inst.patchBox(boxDelta{boxID: "modesto", category: "tools", quantity: 1, litres: 8}) {
		t.Error("patched a box the index does not contain")
	}
	if inst.patchBox(boxDelta{boxID: "fresno", category: "tools", quantity: 0, litres: 0}) {
		t.Error("accepted a zero-quantity delta")
	}
	if !inst.patchBox(boxDelta{boxID: "fresno", category: "tools", quantity: 2, litres: 16}) {
		t.Fatal("refused a patch it could apply")
	}

	inst.mu.RLock()
	defer inst.mu.RUnlock()
	if got := inst.boxes[0]; got.Categories["tools"] != 2 || got.ItemCount != 2 {
		t.Errorf("Fresno = categories %v, itemCount %d; want tools:2 and 2 items",
			got.Categories, got.ItemCount)
	}
}

// Drift has to self-heal. The patch keeps the cache serveable but deliberately
// does not touch `fetched`, so the TTL keeps running from the last real fetch
// and the periodic full refresh still lands -- picking up size accounting we
// approximated, and anything changed in the Homebox UI behind our back.
func TestPatchDoesNotPostponeTheTTLRefresh(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, 40*time.Millisecond)
	h := s.Routes()

	getBoxes(h)
	if code := postCatalog(h, catalogEntry{
		Item:  placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"},
		BoxID: "fresno",
	}); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}

	before := f.indexReads.Load()
	time.Sleep(80 * time.Millisecond)
	if _, _, _ = getBoxes(h); f.indexReads.Load() == before {
		t.Error("no refresh past the TTL; a patched cache would drift from Homebox forever")
	}
}

// One photo, several items, one index. The patch has to account for EVERY
// entity that landed, not just the first: a tote credited with one of the
// three items it just received looks emptier than it is, and the next
// recommendation keeps steering items into a box that is already full.
func TestCatalogPatchesTheIndexForEveryEntity(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()

	if code, boxes, _ := getBoxes(h); code != http.StatusOK || len(containers(boxes)) != 2 {
		t.Fatalf("warm-up: status %d, %d containers", code, len(containers(boxes)))
	}

	// A drill and two tape measures into Fresno, a jar into Modesto.
	if code := postCatalog(h,
		catalogEntry{Item: placement.ItemDraft{
			Name: "cordless drill", Category: "tools", SizeBucket: "M", Quantity: 1}, BoxID: "fresno"},
		catalogEntry{Item: placement.ItemDraft{
			Name: "tape measure", Category: "tools", SizeBucket: "S", Quantity: 2}, BoxID: "fresno"},
		catalogEntry{Item: placement.ItemDraft{
			Name: "mason jar", Category: "kitchen", SizeBucket: "S", Quantity: 1}, BoxID: "modesto"},
	); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}

	// Still no rebuild: the write told us everything a delta needs, three
	// times over.
	before := f.indexReads.Load()
	code, recs := postRecommendBatch(h, placement.ItemDraft{
		Name: "socket set", Category: "tools", SizeBucket: "M", Quantity: 1})
	if code != http.StatusOK || len(recs) != 1 {
		t.Fatalf("recommend status %d, %d recommendations", code, len(recs))
	}
	if spent := f.indexReads.Load() - before; spent != 0 {
		t.Errorf("the recommendation after a multi-item catalog spent %d index calls, want 0", spent)
	}

	fresno, ok := candidateBox(recs[0], "fresno")
	if !ok {
		t.Fatalf("Fresno is not a candidate after three tools went into it: %+v", recs[0])
	}
	// One drill plus two tape measures: 3 items.
	if fresno.Categories["tools"] != 3 {
		t.Errorf("Fresno categories = %v, want tools:3 -- entries after the first were dropped",
			fresno.Categories)
	}
	if fresno.ItemCount != 3 {
		t.Errorf("Fresno itemCount = %d, want 3", fresno.ItemCount)
	}
	modesto, ok := candidateBox(recs[0], "modesto")
	if !ok {
		t.Fatalf("Modesto left the index: %+v", recs[0])
	}
	if modesto.Categories["kitchen"] != 1 || modesto.ItemCount != 1 {
		t.Errorf("Modesto = categories %v, itemCount %d; want kitchen:1 and 1 item -- an entry "+
			"landed on the wrong box or not at all", modesto.Categories, modesto.ItemCount)
	}
}

// /catalog is the write path the echo suppression exists for. Homebox's change
// feed fires on our own writes too, and says nothing but "something changed",
// so an unmuted session would answer each of its own entities with the full
// rebuild every test above this one exists to avoid.
func TestCatalogMutesTheChangeFeedForItsOwnEchoes(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()
	inst := s.defaultInstance()

	getBoxes(h)
	if inst.muted(time.Now()) {
		t.Fatal("muted before anything was written; a foreign edit would be ignored from a cold start")
	}

	if code := postCatalog(h, catalogEntry{
		Item:  placement.ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"},
		BoxID: "fresno",
	}); code != http.StatusCreated {
		t.Fatalf("catalog status %d, want 201", code)
	}

	if !inst.muted(time.Now()) {
		t.Error("not muted once the request returned; the echo arrives after the write does")
	}
	// And it lifts on its own: the quiet window is the only thing holding it.
	if inst.muted(time.Now().Add(selfWriteQuiet + time.Second)) {
		t.Error("still muted past the quiet window, so no foreign edit would ever be seen again")
	}
}

// The pre-warm gate reads lastRead: the watcher rebuilds an index only for a
// Homebox somebody has been reading, so an idle backend makes no unprompted
// upstream calls. A cache HIT has to count, or a session working entirely out
// of a warm index -- the busiest case there is -- would look idle.
func TestBoxIndexStampsLastRead(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()
	inst := s.defaultInstance()

	if inst.readRecently(time.Hour) {
		t.Fatal("an instance nothing has read reports a recent read; a pre-warm would fire on an idle backend")
	}

	if code, _, _ := getBoxes(h); code != http.StatusOK {
		t.Fatalf("boxes status %d, want 200", code)
	}
	if inst.lastRead.Load() == 0 {
		t.Fatal("lastRead is still zero after a read")
	}
	if !inst.readRecently(time.Minute) {
		t.Error("readRecently is false immediately after a read")
	}

	// Backdate past any window, then read again -- from the warm cache, which
	// is the path that returns before the fetch and could miss the stamp.
	inst.lastRead.Store(time.Now().Add(-time.Hour).UnixNano())
	before := f.indexReads.Load()
	getBoxes(h)
	if f.indexReads.Load() != before {
		t.Fatal("the second read rebuilt, so it says nothing about a cache hit")
	}
	if !inst.readRecently(time.Minute) {
		t.Error("a read served from the warm cache did not count as a read")
	}
}
