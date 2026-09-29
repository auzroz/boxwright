package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"boxwright/internal/ai"
	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// loc builds a location row as the live server sends it: entityType carries
// isLocation, the parent is a nested object, and there are NO custom fields --
// list rows never include them.
func loc(id, name, parentID, parentName string) map[string]any {
	e := map[string]any{
		"id": id, "name": name,
		"entityType": map[string]any{"id": "loc-type", "name": "Location", "isLocation": true},
	}
	if parentID != "" {
		e["parent"] = map[string]any{"id": parentID, "name": parentName}
	}
	return e
}

func item(id, name, category string) map[string]any {
	e := map[string]any{
		"id": id, "name": name,
		"entityType": map[string]any{"id": "item-type", "name": "Item", "isLocation": false},
	}
	if category != "" {
		e["fields"] = []any{map[string]any{"name": "category", "type": "text", "textValue": category}}
	}
	return e
}

func page(items ...map[string]any) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{"items": items, "page": 1, "pageSize": 200, "total": len(items)}
}

// chosen renders the custom field that opts a location in to placement, as
// the live server returns it. Tests spell selection out this way because that
// is now the ONLY thing that makes a location a candidate.
// capacity and filled spell out a container the user has measured.
func capacity(litres int) *int    { return &litres }
func filled(pct float64) *float64 { return &pct }

func chosen() map[string]any {
	return map[string]any{"name": "boxwrightPlacement", "type": "text", "textValue": "true"}
}

// fakeHomebox serves a small hierarchy and, crucially, models Homebox's
// fields=Name=Value filter the way the live server actually behaves: exact,
// case-sensitive, TEXT fields only, repeated parameters OR'd -- and a bare
// `fields=Name` with no "=" silently ignored, which returns everything.
type fakeHomebox struct {
	mu        sync.Mutex
	locations []map[string]any // nil uses the default hierarchy below
	details   map[string][]any // entity id -> custom fields for the detail GET
	children  map[string][]map[string]any
	childErr  string // when set, listing this parent's children fails
	// writeErrIDs are entity ids whose PUT fails, so a partial write can be
	// tested: selecting eighty locations is eighty writes.
	writeErrIDs map[string]bool
	calls       atomic.Int32
	// ignoreFieldFilter makes the fake behave like a server with no support
	// for the filter at all: it returns everything. The index must still come
	// out right, because the filter is an optimisation and the per-location
	// fields are the actual check.
	ignoreFieldFilter bool
}

func (f *fakeHomebox) locs() []map[string]any {
	if f.locations != nil {
		return f.locations
	}
	return []map[string]any{
		loc("garage", "Garage", "", ""),
		loc("california", "California", "garage", "Garage"),
		loc("fresno", "Fresno", "california", "California"),
		loc("modesto", "Modesto", "california", "California"),
	}
}

// fieldsOf returns one entity's custom fields.
func (f *fakeHomebox) fieldsOf(id string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]any{}, f.details[id]...)
}

// setFields records a full-replace PUT.
func (f *fakeHomebox) setFields(id string, fields []any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.details == nil {
		f.details = map[string][]any{}
	}
	f.details[id] = fields
}

// matchesFields applies the fields= filter to one location.
func (f *fakeHomebox) matchesFields(id string, pairs []string) bool {
	for _, pair := range pairs {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			// The trap: Homebox does not reject this, it ignores it. A filter
			// made only of these matches everything.
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

func (f *fakeHomebox) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")

		if strings.HasPrefix(r.URL.Path, "/api/v1/entities/") {
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			if r.Method == http.MethodPut {
				if f.writeErrIDs[id] {
					http.Error(w, "write refused", http.StatusInternalServerError)
					return
				}
				var body struct {
					Fields []any `json:"fields"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				f.setFields(id, body.Fields)
				json.NewEncoder(w).Encode(map[string]any{"id": id, "name": id})
				return
			}
			e := map[string]any{"id": id, "name": id,
				"entityType": map[string]any{"id": "loc-type", "isLocation": true}}
			if fields := f.fieldsOf(id); len(fields) > 0 {
				e["fields"] = fields
			}
			json.NewEncoder(w).Encode(e)
			return
		}

		if parents := q["parentIds"]; len(parents) > 0 {
			if f.childErr != "" && parents[0] == f.childErr {
				http.Error(w, "upstream exploded", http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(page(f.children[parents[0]]...))
			return
		}

		if q.Get("isLocation") == "true" {
			out := []map[string]any{}
			pairs := q["fields"]
			anyPair := false
			for _, p := range pairs {
				if strings.Contains(p, "=") {
					anyPair = true
				}
			}
			for _, l := range f.locs() {
				id, _ := l["id"].(string)
				if !anyPair || f.ignoreFieldFilter || f.matchesFields(id, pairs) {
					out = append(out, l)
				}
			}
			json.NewEncoder(w).Encode(page(out...))
			return
		}
		json.NewEncoder(w).Encode(page())
	}
}

// picked is the default hierarchy with the two totes opted in, which most of
// these tests want as their starting point.
func picked() map[string][]any {
	return map[string][]any{
		"fresno":  {chosen()},
		"modesto": {chosen()},
	}
}

func newTestServer(t *testing.T, f *fakeHomebox) *Server {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	hb := homebox.New(srv.URL+"/api", "tok")
	return New(hb, mustProvider(t), time.Minute, discardLogger())
}

func mustProvider(t *testing.T) ai.Identifier {
	t.Helper()
	p, err := ai.New("none", "", "", "")
	if err != nil {
		t.Fatalf("ai.New(none): %v", err)
	}
	return p
}

func decodeBoxes(t *testing.T, body io.Reader) ([]placement.Box, bool) {
	t.Helper()
	var out struct {
		Boxes []placement.Box `json:"boxes"`
		Stale bool            `json:"stale"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Boxes, out.Stale
}

// Placement candidates are the locations the USER chose, and nothing else.
// Garage and California exist and are perfectly real; they are not candidates
// because nobody said they were.
func TestOnlyChosenLocationsAreCandidates(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	boxes, _ := decodeBoxes(t, rr.Body)

	got := map[string]placement.Box{}
	for _, b := range containers(boxes) {
		got[b.Name] = b
	}
	if len(got) != 2 {
		t.Fatalf("got %d containers %v, want only the two that were chosen", len(got), keys(got))
	}
	for _, bad := range []string{"Garage", "California"} {
		if _, present := got[bad]; present {
			t.Errorf("%q was never chosen and must not be a placement candidate", bad)
		}
	}
	// They ARE in the index, as areas, which is where a lawn mower goes. That
	// is a different question from whether a drill may be put in them.
	names := map[string]bool{}
	for _, b := range areas(boxes) {
		names[b.Name] = true
	}
	if !names["Garage"] || !names["California"] {
		t.Errorf("the locations above the chosen ones are missing as areas: %v", names)
	}
	// Area is derived from the row the location sits in, not stored.
	if got["Fresno"].Area != "California" {
		t.Errorf("Fresno.Area = %q, want \"California\"", got["Fresno"].Area)
	}
	if got["Fresno"].ParentID != "california" {
		t.Errorf("Fresno.ParentID = %q, want \"california\"", got["Fresno"].ParentID)
	}
}

// The regression that this whole design exists for.
//
// A new user's Homebox is not a Room > Row > Tote tree. It is twenty boxes at
// the top level with nothing in them yet. Every rule the old code used to
// infer a container from -- has no location children, is not top-level, is
// annotated, holds items -- rejected all twenty, so the engine reported that
// no existing box was a good fit and suggested buying another one, forever.
//
// Selecting them is all it takes now, and selecting them is a thing the user
// does rather than a shape we recognise.
func TestFlatInventoryOfEmptyTopLevelBoxes(t *testing.T) {
	locs := []map[string]any{}
	details := map[string][]any{}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("box%02d", i)
		locs = append(locs, loc(id, "Box "+id, "", ""))
	}
	f := &fakeHomebox{locations: locs, details: details, children: map[string][]map[string]any{}}
	s := newTestServer(t, f)
	h := s.Routes()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	before, _ := decodeBoxes(t, rr.Body)
	if len(before) != 0 {
		t.Fatalf("got %d candidates before anything was chosen, want 0", len(before))
	}

	// The user opens the picker and ticks all twenty.
	for i := 0; i < 20; i++ {
		details[fmt.Sprintf("box%02d", i)] = []any{chosen()}
	}
	s.defaultInstance().invalidate()

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	after, _ := decodeBoxes(t, rr.Body)
	if len(after) != 20 {
		t.Fatalf("got %d candidates after choosing all twenty, want 20", len(after))
	}
}

// A location that holds other locations can still be a place you put things.
// "Garage", "Attic", "Shelf in Pantry" -- plenty of people file at that level,
// and the old leaf rule made every one of them impossible.
func TestChosenLocationWithChildrenIsACandidate(t *testing.T) {
	f := &fakeHomebox{
		children: map[string][]map[string]any{},
		details:  map[string][]any{"garage": {chosen()}},
	}
	s := newTestServer(t, f)

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	boxes, _ := decodeBoxes(t, rr.Body)
	if len(boxes) != 1 || boxes[0].Name != "Garage" {
		t.Fatalf("got %v, want Garage alone -- holding other locations is not a disqualification", boxes)
	}
}

// The fields= query narrows what we have to read; it is not what decides.
// Against a server that ignores the filter and returns everything, the index
// must come out identical, because each location's own fields are re-read.
func TestFieldFilterIsAnOptimisationNotTheCheck(t *testing.T) {
	f := &fakeHomebox{
		children: map[string][]map[string]any{}, details: picked(), ignoreFieldFilter: true,
	}
	s := newTestServer(t, f)

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	boxes, _ := decodeBoxes(t, rr.Body)
	if len(containers(boxes)) != 2 {
		t.Fatalf("got %d containers %v, want the 2 chosen ones even though the filter was ignored",
			len(containers(boxes)), containers(boxes))
	}
}

// The first real use of Boxwright, end to end: a chosen container holding a
// dozen tools, capacity never recorded. The index charged each as an M item (2
// units) against an assumed 8, read it as full, and the engine excluded it --
// so a hammer was told to start a new container beside a box of tools.
func TestAContainerWithNoRecordedCapacityIsOfferedHoweverMuchItHolds(t *testing.T) {
	kids := []map[string]any{}
	for i := 0; i < 12; i++ {
		kids = append(kids, taggedItem(fmt.Sprintf("tool%02d", i), 1, "tools"))
	}
	f := &fakeHomebox{
		children: map[string][]map[string]any{"fresno": kids},
		details:  map[string][]any{"fresno": {chosen()}, "modesto": {chosen()}},
	}
	s := newTestServer(t, f)

	code, rec := postRecommend(s.Routes(), placement.ItemDraft{Name: "hammer", Category: "tools", SizeBucket: "M"})
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(rec.Candidates) == 0 || rec.Candidates[0].Box.ID != "fresno" {
		t.Fatalf("the box holding twelve tools was not the first candidate: %+v", rec)
	}
	if rec.NewContainer != nil {
		t.Errorf("suggested a new container beside a box of tools: %+v", rec.NewContainer)
	}
	fresno := rec.Candidates[0].Box
	if fresno.ItemCount != 12 || fresno.UsedUnits != 0 || fresno.CapacityUnits != 0 {
		t.Errorf("Fresno = itemCount %d, usedUnits %d, capacityUnits %d; want 12, 0, 0 (counted, not estimated)",
			fresno.ItemCount, fresno.UsedUnits, fresno.CapacityUnits)
	}
}

// The end-to-end shape of a live bug: a container really holding
// fragileSafe=true was reported by the backend as false.
//
// capacityUnits is present here and must NOT be read: its values in the wild
// were Boxwright's own bucket guesses, and read back as a recorded capacity
// they excluded established containers from every recommendation.
func TestBoxIndexReadsTypedCustomFields(t *testing.T) {
	f := &fakeHomebox{
		children: map[string][]map[string]any{},
		details: map[string][]any{
			"fresno": {
				chosen(),
				map[string]any{"name": "capacityUnits", "type": "number", "numberValue": 8},
				map[string]any{"name": "fragileSafe", "type": "boolean", "booleanValue": true},
				map[string]any{"name": "access", "type": "text", "textValue": "easy"},
			},
			"modesto": {chosen()},
		},
	}
	s := newTestServer(t, f)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	boxes, _ := decodeBoxes(t, rr.Body)

	var fresno placement.Box
	for _, b := range boxes {
		if b.Name == "Fresno" {
			fresno = b
		}
	}
	if fresno.CapacityUnits != 0 {
		t.Errorf("capacityUnits = %d, want 0: the legacy field is a guess, not a recorded capacity", fresno.CapacityUnits)
	}
	if fresno.FragileSafe == nil || !*fresno.FragileSafe {
		t.Errorf("fragileSafe = %v, want a recorded true", fresno.FragileSafe)
	}
	if fresno.Access != "easy" {
		t.Errorf("access = %q, want \"easy\"", fresno.Access)
	}
	// A tote with no metadata must come back unknown, NOT false.
	for _, b := range boxes {
		if b.Name == "Modesto" && (b.FragileSafe != nil || b.HeavySafe != nil) {
			t.Errorf("Modesto has no metadata; want nil flags, got fragile=%v heavy=%v",
				b.FragileSafe, b.HeavySafe)
		}
	}
}

// A box whose contents could not be listed looks like maximum headroom to the
// engine and can win a ranking it should have lost. Failing the refresh lets
// the cache serve the last good index with stale=true instead.
func TestChildListingFailureFailsRefreshRatherThanDroppingABox(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked(), childErr: "fresno"}
	s := newTestServer(t, f)

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 with no cache to fall back on: %s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "Modesto") {
		t.Error("a partial index was served; the whole refresh should have failed")
	}
}

// Offline tolerance: once one fetch has succeeded, a later failure serves the
// previous index with stale=true rather than a 502.
func TestStaleCacheServedAfterRefreshFailure(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	first, stale := decodeBoxes(t, rr.Body)
	if stale || len(containers(first)) != 2 {
		t.Fatalf("first fetch: stale=%v containers=%d", stale, len(containers(first)))
	}

	f.childErr = "fresno"            // Homebox goes bad
	s.defaultInstance().invalidate() // as a catalog write would

	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 serving stale data", rr.Code)
	}
	got, stale := decodeBoxes(t, rr.Body)
	if !stale {
		t.Error("stale flag not set while serving a cached index")
	}
	if len(containers(got)) != 2 {
		t.Errorf("served %d cached containers, want 2", len(containers(got)))
	}
}

// An inventory with zero boxes is a legitimate answer, not a cache miss. The
// old `inst.boxes != nil` guard made an empty index refetch on every request.
func TestEmptyInventoryStillCachesAndMarshalsAsArray(t *testing.T) {
	empty := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(page())
	}
	srv := httptest.NewServer(http.HandlerFunc(empty))
	t.Cleanup(srv.Close)
	calls := 0
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		empty(w, r)
	}))
	t.Cleanup(counted.Close)

	s := New(homebox.New(counted.URL+"/api", "tok"), mustProvider(t), time.Minute, discardLogger())
	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"boxes":[]`) {
			t.Errorf("want boxes:[] not null, got %s", rr.Body.String())
		}
	}
	if calls != 1 {
		t.Errorf("upstream called %d times for 3 requests; an empty index defeated the cache", calls)
	}
}

func TestEntityTypesEndpointExposesLocationType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]homebox.EntityType{
			{ID: "item-type", Name: "Item", IsLocation: false},
			{ID: "loc-type", Name: "Location", IsLocation: true},
		})
	}))
	t.Cleanup(srv.Close)
	s := New(homebox.New(srv.URL+"/api", "tok"), mustProvider(t), time.Minute, discardLogger())

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/entity-types", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var out struct {
		EntityTypes []homebox.EntityType `json:"entityTypes"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	found := false
	for _, et := range out.EntityTypes {
		if et.IsLocation {
			found = true
		}
	}
	if !found {
		t.Error("no location type exposed; the new-container flow cannot create a box without one")
	}
}

// Principle 3: with no AI provider the app must still be fully usable, which
// means a specific, actionable 422 rather than a generic failure.
func TestIdentifyWithoutProviderReturns422(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)

	body, ct := multipartImage(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "manually") {
		t.Errorf("422 should tell the client to fall back to manual entry, got %s", rr.Body)
	}
}

func multipartImage(t *testing.T) (io.Reader, string) {
	t.Helper()
	var b strings.Builder
	const boundary = "testboundary"
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString(`Content-Disposition: form-data; name="image"; filename="c.jpg"` + "\r\n")
	b.WriteString("Content-Type: image/jpeg\r\n\r\n")
	b.WriteString("notarealjpeg\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return strings.NewReader(b.String()), "multipart/form-data; boundary=" + boundary
}

// containers drops the AREAS -- the garage above the totes -- which the index
// now also carries. They are never offered for something that goes in a
// container, so a test about placement candidates means these.
func containers(boxes []placement.Box) []placement.Box {
	out := []placement.Box{}
	for _, b := range boxes {
		if !b.IsArea {
			out = append(out, b)
		}
	}
	return out
}

func areas(boxes []placement.Box) []placement.Box {
	out := []placement.Box{}
	for _, b := range boxes {
		if b.IsArea {
			out = append(out, b)
		}
	}
	return out
}

func keys(m map[string]placement.Box) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

var _ = url.Values{}

// A catalog write landing mid-rebuild must not be overwritten by a snapshot
// taken before it. Without a generation check the rebuild commits regardless,
// invalidate() is lost, and the just-filed item stays invisible for a whole
// cache TTL -- while the response says stale=false, contradicting the only
// staleness signal the app shows the user.
func TestInvalidationDuringRefreshIsNotLost(t *testing.T) {
	release := make(chan struct{})
	var gate sync.Once
	var listCalls atomic.Int32
	extraLoc := atomic.Bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/entities/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			// Hold the very first detail fetch open so a write can land while
			// this rebuild is still running.
			gate.Do(func() { <-release })
			e := map[string]any{"id": id, "name": id}
			// This handler ignores the fields= filter entirely, so the list
			// below hands back the row as well as the totes. Only the two
			// totes say they are placement targets, and that is what decides.
			if id == "fresno" || id == "modesto" {
				e["fields"] = []any{chosen()}
			}
			json.NewEncoder(w).Encode(e)
		case r.URL.Query().Get("isLocation") == "true":
			listCalls.Add(1)
			items := []map[string]any{loc("fresno", "Fresno", "row", "California"), loc("row", "California", "", "")}
			if extraLoc.Load() {
				items = append(items, loc("modesto", "Modesto", "row", "California"))
			}
			json.NewEncoder(w).Encode(page(items...))
		default:
			json.NewEncoder(w).Encode(page())
		}
	}))
	t.Cleanup(srv.Close)

	s := New(homebox.New(srv.URL+"/api", "tok"), mustProvider(t), time.Hour, discardLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	}()

	// While that rebuild is blocked, a write lands and invalidates.
	time.Sleep(50 * time.Millisecond)
	extraLoc.Store(true)
	s.defaultInstance().invalidate()
	close(release)
	<-done

	// The next read must go back to Homebox rather than serve the pre-write
	// snapshot the in-flight rebuild would otherwise have committed.
	before := listCalls.Load()
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	boxes, stale := decodeBoxes(t, rr.Body)

	if listCalls.Load() == before {
		t.Fatal("served the cache; the invalidation was lost to the in-flight rebuild")
	}
	if stale {
		t.Error("stale=true on a successful fresh read")
	}
	names := map[string]bool{}
	for _, b := range boxes {
		names[b.Name] = true
	}
	if !names["Modesto"] {
		t.Errorf("the box created during the rebuild is missing: %v", names)
	}
}

// The backend holds a Homebox key and can write to real inventory, so once it
// is reachable from the network it must not be usable without the secret.
func TestAPITokenGatesWritesButNotHealth(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)
	s.RequireToken("s3cret")
	h := s.Routes()

	// A reachability probe needs no credential.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("/healthz status %d, want 200 without a token", rr.Code)
	}

	for _, tc := range []struct {
		name, header string
		want         int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"token without prefix", "s3cret", http.StatusOK},
		{"correct token", "Bearer s3cret", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Errorf("status %d, want %d: %s", rr.Code, tc.want, rr.Body)
			}
		})
	}
}

// With no token configured (the loopback default) nothing is gated.
func TestNoTokenMeansNoGate(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("status %d, want 200", rr.Code)
	}
}

// stubIdentifier answers with a fixed result, so the handler's half of the
// array contract can be tested without a model.
type stubIdentifier struct {
	items []placement.ItemDraft
	err   error
}

func (s stubIdentifier) Identify(context.Context, []byte, string, []placement.Category) ([]placement.ItemDraft, error) {
	return s.items, s.err
}

func identifyServer(t *testing.T, id ai.Identifier) *Server {
	t.Helper()
	f := &fakeHomebox{children: map[string][]map[string]any{}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return New(homebox.New(srv.URL+"/api", "tok"), id, time.Minute, discardLogger())
}

func postIdentify(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartImage(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	return rr
}

// The photo of a shelf, all the way through the handler. Every object the
// model recognised has to reach the review screen: the drill winning the frame
// is not a reason to drop the tape measure and the level.
func TestIdentifyReturnsEveryItemInThePhoto(t *testing.T) {
	s := identifyServer(t, stubIdentifier{items: []placement.ItemDraft{
		{Name: "cordless drill", Category: "tools", SizeBucket: "M"},
		{Name: "tape measure", Category: "tools", SizeBucket: "S"},
		{Name: "spirit level", Category: "tools", SizeBucket: "L"},
	}})

	rr := postIdentify(t, s)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var out struct {
		Items []placement.ItemDraft `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(out.Items), out.Items)
	}
	if out.Items[1].Name != "tape measure" {
		t.Errorf("items[1] = %q; the order the model listed them is the order to review them in",
			out.Items[1].Name)
	}
}

// A single item is the common case and must not be a special case: it is a
// one-element array, under the same key.
func TestIdentifyWrapsASingleItemInTheArray(t *testing.T) {
	s := identifyServer(t, stubIdentifier{items: []placement.ItemDraft{
		{Name: "cordless drill", Category: "tools", SizeBucket: "M"},
	}})

	rr := postIdentify(t, s)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), `"items":[{`) {
		t.Errorf("want an items array even for one item, got %s", rr.Body)
	}
}

// A model that saw nothing packable has answered; there is simply nothing to
// review. That is the same dead end as having no provider, so it takes the
// same 422 manual-entry path rather than a 200 with an empty list the app
// would have to invent a second empty state for.
func TestIdentifyWithNothingRecognisedReturns422(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []placement.ItemDraft
	}{
		{"empty array", []placement.ItemDraft{}},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := identifyServer(t, stubIdentifier{items: tc.items})
			rr := postIdentify(t, s)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", rr.Code, rr.Body)
			}
			if !strings.Contains(rr.Body.String(), "manually") {
				t.Errorf("422 should point at manual entry, got %s", rr.Body)
			}
		})
	}
}

// seededServer is a Server whose box index is already warm, so a recommend
// test can state the shelf exactly and never reach Homebox for it.
func seededServer(t *testing.T, boxes []placement.Box) *Server {
	t.Helper()
	s := New(nil, mustProvider(t), time.Hour, discardLogger())
	inst := s.defaultInstance()
	inst.mu.Lock()
	inst.boxes = boxes
	inst.loaded = true
	inst.fetched = time.Now()
	inst.mu.Unlock()
	return s
}

// The bug the batch endpoint exists to prevent: scored one at a time, every
// item from one photo is told to use the same tote, because none of them can
// see that the others are about to go in it.
func TestRecommendChargesEachItemForTheOnesBeforeIt(t *testing.T) {
	s := seededServer(t, []placement.Box{
		{ID: "fresno", Name: "Fresno", Area: "California", Access: "easy",
			CapacityL: capacity(60), FillPct: filled(50), FillSource: placement.FillObserved,
			ItemCount: 2, Categories: map[string]int{"tools": 2}},
		{ID: "modesto", Name: "Modesto", Area: "California", Access: "easy",
			CapacityL: capacity(60), FillPct: filled(0), FillSource: placement.FillObserved,
			Categories: map[string]int{}},
	})

	// Two L tools, 25 L each. Fresno, 60 L and seen half full, has room for
	// exactly one of them.
	code, recs := postRecommendBatch(s.Routes(),
		placement.ItemDraft{Name: "spirit level", Category: "tools", SizeBucket: "L"},
		placement.ItemDraft{Name: "circular saw", Category: "tools", SizeBucket: "L"},
	)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d recommendations for 2 items", len(recs))
	}

	if got := recs[0].Candidates[0].Box.ID; got != "fresno" {
		t.Errorf("item 0 was sent to %q, want the tote that already holds tools", got)
	}
	if len(recs[1].Candidates) == 0 {
		t.Fatalf("item 1 got no candidates at all: %+v", recs[1])
	}
	if got := recs[1].Candidates[0].Box.ID; got != "modesto" {
		t.Errorf("item 1 was sent to %q; the capacity item 0 just consumed was not charged for", got)
	}
	if _, ok := candidateBox(recs[1], "fresno"); ok {
		t.Error("Fresno is full once item 0 is in it and must not be a candidate for item 1")
	}

	// A read handler must not write through the shared index: it is patched
	// elsewhere under a generation counter, and a reservation leaking into it
	// would charge every later request for placements nobody accepted.
	inst := s.defaultInstance()
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	fresno, _ := findBox(inst.boxes, "fresno")
	if *fresno.FillPct != 50 || fresno.ItemCount != 2 || fresno.Categories["tools"] != 2 {
		t.Errorf("the cached index was mutated by a recommendation: %+v", fresno)
	}
}

// Capacity is only half of it. The category an earlier item adds has to count
// towards the next item's like-with-like affinity, which is how one photo of a
// toolbox produces one tote of tools instead of tools scattered by tie-break.
func TestRecommendCarriesCategoryAffinityAcrossOnePhoto(t *testing.T) {
	s := seededServer(t, []placement.Box{
		{ID: "alpha", Name: "Alpha", Access: "easy", CapacityL: capacity(100), Categories: map[string]int{}},
		{ID: "bravo", Name: "Bravo", Access: "easy", CapacityL: capacity(100), Categories: map[string]int{}},
	})

	code, recs := postRecommendBatch(s.Routes(),
		placement.ItemDraft{Name: "socket set", Category: "tools", SizeBucket: "S"},
		placement.ItemDraft{Name: "tape measure", Category: "tools", SizeBucket: "S"},
	)
	if code != http.StatusOK || len(recs) != 2 {
		t.Fatalf("status %d, %d recommendations", code, len(recs))
	}

	first := recs[0].Candidates[0].Box.ID
	if got := recs[1].Candidates[0].Box.ID; got != first {
		t.Errorf("item 1 went to %q while item 0 went to %q; two identical tools split up", got, first)
	}
	if !hasReason(recs[1].Candidates[0].Reasons, "already holds tools") {
		t.Errorf("item 1's reasons %v never mention the tools item 0 just put there",
			recs[1].Candidates[0].Reasons)
	}
	// Item 0 was scored against an empty box and its recommendation still has
	// to say so. If the reservation had mutated the Categories map in place
	// instead of replacing it, this recommendation -- marshalled after the
	// whole batch ran -- would have been rewritten behind the user's back.
	if n := recs[0].Candidates[0].Box.Categories["tools"]; n != 0 {
		t.Errorf("item 0's candidate reports tools:%d; the reservation was written through it", n)
	}
	if n := recs[1].Candidates[0].Box.Categories["tools"]; n != 1 {
		t.Errorf("item 1's candidate reports tools:%d, want the 1 item 0 reserved", n)
	}
}

// A batch that ends in a new-container suggestion must not charge the next
// item for capacity in a box the engine just advised against using.
func TestRecommendReservesNothingWhenItSuggestsANewContainer(t *testing.T) {
	// One tote, full of clothing and nearly full by volume: nothing about it
	// fits a heavy tool, so both items should be told to start a container.
	s := seededServer(t, []placement.Box{
		{ID: "fresno", Name: "Fresno", Access: "deep",
			CapacityL: capacity(100), FillPct: filled(75), FillSource: placement.FillObserved,
			ItemCount: 20, Categories: map[string]int{"clothing": 20}},
	})

	code, recs := postRecommendBatch(s.Routes(),
		placement.ItemDraft{Name: "cordless drill", Category: "tools", SizeBucket: "M"},
		placement.ItemDraft{Name: "spirit level", Category: "tools", SizeBucket: "M"},
	)
	if code != http.StatusOK || len(recs) != 2 {
		t.Fatalf("status %d, %d recommendations", code, len(recs))
	}
	for i, rec := range recs {
		if rec.NewContainer == nil {
			t.Fatalf("recs[%d] found a viable box in a clothing tote: %+v", i, rec)
		}
	}
	// Item 1 must still see the same box it would have seen alone: nobody
	// accepted a placement into it.
	if got := recs[1].Candidates[0].Box; *got.FillPct != 75 || got.ItemCount != 20 {
		t.Errorf("Fresno = fill %v, %d items for item 1, want 75 and 20: a rejected suggestion was charged for",
			*got.FillPct, got.ItemCount)
	}
}

// recommendations[i] is items[i] -- the app pairs them by position -- and the
// same request has to produce the same bytes every time. Map iteration order
// inside the engine or the batch would show up here as a flapping answer the
// user could never reproduce.
func TestRecommendIsPositionalAndDeterministic(t *testing.T) {
	boxes := []placement.Box{
		{ID: "alpha", Name: "Alpha", Access: "easy",
			CapacityL: capacity(100), FillPct: filled(25), FillSource: placement.FillObserved,
			ItemCount: 2, Categories: map[string]int{"tools": 1, "kitchen": 1}},
		{ID: "bravo", Name: "Bravo", Access: "deep",
			CapacityL: capacity(100), FillPct: filled(25), FillSource: placement.FillObserved,
			ItemCount: 2, Categories: map[string]int{"kitchen": 1, "tools": 1}},
		{ID: "charlie", Name: "Charlie", CapacityL: capacity(100), Categories: map[string]int{}},
	}
	items := []placement.ItemDraft{
		{Name: "socket set", Category: "tools", SizeBucket: "M"},
		{Name: "mixing bowl", Category: "kitchen", SizeBucket: "M", Fragile: true},
		{Name: "spirit level", Category: "tools", SizeBucket: "L"},
		{Name: "dinner plates", Category: "kitchen", SizeBucket: "S", Quantity: 8},
	}

	var want string
	for i := 0; i < 12; i++ {
		s := seededServer(t, boxes)
		code, recs := postRecommendBatch(s.Routes(), items...)
		if code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		if len(recs) != len(items) {
			t.Fatalf("got %d recommendations for %d items", len(recs), len(items))
		}
		got, err := json.Marshal(recs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i == 0 {
			want = string(got)
			continue
		}
		if string(got) != want {
			t.Fatalf("run %d differs from run 0:\n%s\n%s", i, want, got)
		}
	}
}

func TestRecommendWithNoItemsIsRefused(t *testing.T) {
	s := seededServer(t, []placement.Box{})
	code, _ := postRecommendBatch(s.Routes())
	if code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for a request naming nothing to place", code)
	}
}

func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

// What a user recorded about a container comes back from /boxes as they
// recorded it, and the types are derived from it -- nothing shipped, nothing
// stored anywhere but the containers themselves.
func TestBoxIndexReadsWhatTheUserRecordedAboutAContainer(t *testing.T) {
	tote := func(fill float64, source string) []any {
		return []any{
			chosen(),
			map[string]any{"name": "containerType", "type": "text", "textValue": "27-gallon tote"},
			map[string]any{"name": "capacityL", "type": "number", "numberValue": 100},
			map[string]any{"name": "interiorCm", "type": "text", "textValue": "70 x 45 x 38"},
			map[string]any{"name": "fillPct", "type": "number", "numberValue": fill},
			map[string]any{"name": "fillSource", "type": "text", "textValue": source},
			map[string]any{"name": "fillCheckedAt", "type": "text", "textValue": "2026-09-28T20:00:00Z"},
		}
	}
	f := &fakeHomebox{
		children: map[string][]map[string]any{},
		details:  map[string][]any{"fresno": tote(40, "observed"), "modesto": {chosen()}},
	}
	s := newTestServer(t, f)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))

	var out struct {
		Boxes          []placement.Box           `json:"boxes"`
		ContainerTypes []placement.ContainerType `json:"containerTypes"`
		SizeLitres     map[string]float64        `json:"sizeLitres"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var fresno, modesto placement.Box
	for _, b := range out.Boxes {
		switch b.ID {
		case "fresno":
			fresno = b
		case "modesto":
			modesto = b
		}
	}
	if fresno.ContainerType != "27-gallon tote" || fresno.CapacityL == nil || *fresno.CapacityL != 100 {
		t.Errorf("Fresno type/capacity = %q/%v", fresno.ContainerType, fresno.CapacityL)
	}
	if fresno.InteriorCm == nil || fresno.InteriorCm.String() != "70x45x38" {
		t.Errorf("Fresno interior = %v, want 70x45x38 parsed from what was typed", fresno.InteriorCm)
	}
	if fresno.FillPct == nil || *fresno.FillPct != 40 || fresno.FillSource != "observed" || fresno.FillCheckedAt == "" {
		t.Errorf("Fresno fill = %v %q %q", fresno.FillPct, fresno.FillSource, fresno.FillCheckedAt)
	}
	// Legacy clients get units derived from what is known: 100 L at 40%.
	if fresno.CapacityUnits != 8 || fresno.UsedUnits != 3 {
		t.Errorf("Fresno legacy units = %d/%d, want 3/8", fresno.UsedUnits, fresno.CapacityUnits)
	}
	// Nothing recorded is unknown -- nil, not zero.
	if modesto.CapacityL != nil || modesto.FillPct != nil || modesto.InteriorCm != nil || modesto.CapacityUnits != 0 {
		t.Errorf("Modesto has nothing recorded; got %+v", modesto)
	}
	if len(out.ContainerTypes) != 1 || out.ContainerTypes[0].Name != "27-gallon tote" || out.ContainerTypes[0].CapacityL != 100 {
		t.Errorf("containerTypes = %+v", out.ContainerTypes)
	}
	if out.SizeLitres["XL"] != placement.NominalLitres["XL"] {
		t.Errorf("sizeLitres = %v, want the engine's own figures", out.SizeLitres)
	}
}
