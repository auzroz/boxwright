package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"boxwright/internal/ai"
	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// categoryHomebox is the live instance in miniature: the user's OWN tags,
// eleven of which fall outside anything we shipped, and two totes whose
// contents carry them.
type categoryHomebox struct {
	tags     []homebox.Tag
	children map[string][]map[string]any
	// eligible is the user's placement selection. Category counts come from
	// the box index, so a location nobody chose contributes nothing.
	eligible map[string]bool

	down     atomic.Bool // the whole instance is unreachable
	tagsDown atomic.Bool // only /v1/tags fails

	entityCalls atomic.Int32 // requests a box-index rebuild makes
	tagCalls    atomic.Int32
}

// taggedItem is an item row as the live server sends it: tags fully expanded,
// no custom fields.
func taggedItem(id string, quantity int, tags ...string) map[string]any {
	rows := []any{}
	for i, name := range tags {
		rows = append(rows, map[string]any{"id": id + "-t" + string(rune('a'+i)), "name": name})
	}
	return map[string]any{
		"id": id, "name": id, "quantity": quantity, "tags": rows,
		"entityType": map[string]any{"id": "item-type", "name": "Item", "isLocation": false},
	}
}

func (f *categoryHomebox) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "connection refused", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/api/v1/tags" {
			f.tagCalls.Add(1)
			if f.tagsDown.Load() {
				http.Error(w, "tags exploded", http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(f.tags)
			return
		}

		f.entityCalls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/api/v1/entities/") {
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/entities/")
			e := map[string]any{"id": id, "name": id,
				"entityType": map[string]any{"id": "loc-type", "isLocation": true}}
			if f.eligible[id] {
				e["fields"] = []any{chosen()}
			}
			json.NewEncoder(w).Encode(e)
			return
		}
		q := r.URL.Query()
		if parents := q["parentIds"]; len(parents) > 0 {
			json.NewEncoder(w).Encode(page(f.children[parents[0]]...))
			return
		}
		if q.Get("isLocation") == "true" {
			rows := []map[string]any{
				loc("garage", "Garage", "", ""),
				loc("california", "California", "garage", "Garage"),
				loc("fresno", "Fresno", "california", "California"),
				loc("modesto", "Modesto", "california", "California"),
			}
			// The index reads only the chosen locations, via fields=.
			if len(q["fields"]) > 0 {
				kept := []map[string]any{}
				for _, l := range rows {
					if id, _ := l["id"].(string); f.eligible[id] {
						kept = append(kept, l)
					}
				}
				rows = kept
			}
			json.NewEncoder(w).Encode(page(rows...))
			return
		}
		json.NewEncoder(w).Encode(page())
	}
}

// liveish returns a fake holding the user's real tag names, with a couple of
// them actually on items.
func liveish() *categoryHomebox {
	return &categoryHomebox{
		tags: []homebox.Tag{
			{ID: "t1", Name: "Micro Masterpieces"},
			{ID: "t2", Name: "Health"},
			{ID: "t3", Name: "Servers"},
			{ID: "t4", Name: "Tools"},
		},
		children: map[string][]map[string]any{
			"fresno":  {taggedItem("i1", 5, "Micro Masterpieces"), taggedItem("i2", 1, "Health")},
			"modesto": {taggedItem("i3", 2, "Health")},
		},
		eligible: map[string]bool{"fresno": true, "modesto": true},
	}
}

func newCategoryServer(t *testing.T, f *categoryHomebox, id ai.Identifier) *Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	if id == nil {
		id = mustProvider(t)
	}
	return New(homebox.New(srv.URL+"/api", "tok"), id, time.Minute, discardLogger())
}

// categoryRow mirrors the shared contract the app decodes.
type categoryRow struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	InUse     bool   `json:"inUse"`
	ItemCount int    `json:"itemCount"`
	Canonical bool   `json:"canonical"`
}

func getCategories(t *testing.T, s *Server) []categoryRow {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/categories", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rr.Code, rr.Body)
	}
	var out struct {
		Categories []categoryRow `json:"categories"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Categories
}

func rowFor(t *testing.T, rows []categoryRow, key string) categoryRow {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no category %q in %v", key, keysOf(rows))
	return categoryRow{}
}

func keysOf(rows []categoryRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Key)
	}
	return out
}

// The vocabulary is the user's, not ours: their tags have to be offered, with
// the name they typed, and marked as not something we shipped.
func TestCategoriesServesTheUsersOwnTags(t *testing.T) {
	s := newCategoryServer(t, liveish(), nil)
	rows := getCategories(t, s)

	mm := rowFor(t, rows, "micro-masterpieces")
	if mm.Label != "Micro Masterpieces" {
		t.Errorf("label = %q, want the tag as the user typed it", mm.Label)
	}
	if mm.Canonical {
		t.Error("micro-masterpieces reported as canonical; we never shipped it")
	}
	if mm.ItemCount != 5 || !mm.InUse {
		t.Errorf("micro-masterpieces count = %d inUse = %v, want 5 and true", mm.ItemCount, mm.InUse)
	}

	// Health is the category their very first item used, and it is nowhere in
	// our seed list.
	health := rowFor(t, rows, "health")
	if health.ItemCount != 3 {
		t.Errorf("health count = %d, want 3 (one item of 1 plus one of 2)", health.ItemCount)
	}

	// A canonical seed the user has never used is still offered, flagged as
	// ours, with no count.
	kitchen := rowFor(t, rows, "kitchen")
	if !kitchen.Canonical || kitchen.InUse || kitchen.ItemCount != 0 {
		t.Errorf("kitchen = %+v, want an unused canonical seed", kitchen)
	}
}

// A tag on nothing is still a deliberate act of vocabulary: offer it.
func TestCategoriesOffersATagThatIsOnNothing(t *testing.T) {
	rows := getCategories(t, newCategoryServer(t, liveish(), nil))

	servers := rowFor(t, rows, "servers")
	if servers.InUse || servers.ItemCount != 0 {
		t.Errorf("servers = %+v, want inUse false with count 0", servers)
	}
	if servers.Canonical {
		t.Error("servers reported as canonical; the user invented it")
	}
}

// Ordering is the contract: in-use first by descending count, everything else
// alphabetically.
func TestCategoriesOrderUsedFirstThenAlphabetical(t *testing.T) {
	rows := getCategories(t, newCategoryServer(t, liveish(), nil))
	if len(rows) < 3 {
		t.Fatalf("only %d categories: %v", len(rows), keysOf(rows))
	}

	if rows[0].Key != "micro-masterpieces" || rows[1].Key != "health" {
		t.Errorf("head of list = %v, want micro-masterpieces (5) then health (3)", keysOf(rows)[:2])
	}

	unusedFrom := 0
	for i, r := range rows {
		if !r.InUse {
			unusedFrom = i
			break
		}
	}
	for _, r := range rows[unusedFrom:] {
		if r.InUse {
			t.Fatalf("an in-use category sorted after an unused one: %v", keysOf(rows))
		}
	}
	tail := keysOf(rows[unusedFrom:])
	if !sort.StringsAreSorted(tail) {
		t.Errorf("unused categories are not alphabetical: %v", tail)
	}
}

// Every canonical seed appears exactly once even though several of the user's
// tags alias onto one ("Tools" folds to the seed "tools").
func TestCategoriesHaveNoDuplicates(t *testing.T) {
	rows := getCategories(t, newCategoryServer(t, liveish(), nil))

	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.Key] {
			t.Errorf("duplicate category key %q in %v", r.Key, keysOf(rows))
		}
		seen[r.Key] = true
	}
	// The user's "Tools" and our "tools" seed are one entry, and it counts as
	// canonical because we ship a label for it.
	tools := rowFor(t, rows, "tools")
	if !tools.Canonical {
		t.Error("tools should be flagged canonical")
	}
}

// A storage unit has no signal. The app still needs something to show, so
// this degrades to the seeds rather than 502-ing like a proxy.
func TestCategoriesSurviveHomeboxBeingUnreachable(t *testing.T) {
	f := liveish()
	f.down.Store(true)
	rows := getCategories(t, newCategoryServer(t, f, nil))

	if len(rows) == 0 {
		t.Fatal("no categories served with Homebox down; the app has nothing to render")
	}
	for _, r := range rows {
		if !r.Canonical {
			t.Errorf("served non-canonical %q with no way to have learned it", r.Key)
		}
		if r.InUse || r.ItemCount != 0 {
			t.Errorf("%q claims usage we could not have counted: %+v", r.Key, r)
		}
	}
	if !sort.StringsAreSorted(keysOf(rows)) {
		t.Errorf("seed fallback is not alphabetical: %v", keysOf(rows))
	}
}

// Only the tag listing failing must not cost the counts we already have.
func TestCategoriesKeepCountsWhenOnlyTheTagListingFails(t *testing.T) {
	f := liveish()
	f.tagsDown.Store(true)
	rows := getCategories(t, newCategoryServer(t, f, nil))

	// micro-masterpieces is unknown to the seed list, but the box index
	// counted it, so it is still real vocabulary and must be offered.
	mm := rowFor(t, rows, "micro-masterpieces")
	if mm.ItemCount != 5 || !mm.InUse {
		t.Errorf("micro-masterpieces = %+v, want the counted 5", mm)
	}
	rowFor(t, rows, "tools") // seeds still there
}

// Counts come from the box index that /recommend already maintains. A second
// crawl would double the cost of every category listing.
func TestCategoriesReuseTheBoxIndexInsteadOfCrawlingAgain(t *testing.T) {
	f := liveish()
	s := newCategoryServer(t, f, nil)

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/boxes", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("warming the index: %d %s", rr.Code, rr.Body)
	}
	afterWarm := f.entityCalls.Load()

	getCategories(t, s)
	if got := f.entityCalls.Load(); got != afterWarm {
		t.Errorf("categories made %d extra entity requests; it must read the cached index",
			got-afterWarm)
	}
}

// recordingIdentifier captures the vocabulary handleIdentify supplies.
type recordingIdentifier struct {
	mu   sync.Mutex
	got  []placement.Category
	seen bool
}

func (r *recordingIdentifier) Identify(_ context.Context, _ []byte, _ string, cats []placement.Category) ([]placement.ItemDraft, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = cats
	r.seen = true
	return []placement.ItemDraft{
		{Name: "resin diorama", Category: "micro-masterpieces", Confidence: 0.9},
	}, nil
}

func (r *recordingIdentifier) categories() []placement.Category {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got
}

func identify(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartImage(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/identify", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	return rr
}

// The model cannot pick from a vocabulary it was never shown.
func TestIdentifyShowsTheModelTheUsersVocabulary(t *testing.T) {
	rec := &recordingIdentifier{}
	s := newCategoryServer(t, liveish(), rec)

	if rr := identify(t, s); rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if !rec.seen {
		t.Fatal("identifier never called")
	}
	got := map[string]bool{}
	for _, c := range rec.categories() {
		got[c.Key] = true
	}
	for _, want := range []string{"micro-masterpieces", "health", "servers"} {
		if !got[want] {
			t.Errorf("vocabulary passed to the model omits the user tag %q", want)
		}
	}
	if !got["kitchen"] {
		t.Error("vocabulary omits the canonical seeds")
	}
}

// A capture is the one thing we cannot ask the user to do twice. A tag listing
// that will not load must degrade to the seeds, never fail the request.
func TestIdentifyFallsBackToSeedsWhenTheVocabularyCannotBeFetched(t *testing.T) {
	f := liveish()
	f.down.Store(true)
	rec := &recordingIdentifier{}
	s := newCategoryServer(t, f, rec)

	rr := identify(t, s)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 with seeds: %s", rr.Code, rr.Body)
	}
	if len(rec.categories()) == 0 {
		t.Fatal("model given an empty vocabulary; the seeds always work")
	}
	for _, c := range rec.categories() {
		if !placement.IsCanonicalCategory(c.Key) {
			t.Errorf("supplied %q, which we could not have learned with Homebox down", c.Key)
		}
	}
}
