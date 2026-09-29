package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"boxwright/internal/homebox"
	"boxwright/internal/placement"
)

func fieldSet(fields ...homebox.CustomField) homebox.FieldSet {
	return homebox.Entity{Fields: fields}.FieldSet()
}

func num(name string, v float64) homebox.CustomField {
	return homebox.CustomField{Name: name, Type: homebox.FieldTypeNumber, NumberValue: v}
}

func text(name, v string) homebox.CustomField {
	return homebox.CustomField{Name: name, Type: homebox.FieldTypeText, TextValue: v}
}

func written(fields []homebox.CustomField) map[string]homebox.CustomField {
	m := map[string]homebox.CustomField{}
	for _, f := range fields {
		m[f.Name] = f
	}
	return m
}

// Every rule for what a container's fill becomes, without Homebox in the way.
func TestFillUpdate(t *testing.T) {
	t9 := "2026-09-28T09:00:00Z"
	tracked := fieldSet(num("capacityL", 100), num("fillPct", 40), text("fillSource", "observed"), text("fillCheckedAt", t9))
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	obs := func(pct float64, when string) boxFill {
		return boxFill{observed: &fillObservation{Pct: pct, Source: "observed", At: when}, observedAt: at(when), litres: 16}
	}

	cases := []struct {
		name       string
		cur        homebox.FieldSet
		fill       boxFill
		captured   string
		wantPct    float64 // -1: nothing written
		wantSource string
		wantAt     string
	}{
		{"estimate added to an observed fill", tracked, boxFill{litres: 16}, "2026-09-28T10:00:00Z", 56, "estimated", t9},
		{"estimate added to an estimate", fieldSet(num("capacityL", 50), num("fillPct", 20), text("fillSource", "estimated")),
			boxFill{litres: 5}, "2026-09-28T10:00:00Z", 30, "estimated", ""},
		{"a capture before the observation is already in it", tracked, boxFill{litres: 16}, "2026-09-28T08:00:00Z", -1, "", ""},
		{"unknown fill stays unknown", fieldSet(num("capacityL", 100)), boxFill{litres: 16}, "", -1, "", ""},
		{"a fill with no source is unknown", fieldSet(num("capacityL", 100), num("fillPct", 40), text("fillSource", "")),
			boxFill{litres: 16}, "", -1, "", ""},
		{"no capacity, nothing to add to", fieldSet(num("fillPct", 40), text("fillSource", "observed")), boxFill{litres: 16}, "", -1, "", ""},
		{"an observation replaces the estimate", tracked, obs(70, "2026-09-28T11:00:00Z"), "", 70, "observed", "2026-09-28T11:00:00Z"},
		{"an observation is written where nothing was known", fieldSet(), obs(30, "2026-09-28T11:00:00Z"), "", 30, "observed", "2026-09-28T11:00:00Z"},
		{"an older observation never overwrites a newer one", tracked, obs(10, "2026-09-28T08:00:00Z"), "", -1, "", ""},
		{"the same observation twice is a no-op (a resend)", tracked, obs(40, t9), "", -1, "", ""},
		{"whole percentages", fieldSet(num("capacityL", 30), num("fillPct", 0), text("fillSource", "estimated")),
			boxFill{litres: 8}, "", 27, "estimated", ""},
	}
	for _, c := range cases {
		got := fillUpdate(c.cur, c.fill, at(c.captured))
		if c.wantPct < 0 {
			if len(got) != 0 {
				t.Errorf("%s: wrote %+v, want nothing", c.name, got)
			}
			continue
		}
		w := written(got)
		if w["fillPct"].NumberValue != c.wantPct || w["fillSource"].TextValue != c.wantSource || w["fillCheckedAt"].TextValue != c.wantAt {
			t.Errorf("%s: wrote %v%% %q at %q, want %v%% %q at %q", c.name,
				w["fillPct"].NumberValue, w["fillSource"].TextValue, w["fillCheckedAt"].TextValue,
				c.wantPct, c.wantSource, c.wantAt)
		}
	}
}

// fillOf reads a container's fill as Homebox holds it.
func fillOf(t *testing.T, f *cacheHomebox, id string) (pct float64, source, checked string) {
	t.Helper()
	for _, raw := range f.fieldsOf(id) {
		fld, _ := raw.(map[string]any)
		switch fld["name"] {
		case "fillPct":
			pct, _ = fld["numberValue"].(float64)
		case "fillSource":
			source, _ = fld["textValue"].(string)
		case "fillCheckedAt":
			checked, _ = fld["textValue"].(string)
		}
	}
	return
}

func trackedFresno(f *cacheHomebox, fill float64, checked string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fields["fresno"] = []any{
		chosen(),
		map[string]any{"name": "capacityL", "type": "number", "numberValue": 100},
		map[string]any{"name": "fillPct", "type": "number", "numberValue": fill},
		map[string]any{"name": "fillSource", "type": "text", "textValue": "observed"},
		map[string]any{"name": "fillCheckedAt", "type": "text", "textValue": checked},
	}
}

// Two M items (8 L each) into a 100 L container seen 40% full: 56%, as an
// estimate, with the observation time left alone -- in Homebox, and in the
// cache without a rebuild. A resend of the same entries adds nothing.
func TestFilingAddsToAKnownFillOnceAndAResendAddsNothing(t *testing.T) {
	f := newCacheHomebox()
	trackedFresno(f, 40, "2026-09-28T09:00:00Z")
	h := cacheTestServer(t, f, time.Hour).Routes()
	getBoxes(h)

	a := catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno", EntryID: "cap1-0"}
	b := catalogEntry{Item: thing("sander", "tools", "M"), BoxID: "fresno", EntryID: "cap1-1"}
	if code, res := fileCapture(h, "cap1", a, b); code != http.StatusCreated || res[0].FillError != "" {
		t.Fatalf("status %d, results %+v", code, res)
	}
	pct, source, checked := fillOf(t, f, "fresno")
	if pct != 56 || source != "estimated" || checked != "2026-09-28T09:00:00Z" {
		t.Errorf("Homebox holds %v%% %q checked %q, want 56%% estimated, still checked at 09:00", pct, source, checked)
	}
	reads := f.indexReads.Load()
	_, boxes, _ := getBoxes(h)
	fresno, _ := findBox(boxes, "fresno")
	if fresno.FillPct == nil || *fresno.FillPct != 56 || fresno.FillSource != "estimated" {
		t.Errorf("cached fill = %v %q, want 56 estimated", fresno.FillPct, fresno.FillSource)
	}
	if f.indexReads.Load() != reads {
		t.Error("the cache needed a rebuild to see the fill it had just written")
	}

	if code, res := fileCapture(h, "cap1", a, b); code != http.StatusCreated || !res[0].Deduped {
		t.Fatalf("resend: status %d, results %+v", code, res)
	}
	if pct, _, _ := fillOf(t, f, "fresno"); pct != 56 {
		t.Errorf("a resend that created nothing moved the fill to %v%%", pct)
	}
}

// "How full is it now?" answered at the container replaces the estimate; an
// answer older than the one Homebox holds -- a capture that sat in the offline
// queue -- does not overwrite it.
func TestAnObservationIsWrittenAndAnOlderOneIsNot(t *testing.T) {
	f := newCacheHomebox()
	trackedFresno(f, 40, "2026-09-28T09:00:00Z")
	h := cacheTestServer(t, f, time.Hour).Routes()
	getBoxes(h)

	e := catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno", EntryID: "cap2-0",
		FillAfter: &fillObservation{Pct: 75, Source: placement.FillObserved, At: "2026-09-28T12:00:00Z"}}
	fileCapture(h, "cap2", e)
	if pct, source, checked := fillOf(t, f, "fresno"); pct != 75 || source != "observed" || checked != "2026-09-28T12:00:00Z" {
		t.Errorf("after an observation: %v%% %q at %q, want 75%% observed at 12:00", pct, source, checked)
	}

	late := catalogEntry{Item: thing("level", "tools", "M"), BoxID: "fresno", EntryID: "cap3-0",
		FillAfter: &fillObservation{Pct: 20, Source: placement.FillObserved, At: "2026-09-28T11:00:00Z"}}
	fileCapture(h, "cap3", late)
	if pct, _, _ := fillOf(t, f, "fresno"); pct != 75 {
		t.Errorf("an 11:00 observation overwrote the 12:00 one: %v%%", pct)
	}
}

// A fill that cannot be saved is reported on each entry that went into the
// container, and the items are filed regardless.
func TestAFillThatCannotBeSavedIsReportedNotFatal(t *testing.T) {
	f := newCacheHomebox()
	trackedFresno(f, 40, "2026-09-28T09:00:00Z")
	f.failPuts = map[string]bool{"fresno": true}
	h := cacheTestServer(t, f, time.Hour).Routes()
	getBoxes(h)

	code, res := fileCapture(h, "cap4",
		catalogEntry{Item: thing("drill", "tools", "M"), BoxID: "fresno", EntryID: "cap4-0"},
		catalogEntry{Item: thing("pan", "kitchen", "M"), BoxID: "modesto", EntryID: "cap4-1"})
	if code != http.StatusCreated {
		t.Fatalf("status %d: a fill failure must not fail the filing", code)
	}
	if res[0].Entity == nil || res[0].FillError == "" {
		t.Errorf("fresno entry = %+v, want filed with a fillError", res[0])
	}
	if res[1].FillError != "" {
		t.Errorf("modesto entry has fillError %q; its container has no fill to update", res[1].FillError)
	}
}

// A new container of one of the user's own types is created knowing its size,
// and starts with what was filed into it.
func TestANewContainerOfAKnownTypeStartsWithWhatWasFiled(t *testing.T) {
	f := newCacheHomebox()
	h := cacheTestServer(t, f, time.Hour).Routes()
	nc := func() *newContainerRequest {
		return &newContainerRequest{Label: "Tools 1", ParentID: "california", SizeBucket: "L", Access: "easy",
			ContainerType: "27-gallon tote", CapacityL: 100, InteriorCm: &placement.Dims{L: 45, W: 70, H: 38}}
	}
	body, _ := json.Marshal(catalogRequest{Entries: []catalogEntry{
		{Item: thing("drill", "tools", "M"), NewContainer: nc(), EntryID: "cap5-0"},
		{Item: thing("sander", "tools", "L"), NewContainer: nc(), EntryID: "cap5-1"},
	}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/catalog", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}

	got := map[string]map[string]any{}
	for _, raw := range f.fieldsOf("e1") {
		fld, _ := raw.(map[string]any)
		got[fld["name"].(string)] = fld
	}
	if got["containerType"]["textValue"] != "27-gallon tote" || got["capacityL"]["numberValue"] != 100.0 ||
		got["interiorCm"]["textValue"] != "70x45x38" {
		t.Errorf("new container fields = %v", got)
	}
	// 8 L + 25 L into 100 L.
	if got["fillPct"]["numberValue"] != 33.0 || got["fillSource"]["textValue"] != "estimated" {
		t.Errorf("new container fill = %v %v, want 33 estimated", got["fillPct"]["numberValue"], got["fillSource"]["textValue"])
	}
}
