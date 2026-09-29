package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func putContainers(h http.Handler, body string) (int, []containerResult) {
	r := httptest.NewRequest(http.MethodPut, "/api/v1/containers", bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	var out struct {
		Results []containerResult `json:"results"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	return rr.Code, out.Results
}

// "These are 27-gallon totes": one request records the type and size on every
// container named, and the index sees it at once, without a rebuild.
func TestRecordingAContainerTypeOnSeveralContainers(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()
	getBoxes(h)
	reads := f.indexReads.Load()

	code, res := putContainers(h, `{"ids": ["fresno", "modesto"], "set": {
		"containerType": " 27-gallon tote ", "capacityL": 100,
		"interiorCm": {"l": 45, "w": 70, "h": 38}, "access": "easy"}}`)
	if code != http.StatusOK || len(res) != 2 {
		t.Fatalf("status %d, results %+v", code, res)
	}
	for i, id := range []string{"fresno", "modesto"} {
		b := res[i].Box
		if res[i].ID != id || res[i].Error != "" || b == nil {
			t.Fatalf("results[%d] = %+v, want %s written", i, res[i], id)
		}
		if b.ContainerType != "27-gallon tote" || b.CapacityL == nil || *b.CapacityL != 100 ||
			b.InteriorCm == nil || b.InteriorCm.String() != "70x45x38" || b.Access != "easy" {
			t.Errorf("%s = %+v", id, b)
		}
		// Written alongside, never instead of, what was there: still chosen.
		if !f.matchesFields(id, []string{"boxwrightPlacement=true"}) {
			t.Errorf("%s lost its placement flag", id)
		}
	}
	// Two writes read each container back once; nothing else.
	if spent := f.indexReads.Load() - reads; spent != 2 {
		t.Errorf("%d index reads, want 2 (the read-before-write of each container)", spent)
	}
	_, boxes, _ := getBoxes(h)
	if fresno, _ := findBox(boxes, "fresno"); fresno.CapacityL == nil {
		t.Error("the cached index does not show the capacity just recorded")
	}
}

// A bad value is refused before anything is written.
func TestRecordingRefusesNonsenseBeforeWriting(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()
	for _, body := range []string{
		`{"ids": ["fresno"], "set": {"capacityL": 0}}`,
		`{"ids": ["fresno"], "set": {"interiorCm": {"l": 700, "w": 45, "h": 38}}}`,
		`{"ids": ["fresno"], "set": {"access": "upstairs"}}`,
		`{"ids": ["fresno"], "set": {"fill": {"pct": 50, "source": "guessed", "at": "2026-09-28T10:00:00Z"}}}`,
		`{"ids": ["fresno"], "set": {}}`,
		`{"ids": [], "set": {"capacityL": 100}}`,
	} {
		if code, _ := putContainers(h, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, code)
		}
	}
	if len(f.fieldsOf("fresno")) != 1 {
		t.Errorf("fresno was written despite every request being refused: %v", f.fieldsOf("fresno"))
	}
}

// One container that cannot be written does not undo the others.
func TestRecordingIsPerContainer(t *testing.T) {
	f := newCacheHomebox()
	f.failPuts = map[string]bool{"modesto": true}
	h := cacheTestServer(t, f, time.Hour).Routes()
	code, res := putContainers(h, `{"ids": ["fresno", "modesto"], "set": {"capacityL": 60}}`)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if res[0].Error != "" || res[1].Error == "" {
		t.Errorf("results = %+v, want fresno written and modesto's failure reported", res)
	}
}

// A fill recorded from the locations screen follows the same rule as one sent
// with a filing: an older observation never overwrites a newer one.
func TestRecordingAFillRespectsANewerObservation(t *testing.T) {
	f := newCacheHomebox()
	trackedFresno(f, 40, "2026-09-28T12:00:00Z")
	h := cacheTestServer(t, f, time.Hour).Routes()

	putContainers(h, `{"ids": ["fresno"], "set": {"fill": {"pct": 90, "source": "observed", "at": "2026-09-28T11:00:00Z"}}}`)
	if pct, _, _ := fillOf(t, f, "fresno"); pct != 40 {
		t.Errorf("an 11:00 reading overwrote the 12:00 one: %v%%", pct)
	}
	putContainers(h, `{"ids": ["fresno"], "set": {"fill": {"pct": 90, "source": "lidar", "at": "2026-09-28T13:00:00Z"}}}`)
	if pct, source, _ := fillOf(t, f, "fresno"); pct != 90 || source != "lidar" {
		t.Errorf("a 13:00 LiDAR reading = %v%% %q, want 90%% lidar", pct, source)
	}
}
