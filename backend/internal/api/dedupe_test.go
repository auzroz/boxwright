package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

// fileCapture posts one capture and returns the status plus the per-entry
// results, which is where everything interesting about a resend shows up.
func fileCapture(h http.Handler, captureID string, entries ...catalogEntry) (int, []catalogResult) {
	b, _ := json.Marshal(catalogRequest{Entries: entries, CaptureID: captureID})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/catalog", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	var out struct {
		Results []catalogResult `json:"results"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	return rr.Code, out.Results
}

func thing(name, category, size string) placement.ItemDraft {
	return placement.ItemDraft{Name: name, Category: category, SizeBucket: size, Quantity: 1}
}

// createsOfType counts what actually reached Homebox.
func (f *cacheHomebox) createdNamed(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.created {
		if c.Name == name {
			n++
		}
	}
	return n
}

// The bug: the offline queue retries on a timeout, and a timeout is exactly
// when the request may have succeeded and only the response was lost.
func TestResendAfterALostResponseFilesNothingTwice(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()

	e := catalogEntry{Item: thing("cordless drill", "tools", "M"), BoxID: "fresno", EntryID: "cap1-0"}

	code, first := fileCapture(h, "cap1", e)
	if code != http.StatusCreated || len(first) != 1 || first[0].Entity == nil {
		t.Fatalf("first send: status %d results %+v", code, first)
	}
	if first[0].Deduped {
		t.Error("the first send reported itself as a duplicate")
	}

	code, second := fileCapture(h, "cap1", e)
	if code != http.StatusCreated || len(second) != 1 {
		t.Fatalf("resend: status %d results %+v", code, second)
	}
	if n := f.createdNamed("cordless drill"); n != 1 {
		t.Fatalf("the drill was created %d times; the resend filed a duplicate", n)
	}
	if !second[0].Deduped {
		t.Error("the resend created nothing but did not say so")
	}
	if second[0].Entity == nil || second[0].Entity.ID != first[0].Entity.ID {
		t.Errorf("resend returned %+v, want the entity the first send created (%s)",
			second[0].Entity, first[0].Entity.ID)
	}
}

// The reason the key is per ENTRY and stored, not the capture id and not a
// position within it.
//
// After a partial failure the app re-sends the SAME capture with a SUBSET of
// its entries, and the entry that failed is no longer where it was. Keyed
// positionally, the resent entry matches the already-filed FIRST one, is
// reported as a duplicate, and is never filed at all -- so the failure mode
// this test detects is a LOST item, not a duplicated one.
func TestSubsetResendIsMatchedByKeyNotPosition(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()

	drill := catalogEntry{Item: thing("cordless drill", "tools", "M"), BoxID: "fresno", EntryID: "cap1-0"}
	sander := catalogEntry{Item: thing("orbital sander", "tools", "M"), BoxID: "fresno", EntryID: "cap1-1"}

	// The first send lands only the drill: the sander's box does not exist, so
	// Homebox refuses it. (Its entry keeps its key regardless.)
	bad := sander
	bad.BoxID = ""
	if code, res := fileCapture(h, "cap1", drill, bad); code != http.StatusCreated || len(res) != 2 {
		t.Fatalf("first send: status %d results %+v", code, res)
	}

	// The queue re-sends what did not land. The sander is now entry ZERO.
	code, res := fileCapture(h, "cap1", sander)
	if code != http.StatusCreated || len(res) != 1 {
		t.Fatalf("resend: status %d results %+v", code, res)
	}
	if res[0].Deduped {
		t.Fatal("the sander was reported as already filed; it was matched against the drill " +
			"by position and has now been lost")
	}
	if n := f.createdNamed("orbital sander"); n != 1 {
		t.Errorf("the sander was created %d times, want exactly 1", n)
	}
	if n := f.createdNamed("cordless drill"); n != 1 {
		t.Errorf("the drill was created %d times; the resend re-filed it", n)
	}
}

// A resent capture must not create a second container with the same name in
// the same place, splitting the items between them.
//
// Nor may either send give it a capacity: a created container's capacity is
// unknown until the user records one, and an unknown capacity never excludes.
func TestResendReusesTheContainerAndNeverSizesIt(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()

	nc := func() *newContainerRequest {
		return &newContainerRequest{Label: "Tools 1", ParentID: "california", SizeBucket: "M", Access: "easy"}
	}
	a := catalogEntry{Item: thing("drill", "tools", "L"), NewContainer: nc(), EntryID: "cap1-0"}
	b := catalogEntry{Item: thing("sander", "tools", "L"), NewContainer: nc(), EntryID: "cap1-1"}

	if code, _ := fileCapture(h, "cap1", a, b); code != http.StatusCreated {
		t.Fatalf("first send: status %d", code)
	}
	if n := f.createdNamed("Tools 1"); n != 1 {
		t.Fatalf("two entries wanting one container created %d of them", n)
	}
	if got := capacityOf(t, f, "e1"); got != 0 {
		t.Fatalf("capacity = %d written on create, want none", got)
	}

	// Only the second entry is re-sent, as a partial failure would.
	if code, res := fileCapture(h, "cap1", b); code != http.StatusCreated || len(res) != 1 {
		t.Fatalf("resend: status %d results %+v", code, res)
	}
	if n := f.createdNamed("Tools 1"); n != 1 {
		t.Errorf("the resend created a second container: %d exist", n)
	}
	if got := capacityOf(t, f, "e1"); got != 0 {
		t.Errorf("capacity = %d written on a resend, want none", got)
	}
}

func capacityOf(t *testing.T, f *cacheHomebox, id string) int {
	t.Helper()
	for _, raw := range f.fieldsOf(id) {
		fld, _ := raw.(map[string]any)
		if fld["name"] != fieldCapacityUnits {
			continue
		}
		switch v := fld["numberValue"].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

// A dedupe hit must not charge the box a second time. The delta was applied
// when the item was really filed; applying it again makes the box look fuller
// than it is and steers the next item away from a container with room.
func TestADedupedEntryDoesNotChargeItsBoxTwice(t *testing.T) {
	f := newCacheHomebox()
	s := cacheTestServer(t, f, time.Hour)
	h := s.Routes()
	getBoxes(h)

	e := catalogEntry{Item: thing("drill", "tools", "L"), BoxID: "fresno", EntryID: "cap1-0"}
	fileCapture(h, "cap1", e)
	_, boxes, _ := getBoxes(h)
	fresno, _ := findBox(boxes, "fresno")
	once := fresno.ItemCount
	if once != 1 {
		t.Fatalf("itemCount after one item = %d, want 1", once)
	}

	fileCapture(h, "cap1", e)
	_, boxes, _ = getBoxes(h)
	fresno, _ = findBox(boxes, "fresno")
	if fresno.ItemCount != once {
		t.Errorf("itemCount = %d after a resend that created nothing, want %d", fresno.ItemCount, once)
	}
	if fresno.Categories["tools"] != 1 {
		t.Errorf("categories = %v, want tools:1 -- a resend counted the item twice", fresno.Categories)
	}
}

// `fields=Name` with no "=" is silently ignored by Homebox and returns the
// ENTIRE inventory. Read as dedupe hits, that means every entry is "already
// filed" and the capture is lost without a word. So an implausibly large
// result set is treated as "the filter did not apply", and we file.
func TestAnUnfilteredLookupIsNeverReadAsAlreadyFiled(t *testing.T) {
	f := newCacheHomebox()
	// Far more rows than keys, none of them carrying the key we ask about.
	f.mu.Lock()
	for i := 0; i < maxDedupeLookups+5; i++ {
		f.items = append(f.items, map[string]any{
			"id": "noise" + string(rune('a'+i%26)) + string(rune('a'+i/26)), "name": "noise",
			"entityType": map[string]any{"id": "item-type", "isLocation": false},
		})
	}
	f.ignoreFieldFilter = true
	f.mu.Unlock()

	h := cacheTestServer(t, f, time.Hour).Routes()
	code, res := fileCapture(h, "cap1",
		catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno", EntryID: "cap1-0"})
	if code != http.StatusCreated || len(res) != 1 {
		t.Fatalf("status %d results %+v", code, res)
	}
	if res[0].Deduped {
		t.Fatal("an unfiltered listing was read as a dedupe hit; the capture was silently dropped")
	}
	if n := f.createdNamed("drill"); n != 1 {
		t.Errorf("created %d times, want 1", n)
	}
}

// An older client, or one that lost its key, must still be able to file. A
// missing key is a reason to accept a possible duplicate, never a 400.
func TestAnEntryWithNoKeyStillFiles(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()

	e := catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno"}
	if code, res := fileCapture(h, "", e); code != http.StatusCreated || len(res) != 1 || res[0].Entity == nil {
		t.Fatalf("status %d results %+v", code, res)
	}
	// And without a key there is nothing to match on, so a resend duplicates.
	// That is the documented trade, stated here so it is not mistaken for a
	// dedupe that failed.
	fileCapture(h, "", e)
	if n := f.createdNamed("drill"); n != 2 {
		t.Errorf("created %d times; without a key both sends should file", n)
	}
}

// The query cannot see a create that is still in flight, so two requests
// carrying the same key can both find nothing and both create.
func TestConcurrentResendsOfOneEntryCreateOneItem(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()

	e := catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno", EntryID: "cap1-0"}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fileCapture(h, "cap1", e)
		}()
	}
	wg.Wait()

	if n := f.createdNamed("drill"); n != 1 {
		t.Errorf("four concurrent sends of one entry created %d items, want 1", n)
	}
}

// The interleaving CI's -race run hit, made deterministic. Request B reads
// Homebox for its keys up front and finds nothing; request A then creates,
// writes the key and releases its claim entirely; B takes the now-free claim
// WITHOUT having waited, so the old code never looked again and filed a second
// copy. Both calls here use a batch whose up-front read is empty -- B's stale
// view -- and run one after the other, which is exactly that ordering.
func TestAResendThatReadBeforeTheCreateDoesNotFileTwice(t *testing.T) {
	stale := func() *catalogBatch {
		return &catalogBatch{
			tags: map[string]homebox.Tag{}, containers: map[string]string{},
			filed: map[string]homebox.Entity{},
		}
	}
	ctx := context.Background()

	t.Run("item", func(t *testing.T) {
		f := newCacheHomebox()
		s := cacheTestServer(t, f, time.Hour)
		inst := s.defaultInstance()
		e := catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno", EntryID: "cap1-0"}

		first, _ := s.fileEntry(ctx, inst, e, stale())
		second, _ := s.fileEntry(ctx, inst, e, stale())

		if first.Error != "" || first.Deduped {
			t.Fatalf("first = %+v", first)
		}
		if !second.Deduped || second.Entity == nil || second.Entity.ID != first.Entity.ID {
			t.Fatalf("second = %+v, want deduped to %s", second, first.Entity.ID)
		}
		if n := f.createdNamed("drill"); n != 1 {
			t.Errorf("created %d, want 1", n)
		}
	})

	t.Run("new container", func(t *testing.T) {
		f := newCacheHomebox()
		s := cacheTestServer(t, f, time.Hour)
		inst := s.defaultInstance()
		nc := &newContainerRequest{Label: "Tools box", ParentID: "garage", SizeBucket: "L", Access: "easy"}

		a, _ := s.fileEntry(ctx, inst, catalogEntry{Item: thing("drill", "tools", "M"), NewContainer: nc, EntryID: "cap1-0"}, stale())
		b, _ := s.fileEntry(ctx, inst, catalogEntry{Item: thing("sander", "tools", "M"), NewContainer: nc, EntryID: "cap1-1"}, stale())

		if a.Error != "" || b.Error != "" {
			t.Fatalf("a = %+v, b = %+v", a, b)
		}
		if n := f.createdNamed("Tools box"); n != 1 {
			t.Errorf("created the container %d times, want 1", n)
		}
	})
}

// The key has to survive on the entity, or the NEXT resend cannot find it.
func TestTheKeyIsStoredOnTheCreatedEntity(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()

	_, res := fileCapture(h, "cap1",
		catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno", EntryID: "cap1-0"})
	if len(res) != 1 || res[0].Entity == nil {
		t.Fatalf("results %+v", res)
	}
	var got string
	for _, raw := range f.fieldsOf(res[0].Entity.ID) {
		fld, _ := raw.(map[string]any)
		if fld["name"] == fieldKey {
			got, _ = fld["textValue"].(string)
		}
	}
	if got != "cap1-0" {
		t.Errorf("%s on the created entity = %q, want %q", fieldKey, got, "cap1-0")
	}
	// It must be TEXT: the fields= query that reads it back matches text only.
	if keyField("x").Type != homebox.FieldTypeText {
		t.Errorf("the key field is %q, which the fields= filter cannot match", keyField("x").Type)
	}
}

// Two entries wanting the same new container derive the same key, and two
// wanting different ones do not.
func TestContainerKeysAreDerivedFromWhereTheContainerGoes(t *testing.T) {
	a := newContainerRequest{Label: "Tools 1", ParentID: "california", SizeBucket: "M", Access: "easy"}
	b := a
	// The size and access an entry happened to suggest are not part of what
	// makes it the same container.
	b.SizeBucket, b.Access = "XL", "deep"
	if containerKeyID(a) != containerKeyID(b) {
		t.Error("the same container under the same parent derived two different keys")
	}
	c := a
	c.ParentID = "nevada"
	if containerKeyID(a) == containerKeyID(c) {
		t.Error("\"Tools 1\" in two different places derived the same key")
	}
	d := a
	d.Label = "Tools 2"
	if containerKeyID(a) == containerKeyID(d) {
		t.Error("two differently named containers derived the same key")
	}
}
