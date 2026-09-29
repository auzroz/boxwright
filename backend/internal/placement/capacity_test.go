package placement

import (
	"math"
	"testing"
)

func capL(l int) *int            { return &l }
func fillAt(p float64) *float64  { return &p }
func dims(l, w, h float64) *Dims { return &Dims{L: l, W: w, H: h} }

// A container the user measured: 100 L, 70x45x38 inside.
func tote(id string, fill *float64, source string, cats map[string]int) Box {
	n := 0
	for _, c := range cats {
		n += c
	}
	return Box{
		ID: id, Name: id, ContainerType: "27-gallon tote",
		CapacityL: capL(100), InteriorCm: dims(70, 45, 38),
		FillPct: fill, FillSource: source, ItemCount: n, Categories: cats,
	}
}

func candidateIDs(rec Recommendation) []string {
	ids := make([]string, 0, len(rec.Candidates))
	for _, c := range rec.Candidates {
		ids = append(ids, c.Box.ID)
	}
	return ids
}

func has(rec Recommendation, id string) bool {
	for _, c := range rec.Candidates {
		if c.Box.ID == id {
			return true
		}
	}
	return false
}

// Not knowing how full a box is sits between knowing it is roomy and knowing
// it is nearly full -- never above the first, never below the second.
func TestUnknownFillRanksBetweenRoomyAndNearlyFull(t *testing.T) {
	tools := map[string]int{"tools": 4}
	boxes := []Box{
		tote("roomy", fillAt(10), FillObserved, tools),
		tote("unknown", nil, "", tools),
		tote("tight", fillAt(85), FillObserved, tools),
	}
	w := DefaultWeights()
	rec := Recommend(ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, boxes, w)
	got := candidateIDs(rec)
	want := []string{"roomy", "unknown", "tight"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("ranking = %v, want %v", got, want)
	}
}

// An OBSERVED fill is a fact and may exclude: a box seen full, and a box seen
// too full for this item (past 110% once it is in).
func TestObservedFillExcludes(t *testing.T) {
	tools := map[string]int{"tools": 4}
	boxes := []Box{
		tote("full", fillAt(100), FillObserved, tools),
		tote("almost", fillAt(80), FillLidar, tools),
		tote("fine", fillAt(20), FillObserved, tools),
	}
	// L is 25 L: 80% + 25% of 100 L = 105%, within the 10% slack.
	rec := Recommend(ItemDraft{Name: "saw", Category: "tools", SizeBucket: "L"}, boxes, DefaultWeights())
	if has(rec, "full") {
		t.Error("a box observed full was offered")
	}
	if !has(rec, "almost") || !has(rec, "fine") {
		t.Errorf("candidates = %v, want almost and fine", candidateIDs(rec))
	}
	// Two of them are 50 L: 130%, past the slack.
	rec = Recommend(ItemDraft{Name: "saw", Category: "tools", SizeBucket: "L", Quantity: 2}, boxes, DefaultWeights())
	if has(rec, "almost") {
		t.Error("a box measured 80% full was offered 50 L more")
	}
}

// An ESTIMATED fill -- Boxwright adding up what it filed -- never excludes.
// When it says the item will not fit, the box stays in the list (the user can
// see it and knows better than a sum of guesses) but loses enough score that
// the new-container suggestion appears beside it, even for a perfect match.
func TestEstimatedOverfullIsPenalisedNeverExcluded(t *testing.T) {
	boxes := []Box{tote("guessed", fillAt(95), FillEstimated, map[string]int{"tools": 10})}
	rec := Recommend(ItemDraft{Name: "saw", Category: "tools", SizeBucket: "L"}, boxes, DefaultWeights())
	if !has(rec, "guessed") {
		t.Fatal("an estimate excluded a box; only observations may")
	}
	if rec.NewContainer == nil {
		t.Errorf("a box estimated at 120%% scored %v and hid the new-container suggestion", rec.Candidates[0].Score)
	}
}

// A measured size decides whether an item goes in at all, which capacity
// cannot: a 90 cm tripod does not go in a 70 cm container however empty. A
// model's estimate, from a photo with no scale, never excludes.
func TestMeasuredSizeMustFitTheInterior(t *testing.T) {
	boxes := []Box{tote("empty", fillAt(0), FillObserved, map[string]int{})}
	tripod := ItemDraft{Name: "tripod", Category: "other", SizeBucket: "M", DimensionsCm: dims(90, 12, 12)}

	for _, src := range []string{DimsLidar, DimsManual} {
		tripod.DimensionsSource = src
		if rec := Recommend(tripod, boxes, DefaultWeights()); has(rec, "empty") {
			t.Errorf("%s: a 90 cm item was offered a 70 cm container", src)
		}
	}
	tripod.DimensionsSource = DimsVision
	if rec := Recommend(tripod, boxes, DefaultWeights()); !has(rec, "empty") {
		t.Error("vision: an estimated size excluded a container")
	}
	// Longest side against longest side: a 65x40x10 board lying any way up fits.
	board := ItemDraft{Name: "board", Category: "other", DimensionsCm: dims(10, 65, 40), DimensionsSource: DimsManual}
	if rec := Recommend(board, boxes, DefaultWeights()); !has(rec, "empty") {
		t.Error("a 65x40x10 item did not fit a 70x45x38 interior")
	}
}

// An empty container the item fits in always clears the bar, whatever its
// access and however much of it the item takes. Buying a container while an
// empty one that would do sits right there is the answer that loses trust.
func TestAnEmptyContainerTheItemFitsAlwaysClearsTheBar(t *testing.T) {
	w := DefaultWeights()
	for _, size := range []string{"S", "M", "L", "XL"} {
		for _, box := range []Box{
			{ID: "known", Name: "known", Access: "deep", CapacityL: capL(62), Categories: map[string]int{}},
			{ID: "unknown", Name: "unknown", Access: "deep", Categories: map[string]int{}},
		} {
			rec := Recommend(ItemDraft{Name: "thing", Category: "other", SizeBucket: size}, []Box{box}, w)
			if rec.NewContainer != nil || len(rec.Candidates) != 1 || rec.Candidates[0].Score < w.MinViableScore {
				t.Errorf("%s into an empty %s container: %+v", size, box.ID, rec)
			}
		}
	}
}

// Emptiness must not outrank affinity: a box already holding the same things,
// with room, beats an empty one.
func TestAffinityStillBeatsEmptinessInLitres(t *testing.T) {
	boxes := []Box{
		tote("empty", fillAt(0), FillObserved, map[string]int{}),
		tote("tools", fillAt(50), FillObserved, map[string]int{"tools": 6}),
	}
	rec := Recommend(ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, boxes, DefaultWeights())
	if got := candidateIDs(rec); len(got) == 0 || got[0] != "tools" {
		t.Errorf("ranking = %v, want the half-full box of tools first", got)
	}
}

// A new container is one of the user's own types when one suits: the smallest
// with room for twice the item, since it starts a group. None recorded, and
// the suggestion is the size bucket, as before.
func TestNewContainerUsesTheUsersOwnType(t *testing.T) {
	kitchen := map[string]int{"kitchen": 3}
	small := Box{ID: "s", Name: "s", ContainerType: "shoebox", CapacityL: capL(8), ItemCount: 3, Categories: kitchen}
	big := tote("t", fillAt(90), FillObserved, kitchen)

	rec := Recommend(ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"}, []Box{small, big}, DefaultWeights())
	if rec.NewContainer == nil {
		t.Fatal("no new-container suggestion")
	}
	if rec.NewContainer.ContainerType != "27-gallon tote" || rec.NewContainer.CapacityL != 100 {
		t.Errorf("suggested %q (%d L), want the 27-gallon tote: an 8 L item wants 16 L, which the shoebox is not",
			rec.NewContainer.ContainerType, rec.NewContainer.CapacityL)
	}
	if rec.NewContainer.InteriorCm == nil || rec.NewContainer.InteriorCm.String() != "70x45x38" {
		t.Errorf("interior = %v, want the type's 70x45x38", rec.NewContainer.InteriorCm)
	}

	rec = Recommend(ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M"},
		[]Box{{ID: "k", Name: "k", ItemCount: 3, Categories: kitchen}}, DefaultWeights())
	if rec.NewContainer == nil || rec.NewContainer.ContainerType != "" || rec.NewContainer.SizeBucket != "L" {
		t.Errorf("with no types recorded, want the bucket suggestion: %+v", rec.NewContainer)
	}
}

func TestContainerTypesAreDerivedFromTheContainers(t *testing.T) {
	boxes := []Box{
		{ContainerType: "27-gallon tote", CapacityL: capL(100), InteriorCm: dims(70, 45, 38)},
		{ContainerType: "27-gallon tote", CapacityL: capL(100), InteriorCm: dims(70, 45, 38)},
		{ContainerType: "27-gallon tote", CapacityL: capL(10)}, // one mistyped container
		{ContainerType: "shoebox", CapacityL: capL(8)},
		{ContainerType: "shelf"},                                    // no capacity: not a type yet
		{ContainerType: "garage", CapacityL: capL(9), IsArea: true}, // areas are not containers
	}
	got := ContainerTypes(boxes)
	if len(got) != 2 {
		t.Fatalf("types = %+v, want shoebox and 27-gallon tote", got)
	}
	if got[0].Name != "shoebox" || got[1].Name != "27-gallon tote" {
		t.Errorf("order = %s, %s; want smallest first", got[0].Name, got[1].Name)
	}
	if got[1].CapacityL != 100 || got[1].Count != 3 || got[1].InteriorCm == nil {
		t.Errorf("27-gallon tote = %+v, want 100 L from the majority, count 3, with its interior", got[1])
	}
}

func TestParseDims(t *testing.T) {
	cases := map[string]string{
		"70x45x38":         "70x45x38",
		"45 × 70 × 38":     "70x45x38",
		"70*45*38 cm":      "70x45x38",
		" 70 X 45 X 38.5 ": "70x45x38.5",
		"70x45":            "",
		"70x45xabc":        "",
		"0x45x38":          "",
		"7000x45x38":       "",
		"":                 "",
	}
	for in, want := range cases {
		got := ""
		if d := ParseDims(in); d != nil {
			got = d.String()
		}
		if got != want {
			t.Errorf("ParseDims(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNeedLitres(t *testing.T) {
	cases := []struct {
		name string
		item ItemDraft
		want float64
	}{
		{"bucket", ItemDraft{SizeBucket: "L"}, 25},
		{"bucket times quantity", ItemDraft{SizeBucket: "S", Quantity: 6}, 12},
		{"unknown bucket is M", ItemDraft{SizeBucket: "huge"}, 8},
		{"a size beats the bucket", ItemDraft{SizeBucket: "XL", DimensionsCm: dims(30, 20, 10)}, 6},
	}
	for _, c := range cases {
		if got := c.item.NeedLitres(); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A size nobody vouched for is an estimate, and an unbelievable one is dropped:
// the bucket then stands in, which is never wrong in kind.
func TestNormalizeDimensions(t *testing.T) {
	d := ItemDraft{DimensionsCm: dims(10, 30, 20), DimensionsSource: "made-up"}
	d.Normalize()
	if d.DimensionsCm == nil || d.DimensionsCm.String() != "30x20x10" || d.DimensionsSource != DimsVision {
		t.Errorf("got %v from %q, want 30x20x10 from vision", d.DimensionsCm, d.DimensionsSource)
	}
	d = ItemDraft{DimensionsCm: dims(1200, 30, 20), DimensionsSource: DimsLidar} // millimetres, probably
	d.Normalize()
	if d.DimensionsCm != nil || d.DimensionsSource != "" {
		t.Errorf("a 12 m item survived Normalize: %v %q", d.DimensionsCm, d.DimensionsSource)
	}
}

// Clients that predate litres get units derived from what is known, and 0/0
// -- which they read as room -- for what is not.
func TestSetLegacyUnits(t *testing.T) {
	b := tote("t", fillAt(50), FillObserved, nil)
	b.SetLegacyUnits()
	if b.CapacityUnits != 8 || b.UsedUnits != 4 {
		t.Errorf("100 L at 50%% = %d/%d units, want 8/4", b.UsedUnits, b.CapacityUnits)
	}
	u := Box{CapacityUnits: 8, UsedUnits: 8}
	u.SetLegacyUnits()
	if u.CapacityUnits != 0 || u.UsedUnits != 0 {
		t.Errorf("unknown capacity = %d/%d units, want 0/0", u.UsedUnits, u.CapacityUnits)
	}
}

// Within one photo, the second item sees what the first took -- but only
// where the fill is known; an unknown stays unknown.
func TestAddLitres(t *testing.T) {
	b := tote("t", fillAt(50), FillObserved, nil)
	b.AddLitres(25)
	if *b.FillPct != 75 || b.FillSource != FillObserved {
		t.Errorf("fill = %v %s, want 75 observed", *b.FillPct, b.FillSource)
	}
	u := tote("u", nil, "", nil)
	u.AddLitres(25)
	if u.FillPct != nil {
		t.Errorf("an unknown fill became %v", *u.FillPct)
	}
}
