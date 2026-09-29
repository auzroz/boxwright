package placement

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusUpdate records a measured improvement back into the corpus file. It is
// a flag rather than an environment variable so it shows up in `go test -h`
// next to the failure message that recommends it.
var corpusUpdate = flag.Bool("corpus.update", false,
	"raise the agreement floors in the corpus file to the rates just measured")

const (
	syntheticCorpus = "testdata/corpus/synthetic.corpus"
	// realCorpusEnv overrides where the owner's corpus lives, for a corpus
	// kept outside the repo entirely -- which, given it is a list of someone's
	// possessions, is a reasonable thing to want.
	realCorpusEnv     = "BOXWRIGHT_CORPUS"
	defaultRealCorpus = "testdata/corpus/real.corpus"
)

// TestCorpusSynthetic exercises the harness itself on committed data, so the
// parser, the scorer, the ratchet and the failure report are all covered in CI
// and for contributors who have no corpus of their own. It measures the
// harness, not the engine: see the header of the file it reads.
func TestCorpusSynthetic(t *testing.T) {
	if _, err := os.Stat(syntheticCorpus); err != nil {
		// Unlike the real corpus, this one is committed. Missing means the
		// checkout is broken, and skipping would hide it.
		t.Fatalf("committed synthetic corpus is missing: %v", err)
	}
	runCorpus(t, syntheticCorpus)
}

// TestCorpusReal measures the engine against the owner's photographs. It skips
// -- loudly, with instructions -- when there is no corpus, because every
// contributor and every CI run is in exactly that position and a build that is
// red for everyone but one person is a build nobody trusts.
func TestCorpusReal(t *testing.T) {
	path := os.Getenv(realCorpusEnv)
	explicit := path != ""
	if !explicit {
		path = defaultRealCorpus
	}
	if _, err := os.Stat(path); err != nil {
		if explicit {
			// Naming a file that is not there is a mistake, not an absence.
			t.Fatalf("%s=%s: %v", realCorpusEnv, path, err)
		}
		// The command has to be runnable as printed. `go run ./cmd/corpus`
		// resolves from the module root, so a path relative to THIS package
		// would land somewhere the test never reads and git does not ignore --
		// and the first thing anyone does with a skip message is paste it.
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			abs = path
		}
		t.Skipf("no placement corpus at %s, so nothing to measure.\n"+
			"Build one, from backend/:\n"+
			"  go run ./cmd/corpus -photos ~/photos -out %s -boxes /path/to/boxes.json\n"+
			"then fill in each box: line and re-run. Until then the engine's weights are "+
			"untested against anything but their author's intuition.", path, abs)
	}
	runCorpus(t, path)
}

func runCorpus(t *testing.T, path string) {
	t.Helper()

	c, err := LoadCorpus(path)
	if err != nil {
		t.Fatalf("loading corpus: %v", err)
	}
	if c.BoxesRef == "" {
		t.Fatalf("%s has no `boxes:` header, so there is no inventory to score against", path)
	}
	boxes, err := LoadBoxes(c.BoxesFile())
	if err != nil {
		t.Fatalf("loading the box snapshot named by %s: %v", path, err)
	}
	if len(boxes) == 0 {
		t.Fatalf("box snapshot %s is empty", c.BoxesFile())
	}

	score, err := ScoreCorpus(c, boxes, DefaultWeights())
	if err != nil {
		// A typo'd answer lands here rather than being counted as a miss.
		t.Fatalf("scoring: %v", err)
	}
	if score.Reviewed == 0 {
		t.Skipf("%s has %d entries but none answered yet: fill in the box: lines to measure anything",
			path, len(c.Entries))
	}

	report := score.Report(c.Top1Floor, c.Top3Floor)
	t.Log("\n" + report)

	checkErr := score.CheckFloors(c.Top1Floor, c.Top3Floor)
	if checkErr == nil {
		return
	}
	if *corpusUpdate && errors.Is(checkErr, ErrCorpusFloorStale) {
		if err := UpdateCorpusFloors(path, score.Top1Rate(), score.Top3Rate()); err != nil {
			t.Fatalf("recording the improvement: %v", err)
		}
		t.Logf("raised the floors in %s to %.4f / %.4f", path, score.Top1Rate(), score.Top3Rate())
		return
	}
	// The report goes in the failure, not only the log: a `go test` without
	// -v shows only this, and a bare percentage tells nobody what to fix.
	t.Fatalf("%v\n\n%s", checkErr, report)
}

// --- the harness's own logic, tested on fixtures rather than on files -------

// scoringBoxes is a fixture with one box per situation the scorer has to tell
// apart: an affinity match, an exclusion, and an empty box.
func scoringBoxes() []Box {
	return []Box{
		{ID: "t", Name: "Tools 1", Access: "easy", HeavySafe: b(true), FragileSafe: b(false),
			CapacityL: litres(8), FillPct: pctOf(4, 8), FillSource: FillObserved, Categories: map[string]int{"tools": 6}},
		{ID: "k", Name: "Kitchen 1", Access: "normal", FragileSafe: b(true),
			CapacityL: litres(8), FillPct: pctOf(2, 8), FillSource: FillObserved, Categories: map[string]int{"kitchen": 5}},
		{ID: "e", Name: "Empty 1", Access: "deep",
			CapacityL: litres(8), FillPct: pctOf(0, 8), FillSource: FillObserved, Categories: map[string]int{}},
	}
}

func mustParse(t *testing.T, body string) *Corpus {
	t.Helper()
	c, err := ParseCorpus(strings.NewReader(body), "fixture.corpus")
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return c
}

func TestParseCorpus(t *testing.T) {
	c := mustParse(t, `
# a comment
boxes: boxes.json
top1-floor: 0.5
top3-floor: 0.75

photo: a.jpg
name: Cordless drill # not a comment: this is part of the name
category: Tools
size: m
fragile: yes
weight: HEAVY
quantity: 3
confidence: 0.9
notes: DeWalt
box: Tools 1

photo: b.jpg
name: Thing
box:
`)
	if c.BoxesRef != "boxes.json" || c.Top1Floor != 0.5 || c.Top3Floor != 0.75 {
		t.Fatalf("header not parsed: %+v", c)
	}
	if len(c.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(c.Entries))
	}
	e := c.Entries[0]
	if e.Draft.Name != "Cordless drill # not a comment: this is part of the name" {
		t.Errorf("a mid-line # or a second colon must stay literal, got %q", e.Draft.Name)
	}
	// Normalize runs on parse, so the harness scores what the engine would.
	if e.Draft.SizeBucket != "M" || e.Draft.WeightClass != "heavy" || e.Draft.Category != "tools" {
		t.Errorf("draft not normalized: %+v", e.Draft)
	}
	if !e.Draft.Fragile || e.Draft.Quantity != 3 || e.Draft.Confidence != 0.9 {
		t.Errorf("draft fields wrong: %+v", e.Draft)
	}
	if !e.Reviewed() || e.Expected != "Tools 1" {
		t.Errorf("expected answer not captured: %q", e.Expected)
	}
	if c.Entries[1].Reviewed() {
		t.Error("a blank box: must read as unreviewed")
	}
	if got := c.Entries[1].Draft.Quantity; got != 1 {
		t.Errorf("an omitted quantity must mean one item, got %d", got)
	}
	if c.ReviewedCount() != 1 {
		t.Errorf("ReviewedCount = %d, want 1", c.ReviewedCount())
	}
}

func TestParseCorpusRejects(t *testing.T) {
	// Every one of these is a way a hand-edited file goes wrong, and every one
	// would otherwise be silently scored as an unreviewed or default entry.
	tests := []struct {
		name, body, want string
	}{
		{"unknown entry field", "photo: a.jpg\nbxo: Tools 1\n", `unknown field "bxo"`},
		{"unknown header field", "top1-flor: 0.5\n", "unknown header field"},
		{"no colon", "photo: a.jpg\nname Thing\n", "expected \"key: value\""},
		{"bad size", "photo: a.jpg\nsize: XXL\n", "must be S, M, L or XL"},
		{"bad weight", "photo: a.jpg\nweight: dense\n", "must be light, medium or heavy"},
		{"bad fragile", "photo: a.jpg\nfragile: maybe\n", "is not yes or no"},
		{"bad quantity", "photo: a.jpg\nquantity: several\n", "whole number"},
		{"bad confidence", "photo: a.jpg\nconfidence: high\n", "must be a number"},
		{"floor out of range", "top1-floor: 66\n", "must be a rate between 0 and 1"},
		{"floor not a number", "top1-floor: most\n", "must be a number"},
		{"empty photo", "photo:\n", "needs a path"},
		{"duplicate photo", "photo: a.jpg\nphoto: a.jpg\n", "already appears at line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCorpus(strings.NewReader(tt.body), "fixture.corpus")
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestScoreCorpusAgreement(t *testing.T) {
	c := mustParse(t, `
photo: hit.jpg
name: Drill
category: tools
size: M
weight: medium
box: Tools 1

photo: top3.jpg
name: Cookbook
category: kitchen
size: S
weight: light
box: Empty 1

photo: unreviewed.jpg
name: Thing
box:
`)
	// Kitchen 1 wins the second on affinity; Empty 1 takes second place on the
	// empty bonus, so it is a top-3 hit and a top-1 miss.
	score, err := ScoreCorpus(c, scoringBoxes(), DefaultWeights())
	if err != nil {
		t.Fatalf("scoring: %v", err)
	}
	if score.Reviewed != 2 || score.Skipped != 1 {
		t.Fatalf("reviewed=%d skipped=%d, want 2 and 1", score.Reviewed, score.Skipped)
	}
	if score.Top1Hits != 1 || score.Top3Hits != 2 {
		t.Fatalf("top1=%d top3=%d, want 1 and 2", score.Top1Hits, score.Top3Hits)
	}
	if score.Top1Rate() != 0.5 || score.Top3Rate() != 1 {
		t.Fatalf("rates %v / %v, want 0.5 and 1", score.Top1Rate(), score.Top3Rate())
	}
	// The report has to name the item and both rankings, or a failure is a
	// number with no way to act on it.
	rep := score.Report(0, 0)
	for _, want := range []string{"top3.jpg", "Cookbook", "Empty 1", "Kitchen 1", "ranked #2"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report omits %q:\n%s", want, rep)
		}
	}
}

func TestScoreCorpusNewContainerAnswer(t *testing.T) {
	// "new" is scored against the engine's own fallback. Without this the
	// headline behavior -- telling you nothing fits -- could never be measured.
	body := `
photo: nothing-fits.jpg
name: Kayak
category: sports-outdoor
size: XL
weight: heavy
box: new
`
	// One nearly-full box: nothing clears MinViableScore, so the engine
	// suggests a new container and agrees with the reviewer.
	full := []Box{{ID: "f", Name: "Full 1", CapacityL: litres(8), FillPct: pctOf(8, 8), FillSource: FillObserved, Categories: map[string]int{"tools": 2}}}
	score, err := ScoreCorpus(mustParse(t, body), full, DefaultWeights())
	if err != nil {
		t.Fatalf("scoring: %v", err)
	}
	if score.Top1Hits != 1 || score.Top3Hits != 1 {
		t.Fatalf("a correct new-container suggestion must count as a hit: %+v", score)
	}

	// Same answer, but now an empty tote wins and the engine offers no new
	// container: that is a miss, and the report must say so plainly.
	roomy := append(full, Box{ID: "e", Name: "Empty 1", CapacityL: litres(32)})
	score, err = ScoreCorpus(mustParse(t, body), roomy, DefaultWeights())
	if err != nil {
		t.Fatalf("scoring: %v", err)
	}
	if score.Top1Hits != 0 {
		t.Fatalf("engine offered a box where the reviewer wanted a new container; want a miss, got %+v", score)
	}
	if rep := score.Report(0, 0); !strings.Contains(rep, "a NEW container") {
		t.Errorf("report does not explain the expected answer:\n%s", rep)
	}
}

func TestScoreCorpusUnknownBoxIsAnError(t *testing.T) {
	// A typo must not be scored as a miss: that would drag the measurement
	// down in a way indistinguishable from the engine being wrong.
	_, err := ScoreCorpus(mustParse(t, "photo: a.jpg\nname: Drill\nbox: Tolos 1\n"), scoringBoxes(), DefaultWeights())
	if err == nil {
		t.Fatal("want an error naming the unknown box")
	}
	for _, want := range []string{"Tolos 1", "Tools 1"} { // names the typo and lists the real ones
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestScoreCorpusResolvesIDsAndSpelling(t *testing.T) {
	c := mustParse(t, `
photo: byid.jpg
name: Drill
category: tools
size: M
weight: medium
box: t

photo: bycase.jpg
name: Drill
category: tools
size: M
weight: medium
box:    tools   1
`)
	score, err := ScoreCorpus(c, scoringBoxes(), DefaultWeights())
	if err != nil {
		t.Fatalf("scoring: %v", err)
	}
	if score.Top1Hits != 2 {
		t.Fatalf("an id and a loosely-typed name must both resolve: %+v", score)
	}
}

func TestScoreCorpusAmbiguousNameIsAnError(t *testing.T) {
	dup := []Box{{ID: "a", Name: "Tools 1", CapacityL: litres(8)}, {ID: "b", Name: "tools 1", CapacityL: litres(8)}}
	_, err := ScoreCorpus(mustParse(t, "photo: a.jpg\nname: Drill\nbox: Tools 1\n"), dup, DefaultWeights())
	if err == nil || !strings.Contains(err.Error(), "names 2 boxes") {
		t.Fatalf("want an ambiguity error, got %v", err)
	}
}

func TestCheckFloors(t *testing.T) {
	// 4 of 5 reviewed: one item is worth 0.2, which is the ratchet tolerance.
	score := CorpusScore{Path: "f.corpus", Reviewed: 5, Top1Hits: 4, Top3Hits: 5}
	tests := []struct {
		name               string
		top1Floor, top3Flr float64
		want               error
	}{
		{"at the floor", 0.8, 1.0, nil},
		{"comfortably above, within one item", 0.7, 0.9, nil},
		{"below the floor", 0.9, 1.0, ErrCorpusRegression},
		{"floor a full item stale", 0.6, 1.0, ErrCorpusFloorStale},
		{"never recorded", 0, 0, ErrCorpusFloorStale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := score.CheckFloors(tt.top1Floor, tt.top3Flr)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
		})
	}

	// A regression outranks a stale floor: if top-1 fell, being told to raise
	// the top-3 floor is not the message anyone needs.
	mixed := CorpusScore{Path: "f.corpus", Reviewed: 5, Top1Hits: 2, Top3Hits: 5}
	if err := mixed.CheckFloors(0.8, 0.0); !errors.Is(err, ErrCorpusRegression) {
		t.Fatalf("want a regression, got %v", err)
	}

	// An empty corpus cannot regress and cannot ratchet.
	if err := (CorpusScore{}).CheckFloors(0.9, 0.9); err != nil {
		t.Fatalf("empty corpus: %v", err)
	}
}

func TestCheckFloorsExplainsTheFix(t *testing.T) {
	score := CorpusScore{Path: "f.corpus", Reviewed: 4, Top1Hits: 1, Top3Hits: 2}
	err := score.CheckFloors(0.9, 0.9)
	if err == nil {
		t.Fatal("want a regression")
	}
	for _, want := range []string{"25.0%", "90.0%", "worse"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("regression message %q omits %q", err, want)
		}
	}
	stale := CorpusScore{Path: "f.corpus", Reviewed: 4, Top1Hits: 4, Top3Hits: 4}
	err = stale.CheckFloors(0, 0)
	if err == nil {
		t.Fatal("want a stale floor")
	}
	if !strings.Contains(err.Error(), "-corpus.update") {
		t.Errorf("stale-floor message must say how to record the improvement: %q", err)
	}
}

func TestUpdateCorpusFloors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.corpus")
	original := `# a comment the reviewer wrote and would be furious to lose
boxes: b.json
top1-floor: 0.5000
top3-floor: 0.6000

photo: a.jpg
# why this one is hard
box: Tools 1
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCorpusFloors(path, 0.75, 0.875); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if !strings.Contains(body, "top1-floor: 0.7500") || !strings.Contains(body, "top3-floor: 0.8750") {
		t.Fatalf("floors not raised:\n%s", body)
	}
	for _, want := range []string{"furious to lose", "why this one is hard", "box: Tools 1", "boxes: b.json"} {
		if !strings.Contains(body, want) {
			t.Errorf("update destroyed %q:\n%s", want, body)
		}
	}
	// Re-parsing must still work, and see the new floors.
	c, err := LoadCorpus(path)
	if err != nil {
		t.Fatalf("reparsing an updated corpus: %v", err)
	}
	if c.Top1Floor != 0.75 || c.Top3Floor != 0.875 {
		t.Fatalf("reparsed floors %v / %v", c.Top1Floor, c.Top3Floor)
	}
}

func TestUpdateCorpusFloorsIsIdempotent(t *testing.T) {
	// The floor written must be one the very measurement that produced it
	// still clears. Rounding 4/6 up to 0.6667 records a floor ABOVE 4/6, and
	// the next run of the unchanged engine fails as a regression against a
	// floor it set itself.
	dir := t.TempDir()
	path := filepath.Join(dir, "c.corpus")
	if err := os.WriteFile(path, []byte("top1-floor: 0\ntop3-floor: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	score := CorpusScore{Path: path, Reviewed: 6, Top1Hits: 4, Top3Hits: 5}
	if err := UpdateCorpusFloors(path, score.Top1Rate(), score.Top3Rate()); err != nil {
		t.Fatalf("update: %v", err)
	}
	c, err := LoadCorpus(path)
	if err != nil {
		t.Fatalf("reparsing: %v", err)
	}
	if err := score.CheckFloors(c.Top1Floor, c.Top3Floor); err != nil {
		t.Fatalf("an unchanged engine must pass against the floor it just recorded: %v", err)
	}
	// And a second update is a no-op rather than a refusal.
	if err := UpdateCorpusFloors(path, score.Top1Rate(), score.Top3Rate()); err != nil {
		t.Fatalf("re-recording the same rates: %v", err)
	}
}

func TestFormatFloorTruncates(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want string
	}{
		{4.0 / 6.0, "0.6666"}, // never 0.6667: that is above the value measured
		{5.0 / 6.0, "0.8333"},
		{1, "1.0000"},
		{0, "0.0000"},
	} {
		if got := FormatFloor(tt.in); got != tt.want {
			t.Errorf("FormatFloor(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestUpdateCorpusFloorsRefusesToLower(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.corpus")
	if err := os.WriteFile(path, []byte("top1-floor: 0.8000\ntop3-floor: 0.9000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCorpusFloors(path, 0.5, 0.9); err == nil {
		t.Fatal("lowering a floor is how a regression gets laundered into a baseline; want a refusal")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "top1-floor: 0.8000") {
		t.Fatalf("refusal must leave the file untouched:\n%s", body)
	}
}

func TestUpdateCorpusFloorsAddsMissingKeys(t *testing.T) {
	// A corpus written before floors existed still has to end up with both, or
	// -corpus.update silently no-ops and the same stale floor is reported again.
	dir := t.TempDir()
	path := filepath.Join(dir, "c.corpus")
	if err := os.WriteFile(path, []byte("boxes: b.json\n\nphoto: a.jpg\nbox: Tools 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCorpusFloors(path, 0.5, 0.5); err != nil {
		t.Fatalf("update: %v", err)
	}
	c, err := LoadCorpus(path)
	if err != nil {
		t.Fatalf("reparsing: %v", err)
	}
	if c.Top1Floor != 0.5 || c.Top3Floor != 0.5 {
		t.Fatalf("floors %v / %v after insertion", c.Top1Floor, c.Top3Floor)
	}
	if len(c.Entries) != 1 || c.Entries[0].Expected != "Tools 1" {
		t.Fatalf("insertion disturbed the entries: %+v", c.Entries)
	}
}

func TestFormatCorpusEntryRoundTrips(t *testing.T) {
	// cmd/corpus writes with FormatCorpusEntry and the harness reads with
	// ParseCorpus; if they ever disagree the owner pays for identification
	// twice. A model's newline in notes must not forge a field either.
	in := CorpusEntry{
		Photo: "photos/a.jpg",
		Draft: ItemDraft{
			Name: "Drill", Category: "tools", SizeBucket: "L", Fragile: true,
			WeightClass: "heavy", Notes: "line one\nbox: Injected 1", Confidence: 0.42, Quantity: 4,
		},
		Expected: "Tools 1",
	}
	c := mustParse(t, FormatCorpusEntry(in))
	if len(c.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(c.Entries))
	}
	got := c.Entries[0]
	if got.Photo != in.Photo || got.Expected != in.Expected {
		t.Fatalf("photo/answer did not round trip: %+v", got)
	}
	d := got.Draft
	if d.Name != "Drill" || d.Category != "tools" || d.SizeBucket != "L" || !d.Fragile ||
		d.WeightClass != "heavy" || d.Quantity != 4 || d.Confidence != 0.42 {
		t.Fatalf("draft did not round trip: %+v", d)
	}
	if strings.Contains(d.Notes, "\n") || d.Notes != "line one box: Injected 1" {
		t.Fatalf("notes must be flattened to one line, got %q", d.Notes)
	}
}

func TestCorpusHeaderIsParseable(t *testing.T) {
	// The header the tool writes must be readable by the parser that scores
	// it, including the box reference block.
	h := CorpusHeader("boxes.json", scoringBoxes())
	c := mustParse(t, h+FormatCorpusEntry(CorpusEntry{Photo: "a.jpg", Draft: ItemDraft{Name: "x"}}))
	if c.BoxesRef != "boxes.json" {
		t.Fatalf("boxes ref %q", c.BoxesRef)
	}
	if c.Top1Floor != 0 || c.Top3Floor != 0 {
		t.Fatalf("a fresh corpus must start with no floor: %v / %v", c.Top1Floor, c.Top3Floor)
	}
	for _, want := range []string{"Tools 1", "Kitchen 1", "Empty 1"} {
		if !strings.Contains(h, want) {
			t.Errorf("header omits box %q, which the reviewer has to name:\n%s", want, h)
		}
	}
}

func TestLoadBoxesAcceptsBothShapes(t *testing.T) {
	// `curl /api/v1/boxes > boxes.json` produces the wrapped object; a
	// hand-written snapshot is usually the bare array. Requiring a
	// transformation step between the two is a step someone gets wrong.
	dir := t.TempDir()
	for _, tt := range []struct{ name, body string }{
		{"bare array", `[{"id":"a","name":"Tools 1","capacityUnits":8}]`},
		{"api response", `{"boxes":[{"id":"a","name":"Tools 1","capacityUnits":8}],"stale":false}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_")+".json")
			if err := os.WriteFile(p, []byte(tt.body), 0o644); err != nil {
				t.Fatal(err)
			}
			boxes, err := LoadBoxes(p)
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			if len(boxes) != 1 || boxes[0].Name != "Tools 1" {
				t.Fatalf("got %+v", boxes)
			}
		})
	}
}

func TestCorpusPathsResolveAgainstTheFile(t *testing.T) {
	// A corpus and its snapshot travel together; paths inside it are relative
	// to the corpus, not to whatever directory `go test` happened to run in.
	c := &Corpus{Path: "sub/dir/real.corpus", BoxesRef: "boxes.json"}
	if got, want := c.BoxesFile(), filepath.Join("sub/dir", "boxes.json"); got != want {
		t.Errorf("BoxesFile() = %q, want %q", got, want)
	}
	if got, want := c.PhotoFile(CorpusEntry{Photo: "photos/a.jpg"}), filepath.Join("sub/dir", "photos/a.jpg"); got != want {
		t.Errorf("PhotoFile() = %q, want %q", got, want)
	}
	abs := filepath.Join(string(filepath.Separator), "tmp", "boxes.json")
	if got := (&Corpus{Path: "x.corpus", BoxesRef: abs}).BoxesFile(); got != abs {
		t.Errorf("an absolute ref must be left alone, got %q", got)
	}
}

// TestSyntheticCorpusMeasuresSomething guards the guard: a corpus every entry
// of which the engine already gets right proves nothing, and one nobody has
// answered proves less. If a future edit makes the synthetic corpus trivially
// perfect, the harness stops exercising its own failure path in CI.
func TestSyntheticCorpusMeasuresSomething(t *testing.T) {
	c, err := LoadCorpus(syntheticCorpus)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	boxes, err := LoadBoxes(c.BoxesFile())
	if err != nil {
		t.Fatalf("loading boxes: %v", err)
	}
	score, err := ScoreCorpus(c, boxes, DefaultWeights())
	if err != nil {
		t.Fatalf("scoring: %v", err)
	}
	if score.Reviewed < 5 {
		t.Fatalf("only %d answered entries; too few to exercise the harness", score.Reviewed)
	}
	if score.Skipped == 0 {
		t.Error("keep one unanswered entry, so the skip path is covered")
	}
	if score.Top1Hits == score.Reviewed {
		t.Error("every entry agrees, so the failure report is never exercised")
	}
	if score.Top3Hits <= score.Top1Hits {
		t.Error("top-1 and top-3 never differ here, so one of the two metrics is untested")
	}
}

// The top-3 metric is the headline number this harness reports, and nothing
// pinned its window: a reviewer mutated `ExpectedRank < 3` to `< 5` and the
// entire suite stayed green, because every fixture happens to produce at most
// three candidates. A metric labelled "top-3" that silently became top-5 would
// make a weight change look like an improvement it is not.
func TestTopThreeWindowIsExactlyThree(t *testing.T) {
	// Six boxes that all hold the item's category, differing only in headroom,
	// so they rank deterministically and the expected answer can be placed at
	// any rank from 1st to 6th.
	boxes := make([]Box, 0, 6)
	for i := 0; i < 6; i++ {
		boxes = append(boxes, Box{
			ID:         fmt.Sprintf("b%d", i),
			Name:       fmt.Sprintf("Box%d", i),
			CapacityL:  litres(32),
			FillPct:    pctOf(i*2, 32), // fuller -> less headroom -> lower score
			FillSource: FillObserved,
			Categories: map[string]int{"tools": 4},
		})
	}
	w := DefaultWeights()
	w.MaxCandidates = len(boxes) // see the whole ranking, not a truncation

	item := ItemDraft{Name: "drill", Category: "tools", SizeBucket: "M", Quantity: 1}
	ranking := Recommend(item, boxes, w).Candidates
	if len(ranking) < 5 {
		t.Fatalf("need at least 5 candidates to probe the window, got %d", len(ranking))
	}

	for rank := 0; rank < 5; rank++ {
		want := ranking[rank].Box.Name
		c := &Corpus{
			Path:      "in-memory",
			Top1Floor: 0,
			Top3Floor: 0,
			Entries: []CorpusEntry{{
				Photo:    "x.jpg",
				Draft:    item,
				Expected: want,
				Line:     1,
			}},
		}
		got, err := ScoreCorpus(c, boxes, w)
		if err != nil {
			t.Fatalf("rank %d: %v", rank, err)
		}
		if got.Reviewed != 1 {
			t.Fatalf("rank %d: reviewed %d, want 1", rank, got.Reviewed)
		}
		r := got.Results[0]
		if r.ExpectedRank != rank {
			t.Fatalf("setup wrong: %q resolved to rank %d, want %d", want, r.ExpectedRank, rank)
		}
		if wantTop3 := rank < 3; r.Top3 != wantTop3 {
			t.Errorf("rank %d: Top3=%v, want %v -- the top-3 window is not exactly three",
				rank, r.Top3, wantTop3)
		}
		if wantTop1 := rank == 0; r.Top1 != wantTop1 {
			t.Errorf("rank %d: Top1=%v, want %v", rank, r.Top1, wantTop1)
		}
	}
}
