package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"boxwright/internal/bootstrap"
)

func getLocations(t *testing.T, h http.Handler) ([]locationNode, int) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/locations", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var out struct {
		Locations     []locationNode `json:"locations"`
		EligibleCount int            `json:"eligibleCount"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Locations, out.EligibleCount
}

func putLocations(t *testing.T, h http.Handler, body string) []locationResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/locations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var out struct {
		Results []locationResult `json:"results"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Results
}

// The picker has to show EVERY location, whatever its shape, or the user
// cannot choose the ones this code used to refuse to consider.
func TestLocationsListsEveryLocationWithItsSelection(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	nodes, count := getLocations(t, newTestServer(t, f).Routes())

	if len(nodes) != 4 {
		t.Fatalf("got %d locations, want all 4 including the ones holding other locations", len(nodes))
	}
	if count != 2 {
		t.Errorf("eligibleCount = %d, want 2", count)
	}
	by := map[string]locationNode{}
	for _, n := range nodes {
		by[n.Name] = n
	}
	if !by["Fresno"].Eligible || by["Garage"].Eligible {
		t.Errorf("selection wrong: Fresno=%v Garage=%v", by["Fresno"].Eligible, by["Garage"].Eligible)
	}
	// Context for the person choosing, never a filter.
	if !by["California"].HasChildren {
		t.Error("California holds the totes; the picker should be able to say so")
	}
	if by["Fresno"].Path != "Garage > California > Fresno" || by["Fresno"].Depth != 2 {
		t.Errorf("Fresno path=%q depth=%d, want the full path and depth 2", by["Fresno"].Path, by["Fresno"].Depth)
	}
	// Parents before children, which is the order a picker renders.
	if nodes[0].Name != "Garage" {
		t.Errorf("first row is %q, want Garage -- rows are ordered by path", nodes[0].Name)
	}
}

// Choosing a location makes it a candidate, and un-choosing it stops it being
// one. Both directions, because storing only "true" would make the choice
// impossible to reverse.
func TestPutLocationsChangesWhatTheEngineCanSee(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	h := newTestServer(t, f).Routes()

	names := func() map[string]bool {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
		boxes, _ := decodeBoxes(t, rr.Body)
		out := map[string]bool{}
		// Containers only: the areas above them are in the index too, but for
		// bulky items, not as somewhere the user chose to file into.
		for _, b := range containers(boxes) {
			out[b.Name] = true
		}
		return out
	}

	if got := names(); got["Garage"] || !got["Fresno"] {
		t.Fatalf("starting point wrong: %v", got)
	}

	res := putLocations(t, h, `{"locations":[{"id":"garage","eligible":true},{"id":"fresno","eligible":false}]}`)
	for _, r := range res {
		if r.Error != "" {
			t.Fatalf("%s: %s", r.ID, r.Error)
		}
	}

	// No explicit invalidate here: the handler must have done it, or the app
	// would show a stale answer for a whole TTL after the user chose.
	got := names()
	if !got["Garage"] {
		t.Errorf("Garage was chosen and is still not a candidate: %v", got)
	}
	if got["Fresno"] {
		t.Errorf("Fresno was turned off and is still a candidate: %v", got)
	}
}

// Eighty locations is eighty writes, so partial failure is the normal case and
// has to be reported per location rather than collapsed into a status code.
func TestPutLocationsReportsFailuresPerLocation(t *testing.T) {
	f := &fakeHomebox{
		children:    map[string][]map[string]any{},
		details:     picked(),
		writeErrIDs: map[string]bool{"california": true},
	}
	h := newTestServer(t, f).Routes()

	res := putLocations(t, h,
		`{"locations":[{"id":"garage","eligible":true},{"id":"california","eligible":true},{"id":"","eligible":true}]}`)
	if len(res) != 3 {
		t.Fatalf("got %d results for 3 locations", len(res))
	}
	if res[0].Error != "" {
		t.Errorf("garage should have succeeded: %s", res[0].Error)
	}
	if res[1].Error == "" {
		t.Error("california's write failed upstream and was reported as a success")
	}
	if res[2].Error == "" {
		t.Error("an empty id was accepted")
	}
	// The one that worked still has to take effect.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	boxes, _ := decodeBoxes(t, rr.Body)
	found := false
	for _, b := range boxes {
		if b.Name == "Garage" {
			found = true
		}
	}
	if !found {
		t.Error("a partial failure threw away the writes that succeeded")
	}
}

func TestPutLocationsWithNoLocationsIsRefused(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/locations", strings.NewReader(`{"locations":[]}`))
	rr := httptest.NewRecorder()
	newTestServer(t, f).Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rr.Code)
	}
}

// The migration for someone upgrading: locations already annotated as
// containers are adopted, so eighty of them do not have to be ticked by hand.
// It must not touch a location the user has already decided about -- in either
// direction -- because re-deriving eligibility from metadata on every run is
// precisely the inference this replaced.
func TestAdoptTakesAnnotatedLocationsAndLeavesDecisionsAlone(t *testing.T) {
	f := &fakeHomebox{
		children: map[string][]map[string]any{},
		details: map[string][]any{
			// Annotated by an older version, no decision recorded: adopt it.
			"fresno": {map[string]any{"name": "capacityUnits", "type": "number", "numberValue": 8}},
			// Annotated, but the user turned it off. Leave it off.
			"modesto": {
				map[string]any{"name": "access", "type": "text", "textValue": "deep"},
				map[string]any{"name": "boxwrightPlacement", "type": "text", "textValue": "false"},
			},
			// Not annotated at all: not our business.
			"california": {},
		},
	}
	h := newTestServer(t, f).Routes()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/locations/adopt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var out struct {
		Scanned int               `json:"scanned"`
		Marked  int               `json:"marked"`
		Adopted []adoptedLocation `json:"adopted"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	if out.Marked != 1 || len(out.Adopted) != 1 || out.Adopted[0].ID != "fresno" {
		t.Fatalf("adopted %+v (marked %d), want Fresno alone", out.Adopted, out.Marked)
	}
	if out.Scanned != 4 {
		t.Errorf("scanned %d, want all 4 locations", out.Scanned)
	}

	nodes, count := getLocations(t, h)
	if count != 1 {
		t.Fatalf("eligibleCount = %d after adopting, want 1", count)
	}
	for _, n := range nodes {
		if n.Name == "Modesto" && n.Eligible {
			t.Error("adopt overrode an explicit no")
		}
	}

	// Idempotent: a second run has nothing left to do.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/locations/adopt", nil))
	out.Marked = -1
	json.NewDecoder(rr.Body).Decode(&out)
	if out.Marked != 0 {
		t.Errorf("a second adopt marked %d more; it is meant to be idempotent", out.Marked)
	}
}

// Boxwright creating the container IS the user saying to put things there.
// Without this the new tote is invisible to the engine as soon as the cache
// rebuilds, and the next item off the same shelf is told to create another.
func TestNewContainersAreOptedInAsTheyAreCreated(t *testing.T) {
	want := placementField(true)
	if want.Type != "text" || want.TextValue != placementYes {
		t.Fatalf("placementField(true) = %+v", want)
	}
	off := placementField(false)
	if off.TextValue != placementNo {
		t.Fatalf("placementField(false) = %+v; turning a location off has to be recordable", off)
	}
}

// bootstrap cannot import this package, so it spells the field name out. If
// the two ever drift, an outline would create locations the index never sees.
func TestBootstrapAgreesWithTheAPIOnThePlacementField(t *testing.T) {
	roots, err := bootstrap.Parse(strings.NewReader("Garage\n  Fresno\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got string
	for _, f := range roots[0].Children[0].Metadata(8, "normal", true) {
		if f.Name == fieldPlacement {
			got = f.TextValue
		}
	}
	if got != placementYes {
		t.Fatalf("bootstrap writes %s=%q, api reads %s=%q", fieldPlacement, got, fieldPlacement, placementYes)
	}
}

// A picker on an instance with hundreds of locations must not cost hundreds of
// requests, and neither must a rebuild for a user who chose three of them.
func TestReadPathsDoNotScaleWithTheWholeInventory(t *testing.T) {
	locs := []map[string]any{}
	for i := 0; i < 200; i++ {
		locs = append(locs, loc(fmt.Sprintf("l%03d", i), fmt.Sprintf("Location %03d", i), "", ""))
	}
	f := &fakeHomebox{
		locations: locs,
		children:  map[string][]map[string]any{},
		details:   map[string][]any{"l007": {chosen()}},
	}
	s := newTestServer(t, f)
	h := s.Routes()

	getLocations(t, h)
	if n := f.calls.Load(); n > 4 {
		t.Errorf("the picker made %d requests for 200 locations; it should be a listing plus the selection", n)
	}

	f.calls.Store(0)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	boxes, _ := decodeBoxes(t, rr.Body)
	if len(boxes) != 1 {
		t.Fatalf("got %d boxes, want the 1 that was chosen", len(boxes))
	}
	if n := f.calls.Load(); n > 5 {
		t.Errorf("a rebuild for 1 chosen location out of 200 made %d requests", n)
	}
}

// The other write path that echoes back, and the worse of the two: eighty
// locations is eighty writes and ONE correct invalidation, so unmuted it is
// also eighty mutation frames, each able to invalidate again and kill the
// rebuild that one correct invalidation started.
func TestPutLocationsMutesTheChangeFeedForItsOwnEchoes(t *testing.T) {
	f := &fakeHomebox{children: map[string][]map[string]any{}, details: picked()}
	s := newTestServer(t, f)
	inst := s.defaultInstance()

	if inst.muted(time.Now()) {
		t.Fatal("muted before anything was written")
	}
	putLocations(t, s.Routes(), `{"locations":[{"id":"garage","eligible":true}]}`)

	if !inst.muted(time.Now()) {
		t.Error("not muted once the selection returned; its own echoes would rebuild the index")
	}
	if inst.muted(time.Now().Add(selfWriteQuiet + time.Second)) {
		t.Error("still muted past the quiet window, so no foreign edit would ever be seen again")
	}
}

// And the migration, which is the case that mute was written for: somebody
// upgrading has eighty annotated locations, so eighty writes and eighty echoes
// behind the ONE invalidation that is actually correct.
func TestAdoptLocationsMutesTheChangeFeedForItsOwnEchoes(t *testing.T) {
	f := &fakeHomebox{
		children: map[string][]map[string]any{},
		details: map[string][]any{
			// Annotated by an older version with no decision recorded, which
			// is what adopt writes to.
			"fresno": {map[string]any{"name": "capacityUnits", "type": "number", "numberValue": 8}},
		},
	}
	s := newTestServer(t, f)
	inst := s.defaultInstance()

	if inst.muted(time.Now()) {
		t.Fatal("muted before anything was written")
	}
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/locations/adopt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var out struct {
		Marked int `json:"marked"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Marked != 1 {
		t.Fatalf("marked %d locations, want 1: this migration wrote nothing to echo", out.Marked)
	}

	if !inst.muted(time.Now()) {
		t.Error("not muted once the migration returned; its own echoes would rebuild the index, " +
			"and each rebuild would kill the one the invalidation at the end of it started")
	}
	if inst.muted(time.Now().Add(selfWriteQuiet + time.Second)) {
		t.Error("still muted past the quiet window, so no foreign edit would ever be seen again")
	}
}
