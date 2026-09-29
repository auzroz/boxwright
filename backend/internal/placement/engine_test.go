package placement

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func b(v bool) *bool { return &v }

// litres converts the abstract units these tests were first written in (8 was
// "a 27-gallon container") to the litres the engine now works in.
func litres(units int) *int { l := int(float64(units) * 12.5); return &l }

// pctOf is the fill a box of capUnits holding used units is observed at.
func pctOf(used, capUnits int) *float64 { p := float64(used) / float64(capUnits) * 100; return &p }

// The fixture models a fully-annotated inventory: every safety flag is
// explicitly recorded either way. Tests that care about UNRECORDED metadata
// build their own boxes, because nil is a third state, not a default.
func boxes() []Box {
	return []Box{
		{
			ID: "b1", Name: "Tools 1", Area: "California", Access: "easy",
			HeavySafe: b(true), FragileSafe: b(false),
			CapacityL: litres(8), FillPct: pctOf(4, 8), FillSource: FillObserved,
			Categories: map[string]int{"tools": 6},
		},
		{
			ID: "b2", Name: "Kitchen 1", Area: "California", Access: "normal",
			HeavySafe: b(false), FragileSafe: b(true),
			CapacityL: litres(8), FillPct: pctOf(2, 8), FillSource: FillObserved,
			Categories: map[string]int{"kitchen": 5},
		},
		{
			ID: "b3", Name: "Misc 1", Area: "Nevada", Access: "deep",
			HeavySafe: b(false), FragileSafe: b(false),
			CapacityL: litres(8), FillPct: pctOf(7, 8), FillSource: FillObserved,
			Categories: map[string]int{"other": 3, "decor": 2},
		},
	}
}

func TestRecommendPrefersCategoryAffinity(t *testing.T) {
	item := ItemDraft{Name: "cordless drill", Category: "tools", SizeBucket: "M", WeightClass: "medium"}
	rec := Recommend(item, boxes(), DefaultWeights())

	if len(rec.Candidates) == 0 {
		t.Fatal("expected candidates, got none")
	}
	if rec.Candidates[0].Box.ID != "b1" {
		t.Fatalf("expected Tools 1 first, got %s", rec.Candidates[0].Box.Name)
	}
	if rec.NewContainer != nil {
		t.Fatal("did not expect a new-container suggestion for a clean fit")
	}
}

func TestFragileNeverIntoCrushRiskBox(t *testing.T) {
	item := ItemDraft{Name: "wine glasses", Category: "kitchen", SizeBucket: "S", Fragile: true, WeightClass: "light"}
	rec := Recommend(item, boxes(), DefaultWeights())

	for _, c := range rec.Candidates {
		if isFalse(c.Box.FragileSafe) {
			t.Fatalf("fragile item recommended into a box recorded as crush-risk: %s", c.Box.Name)
		}
	}
	if len(rec.Candidates) == 0 || rec.Candidates[0].Box.ID != "b2" {
		t.Fatal("expected the fragile-safe Kitchen 1 box to win")
	}
}

func TestHeavyExcludedFromBoxesRecordedNotHeavySafe(t *testing.T) {
	item := ItemDraft{Name: "cast iron press", Category: "kitchen", SizeBucket: "M", WeightClass: "heavy"}
	rec := Recommend(item, boxes(), DefaultWeights())

	for _, c := range rec.Candidates {
		if isFalse(c.Box.HeavySafe) {
			t.Fatalf("heavy item recommended into a box recorded as not heavy-safe: %s", c.Box.Name)
		}
	}
}

// The cold-start property, and the reason these flags are tri-state. A fresh
// Homebox has no metadata on anything. If an absent flag read as false, every
// fragile item would be excluded from every box, which forces a new-container
// suggestion, which creates a box that also has no metadata -- so the next
// fragile item is excluded from that one too, forever.
func TestUnrecordedSafetyIsNotAnExclusion(t *testing.T) {
	bare := []Box{{ID: "n1", Name: "Fresno", CapacityL: litres(8), Categories: map[string]int{}}}

	for _, item := range []ItemDraft{
		{Name: "wine glasses", Category: "kitchen", SizeBucket: "S", Fragile: true, WeightClass: "light"},
		{Name: "cast iron press", Category: "kitchen", SizeBucket: "M", WeightClass: "heavy"},
	} {
		rec := Recommend(item, bare, DefaultWeights())
		if len(rec.Candidates) != 1 {
			t.Fatalf("%s: unrecorded metadata excluded the box; want 1 candidate, got %d",
				item.Name, len(rec.Candidates))
		}
		joined := strings.Join(rec.Candidates[0].Reasons, " ")
		if !strings.Contains(joined, "not recorded") {
			t.Errorf("%s: expected a caveat reason about unrecorded safety, got %q", item.Name, joined)
		}
	}
}

// The converse: an explicit false is still a hard exclusion.
func TestExplicitFalseStillExcludes(t *testing.T) {
	no := []Box{{ID: "n1", Name: "Fresno", CapacityL: litres(8),
		FragileSafe: b(false), HeavySafe: b(false), Categories: map[string]int{}}}

	fragile := Recommend(ItemDraft{Name: "vase", Category: "decor", SizeBucket: "S", Fragile: true}, no, DefaultWeights())
	if len(fragile.Candidates) != 0 {
		t.Errorf("fragileSafe=false must exclude, got %d candidates", len(fragile.Candidates))
	}
	heavy := Recommend(ItemDraft{Name: "anvil", Category: "tools", SizeBucket: "S", WeightClass: "heavy"}, no, DefaultWeights())
	if len(heavy.Candidates) != 0 {
		t.Errorf("heavySafe=false must exclude, got %d candidates", len(heavy.Candidates))
	}
}

// Two XL duvets are 120 L by their bucket, more than any 100 L container holds
// even empty: a recorded capacity is a fact, and a fact may exclude.
func TestCapacityExclusion(t *testing.T) {
	item := ItemDraft{Name: "duvet", Category: "other", SizeBucket: "XL", WeightClass: "light", Quantity: 2}
	rec := Recommend(item, boxes(), DefaultWeights())

	if len(rec.Candidates) != 0 {
		t.Fatalf("120 L of duvets should fit in no 100 L container, got %d candidates", len(rec.Candidates))
	}
	if rec.NewContainer == nil {
		t.Fatal("expected a new-container suggestion")
	}
	if rec.NewContainer.SizeBucket != "XL" {
		t.Fatalf("expected XL container suggestion, got %s", rec.NewContainer.SizeBucket)
	}
}

func TestNewContainerFallbackOnWeakScores(t *testing.T) {
	// No box holds this category; scores stay under MinViableScore unless
	// capacity headroom alone carries them, which it should not.
	item := ItemDraft{Name: "photo albums", Category: "books-media", SizeBucket: "M", WeightClass: "medium"}
	rec := Recommend(item, boxes(), DefaultWeights())

	if rec.NewContainer == nil {
		t.Fatal("expected a new-container suggestion when no box has category affinity")
	}
}

func TestNormalizeClampsModelOutput(t *testing.T) {
	d := ItemDraft{SizeBucket: "gigantic", WeightClass: "feather", Category: "", Confidence: 3}
	d.Normalize()
	if d.SizeBucket != "M" || d.WeightClass != "medium" || d.Category != "other" || d.Confidence != 1 {
		t.Fatalf("normalize failed: %+v", d)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	item := ItemDraft{Name: "thing", Category: "other", SizeBucket: "S", WeightClass: "light"}
	a := Recommend(item, boxes(), DefaultWeights())
	b := Recommend(item, boxes(), DefaultWeights())
	if len(a.Candidates) != len(b.Candidates) {
		t.Fatal("nondeterministic candidate count")
	}
	for i := range a.Candidates {
		if a.Candidates[i].Box.ID != b.Candidates[i].Box.ID {
			t.Fatal("nondeterministic ordering")
		}
	}
}

// The app calls .map() on candidates and .join() on each candidate's reasons
// without a nil guard, and types.ts declares both non-optional, so a nil slice
// here is a client crash that TypeScript actively hides. Assert the marshalled
// bytes, not the Go value: only the JSON shape is what the app sees.
func TestJSONNeverEmitsNullSlices(t *testing.T) {
	tests := []struct {
		name  string
		item  ItemDraft
		boxes []Box
	}{
		{
			name:  "no boxes at all",
			item:  ItemDraft{Name: "hammer", Category: "tools", SizeBucket: "M"},
			boxes: nil,
		},
		{
			name:  "every box excluded by capacity",
			item:  ItemDraft{Name: "canoe", Category: "sports-outdoor", SizeBucket: "XL"},
			boxes: boxes(),
		},
		{
			name:  "fragile item, no fragile-safe box",
			item:  ItemDraft{Name: "vase", Category: "decor", SizeBucket: "S", Fragile: true},
			boxes: []Box{{ID: "b", Name: "Crush Box", CapacityL: litres(8)}},
		},
		{
			name:  "candidate with neither category match nor zone bonus",
			item:  ItemDraft{Name: "widget", Category: "other", SizeBucket: "S"},
			boxes: []Box{{ID: "b", Name: "Empty", Access: "deep", CapacityL: litres(8)}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(Recommend(tc.item, tc.boxes, DefaultWeights()))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(raw)
			if strings.Contains(got, `"candidates":null`) {
				t.Errorf("candidates marshalled as null, want []: %s", got)
			}
			if strings.Contains(got, `"reasons":null`) {
				t.Errorf("reasons marshalled as null, want []: %s", got)
			}
		})
	}
}

// The invariant that makes like-with-like real. It is enforced by the
// THRESHOLD, not by a hard filter, so it is only true as long as the arithmetic
// holds -- and it did not: CapacityHeadway 1.5 + AccessBonus 1.0 = 2.5 once
// cleared a MinViableScore of 2.0. Observed on live data, where an empty tote
// scored 2.125 for a cordless drill and suppressed the new-container
// suggestion despite holding no tools whatsoever.
func TestWeightsInvariantConvenienceCannotSubstituteForAffinity(t *testing.T) {
	w := DefaultWeights()
	// EmptyBonus is deliberately excluded: emptiness IS a real reason, unlike
	// mere roominess. What must not clear the bar is convenience alone.
	if w.CapacityHeadway+w.AccessBonus >= w.MinViableScore {
		t.Fatalf("capacity (%v) + access (%v) = %v must be < MinViableScore (%v), "+
			"or an unrelated roomy box beats a new container",
			w.CapacityHeadway, w.AccessBonus, w.CapacityHeadway+w.AccessBonus, w.MinViableScore)
	}
	// The same holds for a box whose capacity nobody recorded: not knowing
	// how full it is must never be worth more than knowing it has room.
	if w.NeutralHeadroom > w.CapacityHeadway {
		t.Fatalf("NeutralHeadroom (%v) must not exceed CapacityHeadway (%v)", w.NeutralHeadroom, w.CapacityHeadway)
	}
	if w.NeutralHeadroom+w.AccessBonus >= w.MinViableScore {
		t.Fatalf("neutral headroom (%v) + access (%v) must be < MinViableScore (%v)",
			w.NeutralHeadroom, w.AccessBonus, w.MinViableScore)
	}
}

// An unrecorded capacity is unknown, and unknown never excludes -- however much
// is being placed and however much the box already holds. This is the bug the
// first real use found: every container without a capacity was assumed to hold
// 8 units, every item in it charged 2, and so any container holding four
// things vanished from the candidates.
func TestUnknownCapacityNeverExcludes(t *testing.T) {
	boxes := []Box{{ID: "a", Name: "Tools", ItemCount: 40, Categories: map[string]int{"tools": 40}}}
	for _, size := range []string{"S", "M", "L", "XL"} {
		rec := Recommend(ItemDraft{Name: "drill", Category: "tools", SizeBucket: size, Quantity: 16}, boxes, DefaultWeights())
		if len(rec.Candidates) != 1 {
			t.Errorf("%s x16: %d candidates, want the box that holds tools", size, len(rec.Candidates))
			continue
		}
		if rec.NewContainer != nil {
			t.Errorf("%s x16: suggested a new container although a box full of tools is right there", size)
		}
	}
}

// Emptiness is read from what Homebox lists, not from units: a box whose
// capacity is unknown but which holds unrelated things is NOT a fresh start,
// and must lose to a new container exactly as TestConvenienceAloneCannotClearTheBar
// requires. The same box, empty, must win.
func TestUnknownCapacityEmptinessComesFromTheItemCount(t *testing.T) {
	w := DefaultWeights()
	occupied := []Box{{ID: "a", Name: "Kitchen", Access: "easy", ItemCount: 3, Categories: map[string]int{"kitchen": 3}}}
	rec := Recommend(ItemDraft{Name: "drill", Category: "tools"}, occupied, w)
	if rec.NewContainer == nil {
		t.Errorf("an occupied box of unrelated things, capacity unknown, beat a new container: %+v", rec.Candidates)
	}

	empty := []Box{{ID: "b", Name: "Spare", Access: "deep", Categories: map[string]int{}}}
	rec = Recommend(ItemDraft{Name: "drill", Category: "tools"}, empty, w)
	if rec.NewContainer != nil || len(rec.Candidates) != 1 {
		t.Fatalf("an empty box with unknown capacity lost to buying a new one: %+v", rec)
	}
	if got := rec.Candidates[0].Score; got < w.MinViableScore {
		t.Errorf("empty box scored %v, want at least MinViableScore %v", got, w.MinViableScore)
	}
}

// The same property observed through the engine rather than the constants:
// the best possible box that shares nothing with the item must still lose to a
// new-container suggestion.
func TestConvenienceAloneCannotClearTheBar(t *testing.T) {
	// Roomy, easy to reach, and holding things -- just nothing related. An
	// EMPTY box would be a fair answer (see TestEmptyBoxBeatsBuyingANewOne),
	// so the box has to have unrelated contents for this to test what it says.
	best := []Box{{
		ID: "fresno", Name: "Fresno", Area: "California", Access: "easy",
		CapacityL: litres(8), FillPct: pctOf(2, 8), FillSource: FillObserved,
		Categories: map[string]int{"kitchen": 1},
	}}
	item := ItemDraft{Name: "cordless drill", Category: "tools", SizeBucket: "M", WeightClass: "medium"}

	rec := Recommend(item, best, DefaultWeights())
	if rec.NewContainer == nil {
		var score float64
		if len(rec.Candidates) > 0 {
			score = rec.Candidates[0].Score
		}
		t.Fatalf("a box with zero category affinity (score %v) suppressed the "+
			"new-container suggestion", score)
	}
}

// The converse, so the threshold is not simply set unreachably high: a box
// that genuinely holds the category must clear it.
func TestRealAffinityStillClearsTheBar(t *testing.T) {
	rec := Recommend(
		ItemDraft{Name: "cordless drill", Category: "tools", SizeBucket: "M", WeightClass: "medium"},
		boxes(), DefaultWeights())

	if rec.NewContainer != nil {
		t.Errorf("a box already holding tools should not trigger a new-container suggestion")
	}
	if len(rec.Candidates) == 0 || rec.Candidates[0].Box.ID != "b1" {
		t.Fatal("expected the tools box to win")
	}
	if got := rec.Candidates[0].Score; got < DefaultWeights().MinViableScore {
		t.Errorf("winning score %v is below MinViableScore %v", got, DefaultWeights().MinViableScore)
	}
}

// The weakest affinity that should still count: one matching item in a box of
// ten. Pins the bottom of the useful range so a future weight change cannot
// quietly make small-but-real affinity worthless.
func TestWeakButRealAffinityIsEnough(t *testing.T) {
	cats := map[string]int{"tools": 1}
	for i := 0; i < 9; i++ {
		cats["other"]++
	}
	b := []Box{{ID: "x", Name: "Mixed", Access: "easy", CapacityL: litres(16), FillPct: pctOf(0, 16), FillSource: FillObserved, Categories: cats}}
	rec := Recommend(
		ItemDraft{Name: "drill bit", Category: "tools", SizeBucket: "S", WeightClass: "light"},
		b, DefaultWeights())

	if len(rec.Candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(rec.Candidates))
	}
	if rec.NewContainer != nil {
		t.Errorf("10%% affinity plus easy access should clear the bar; score was %v",
			rec.Candidates[0].Score)
	}
}

// The cold-start case, and the one that decides whether the product is usable
// on day one. A fresh set of empty totes must not produce "no existing box is
// a good fit, start a new container" -- that answer is absurd standing in front
// of eighty empty boxes, and it is what the engine said before EmptyBonus.
func TestEmptyBoxBeatsBuyingANewOne(t *testing.T) {
	empties := []Box{
		{ID: "ajo", Name: "Ajo", Area: "Arizona", Access: "deep", CapacityL: litres(8), Categories: map[string]int{}},
		{ID: "waco", Name: "Waco", Area: "Texas", Access: "easy", CapacityL: litres(8), Categories: map[string]int{}},
	}
	rec := Recommend(
		ItemDraft{Name: "cordless drill", Category: "tools", SizeBucket: "M", WeightClass: "heavy", Quantity: 1},
		empties, DefaultWeights())

	if rec.NewContainer != nil {
		t.Errorf("suggested a new container with %d empty boxes available", len(empties))
	}
	if len(rec.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	if rec.Candidates[0].Score < DefaultWeights().MinViableScore {
		t.Errorf("best empty box scored %v, below the bar %v",
			rec.Candidates[0].Score, DefaultWeights().MinViableScore)
	}
	// Even the awkward one is viable; you should not have to buy a tote
	// because the only empty ones are at the back.
	deep := Recommend(
		ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M", Quantity: 1},
		empties[:1], DefaultWeights())
	if deep.NewContainer != nil {
		t.Error("a hard-to-reach empty box still beats buying a new container")
	}
}

// But a box that actually holds related things must still beat an empty one --
// otherwise like-with-like never accumulates and every item starts its own pile.
func TestAffinityBeatsEmptiness(t *testing.T) {
	mix := []Box{
		{ID: "empty", Name: "Ajo", Access: "easy", CapacityL: litres(8), Categories: map[string]int{}},
		{ID: "tools", Name: "Waco", Access: "easy", CapacityL: litres(8), FillPct: pctOf(4, 8), FillSource: FillObserved,
			Categories: map[string]int{"tools": 2}},
	}
	rec := Recommend(
		ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M", Quantity: 1},
		mix, DefaultWeights())

	if rec.Candidates[0].Box.ID != "tools" {
		t.Errorf("winner was %q; a box already holding tools must beat an empty one",
			rec.Candidates[0].Box.Name)
	}
}

// A region the model got wrong is worse than none: it crops away the object
// the user is being asked to identify. Anything that does not survive
// clamping is dropped, and the app falls back to the whole photo.
func TestRegionNormalization(t *testing.T) {
	f := func(x, y, w, h float64) *Region { return &Region{X: x, Y: y, W: w, H: h} }
	tests := []struct {
		name string
		in   *Region
		want *Region
	}{
		{"absent stays absent", nil, nil},
		{"a good box is kept", f(0.1, 0.2, 0.3, 0.4), f(0.1, 0.2, 0.3, 0.4)},
		// What gemma3:4b answers for a single object: the trivial box. It is
		// not a crop of anything, so it is dropped HERE rather than travelling
		// to the app for the cropper to decline.
		{"the whole photo is not a box worth having", f(0, 0, 1, 1), nil},
		{"zero width is dropped", f(0.1, 0.1, 0, 0.5), nil},
		{"negative width is dropped", f(0.1, 0.1, -0.5, 0.5), nil},
		{"a sliver is dropped: less identifiable than the photo it replaces",
			f(0.5, 0.5, 0.005, 0.4), nil},
		{"a box running off the right edge is clamped, not dropped",
			f(0.8, 0.1, 0.9, 0.2), f(0.8, 0.1, 0.2, 0.2)},
		{"a negative origin is clamped to the corner", f(-0.4, -0.4, 0.5, 0.5), f(0, 0, 0.5, 0.5)},
		{"pixel coordinates leave nothing inside the image and are dropped",
			f(120, 340, 200, 150), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ItemDraft{Name: "x", Region: tc.in}
			d.Normalize()
			switch {
			case tc.want == nil && d.Region != nil:
				t.Fatalf("region = %+v, want it dropped", *d.Region)
			case tc.want != nil && d.Region == nil:
				t.Fatalf("region was dropped, want %+v", *tc.want)
			case tc.want != nil:
				// Compared with a tolerance: clamping is arithmetic on
				// fractions, so 1-0.8 is 0.19999999999999996 and an exact
				// comparison would be testing float64 rather than the rule.
				if !nearRegion(*d.Region, *tc.want) {
					t.Errorf("region = %+v, want %+v", *d.Region, *tc.want)
				}
			}
		})
	}
}

func nearRegion(a, b Region) bool {
	const eps = 1e-9
	return math.Abs(a.X-b.X) < eps && math.Abs(a.Y-b.Y) < eps &&
		math.Abs(a.W-b.W) < eps && math.Abs(a.H-b.H) < eps
}

// Normalize runs on every draft the client sends back too, so a region that
// arrived intact must not be mangled by a second pass.
func TestRegionNormalizationIsIdempotent(t *testing.T) {
	d := ItemDraft{Name: "x", Region: &Region{X: 0.8, Y: 0.1, W: 0.9, H: 0.2}}
	d.Normalize()
	first := *d.Region
	d.Normalize()
	if *d.Region != first {
		t.Errorf("second Normalize changed the region: %+v then %+v", first, *d.Region)
	}
}

// A typed optional field must not make the reply MORE brittle than it was
// before the field existed. An unrecognised key used to be ignored; a badly
// shaped region must be too, or one bad box discards every item in the photo
// and drops the user into manual entry for all of them.
func TestABadlyShapedRegionNeverFailsTheWholeDecode(t *testing.T) {
	shapes := []struct {
		name string
		json string
		want bool // true when a usable region is expected to survive
	}{
		{"the documented object", `{"x":0.1,"y":0.2,"w":0.3,"h":0.4}`, true},
		{"null", `null`, false},
		{"the array form most detection data uses", `[0.1,0.2,0.3,0.4]`, false},
		{"corner array", `[0.1,0.2,0.4,0.6]`, false},
		{"a string", `"0.1,0.2,0.3,0.4"`, false},
		{"the word none", `"none"`, false},
		{"string coordinates", `{"x":"0.1","y":"0.2","w":"0.3","h":"0.4"}`, false},
		{"an empty object", `{}`, false},
		{"a number", `0.5`, false},
		{"nested nonsense", `{"box":{"x":1}}`, false},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			body := `[{"name":"drill","category":"tools","sizeBucket":"M","quantity":1,"region":` + tc.json + `},` +
				`{"name":"tape measure","category":"tools","sizeBucket":"S","quantity":1}]`
			var drafts []ItemDraft
			if err := json.Unmarshal([]byte(body), &drafts); err != nil {
				t.Fatalf("the whole reply failed to decode over one region: %v", err)
			}
			if len(drafts) != 2 {
				t.Fatalf("got %d drafts, want both items", len(drafts))
			}
			for i := range drafts {
				drafts[i].Normalize()
			}
			if got := drafts[0].Region != nil; got != tc.want {
				t.Errorf("region survived = %v, want %v (region was %s)", got, tc.want, tc.json)
			}
			// The item itself always survives, whatever the box said.
			if drafts[0].Name != "drill" || drafts[1].Name != "tape measure" {
				t.Errorf("items were lost: %+v", drafts)
			}
		})
	}
}

// The real failure, from a real photo: a model that cannot locate objects
// answers anyway, and the user gets a confident picture of the wrong thing
// beside the name of a different one.
//
// The numbers below are verbatim from gemma3:4b on a cluttered desk photo, and
// the Claude ones from the same photo, so this test is the measurement.
func TestInventedRegionsAreDiscardedAndMeasuredOnesAreNot(t *testing.T) {
	r := func(x, y, w, h float64) *Region { return &Region{X: x, Y: y, W: w, H: h} }

	invented := []ItemDraft{
		{Name: "laptop", Region: r(0.20, 0.30, 0.60, 0.70)},
		{Name: "fan", Region: r(0.80, 0.60, 0.15, 0.30)},
		{Name: "printer", Region: r(0.40, 0.50, 0.25, 0.30)},
		{Name: "desk", Region: r(0.00, 0.10, 0.80, 0.70)},
		{Name: "chair", Region: r(0.15, 0.35, 0.30, 0.50)},
		{Name: "papers", Region: r(0.00, 0.40, 0.80, 0.20)},
		{Name: "binder", Region: r(0.55, 0.65, 0.20, 0.25)},
	}
	for i := range invented {
		invented[i].Normalize()
	}
	// Normalize alone already takes the two that are not crops at all.
	if invented[0].Region != nil {
		t.Errorf("a box covering 42%% of the photo survived; it shows the shelf, not the laptop")
	}
	if invented[3].Region != nil {
		t.Errorf("a box covering 56%% of the photo survived")
	}
	if n := DropFabricatedRegions(invented); n == 0 {
		t.Fatal("28 coordinates every one a multiple of 0.05 were accepted as measurements")
	}
	for _, d := range invented {
		if d.Region != nil {
			t.Errorf("%s kept an invented region %+v", d.Name, *d.Region)
		}
	}

	measured := []ItemDraft{
		{Name: "laptop computer", Region: r(0.240, 0.500, 0.240, 0.150)},
		{Name: "smartphone", Region: r(0.540, 0.560, 0.060, 0.080)},
		{Name: "desk ring light", Region: r(0.610, 0.500, 0.080, 0.130)},
		{Name: "printer", Region: r(0.720, 0.480, 0.160, 0.130)},
		{Name: "oscillating fan", Region: r(0.830, 0.500, 0.170, 0.300)},
		{Name: "tote bag", Region: r(0.620, 0.720, 0.200, 0.280)},
		{Name: "wall mirror", Region: r(0.080, 0.280, 0.200, 0.360)},
	}
	for i := range measured {
		measured[i].Normalize()
	}
	if n := DropFabricatedRegions(measured); n != 0 {
		t.Fatalf("discarded %d regions that were verified correct against the photo", n)
	}
	for _, d := range measured {
		if d.Region == nil {
			t.Errorf("%s lost a region that crops to the right object", d.Name)
		}
	}
}

// One box carries too few digits to judge, and a single item is the case where
// a crop matters least anyway.
func TestASingleRegionIsNotJudgedAsFabricated(t *testing.T) {
	drafts := []ItemDraft{{Name: "drill", Region: &Region{X: 0.25, Y: 0.5, W: 0.2, H: 0.2}}}
	drafts[0].Normalize()
	if n := DropFabricatedRegions(drafts); n != 0 {
		t.Error("a lone round-numbered box was treated as invented")
	}
	if drafts[0].Region == nil {
		t.Error("the region was dropped")
	}
}

// A box that is most of the photo is not a crop: it shows the user what they
// are already looking at, and cannot tell one card from another.
func TestABoxThatIsNotACropIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h float64
		want bool
	}{
		{name: "a quarter of the frame is a crop", w: 0.5, h: 0.5, want: true},
		{name: "just under a third still is", w: 0.6, h: 0.5, want: true},
		{name: "over a third is not", w: 0.6, h: 0.6, want: false},
		{name: "most of the photo is not", w: 0.8, h: 0.7, want: false},
		{name: "the whole photo is not", w: 1, h: 1, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ItemDraft{Name: "x", Region: &Region{X: 0, Y: 0, W: tc.w, H: tc.h}}
			d.Normalize()
			if got := d.Region != nil; got != tc.want {
				t.Errorf("kept = %v, want %v for a %.0f%% box", got, tc.want, tc.w*tc.h*100)
			}
		})
	}
}

// The reported bug, exactly: a lawn mower photographed and confidently
// recommended into a tote. XL is 8 units and an empty default container is 8,
// so it scored as fitting perfectly.
func TestALawnMowerIsNeverPutInABox(t *testing.T) {
	mower := ItemDraft{Name: "lawn mower", Category: "tools", SizeBucket: "XL", Bulky: true, Quantity: 1}
	garage := Box{ID: "garage", Name: "Garage", IsArea: true}
	tote := Box{ID: "fresno", Name: "Fresno", Area: "California", CapacityL: litres(8)}

	got := Recommend(mower, []Box{tote, garage}, DefaultWeights())

	if len(got.Candidates) == 0 {
		t.Fatalf("no candidates for a mower with a garage available: %+v", got)
	}
	for _, c := range got.Candidates {
		if !c.Box.IsArea {
			t.Errorf("recommended %q, which is a container, for something that does not fit in one",
				c.Box.Name)
		}
	}
	if got.Candidates[0].Box.ID != "garage" {
		t.Errorf("top candidate is %q, want the Garage", got.Candidates[0].Box.Name)
	}
	// And it must clear the bar on its own, or the answer is "nowhere".
	if got.Candidates[0].Score < DefaultWeights().MinViableScore {
		t.Errorf("the garage scored %.2f, below MinViableScore %.2f, so a mower with somewhere "+
			"to go would still be told there is nowhere",
			got.Candidates[0].Score, DefaultWeights().MinViableScore)
	}
	if got.NewContainer != nil {
		t.Errorf("suggested buying a container for a lawn mower: %+v", *got.NewContainer)
	}
}

// And the other direction: areas must never absorb ordinary items, or every
// drill ends up "in the Garage" instead of in a tote.
func TestAnOrdinaryItemIsNeverPutInAnArea(t *testing.T) {
	drill := ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M", Quantity: 1}
	garage := Box{ID: "garage", Name: "Garage", IsArea: true}
	tote := Box{ID: "fresno", Name: "Fresno", CapacityL: litres(8)}

	got := Recommend(drill, []Box{garage, tote}, DefaultWeights())

	for _, c := range got.Candidates {
		if c.Box.IsArea {
			t.Errorf("a drill was offered %q, an area; it belongs in a container", c.Box.Name)
		}
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Box.ID != "fresno" {
		t.Errorf("candidates = %+v, want the tote alone", got.Candidates)
	}
}

// With nowhere to stand it, the answer must not be "buy a box" -- that is the
// same bug wearing a hat.
func TestABulkyItemWithNoAreaIsToldSoRatherThanSoldABox(t *testing.T) {
	mower := ItemDraft{Name: "lawn mower", Category: "tools", SizeBucket: "XL", Bulky: true, Quantity: 1}
	tote := Box{ID: "fresno", Name: "Fresno", CapacityL: litres(8)}

	got := Recommend(mower, []Box{tote}, DefaultWeights())

	if got.NewContainer != nil {
		t.Errorf("offered to create a container for a lawn mower: %+v", *got.NewContainer)
	}
	if got.NoPlace == nil {
		t.Fatal("no answer at all for a bulky item with nowhere to put it")
	}
	if len(got.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none: the only location is a container", got.Candidates)
	}
}

// An area does not fill up the way a tote does. Four bicycles must not
// exhaust a building.
func TestAnAreaDoesNotRunOutOfRoom(t *testing.T) {
	garage := Box{ID: "garage", Name: "Garage", IsArea: true, ItemCount: 500}
	bike := ItemDraft{Name: "bicycle", Category: "sports-outdoor", SizeBucket: "XL", Bulky: true, Quantity: 4}

	got := Recommend(bike, []Box{garage}, DefaultWeights())

	if len(got.Candidates) != 1 {
		t.Fatalf("the garage was excluded on capacity: %+v", got)
	}
}
