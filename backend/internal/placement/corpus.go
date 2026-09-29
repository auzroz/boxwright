package placement

// A golden corpus is the only way to tell whether a change to the weights in
// engine.go made recommendations BETTER or merely different. The unit tests
// beside this file pin arithmetic: they catch an accidental change, but they
// cannot answer "is 5.0 the right CategoryMatch?", because they were written
// from the same intuitions the weights were. A corpus answers it with
// evidence: real photos, a real box index, and a human's own answer for where
// each thing actually belongs.
//
// The format is line-oriented and hand-editable on purpose. Its readers are a
// person filling in twenty `box:` fields in a text editor and a test; JSON
// would serve the second at the expense of the first, and the first is the one
// doing the expensive work.
//
//	# comments are whole lines
//	boxes: boxes.json
//	top1-floor: 0.6600
//	top3-floor: 0.8300
//
//	photo: photos/IMG_0001.jpg
//	name: Cordless drill
//	category: tools
//	size: M
//	fragile: no
//	weight: medium
//	quantity: 1
//	confidence: 0.86
//	notes: DeWalt DCD771
//	box: Tools 1
//
// Everything before the first `photo:` is the header; each `photo:` opens a
// new entry. `box:` is the ground truth and the only field a reviewer must
// write. See cmd/corpus for the tool that produces this file.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// NewContainerAnswer is what a reviewer writes in `box:` when the correct
// answer is a container that does not exist yet. The new-container fallback is
// a headline feature of the engine, so the corpus has to be able to say that
// it was right -- otherwise every measurement silently assumes some existing
// box was always the answer, and the one behavior we most want evidence for is
// the one behavior we cannot score.
const NewContainerAnswer = "new"

// newContainerAnswers are the spellings accepted for it. A reviewer typing in
// a text editor should not have to remember which one we chose.
var newContainerAnswers = map[string]bool{
	"new":             true,
	"new-container":   true,
	"new container":   true,
	"new box":         true,
	"none":            true,
	"nothing fits":    true,
	"no existing box": true,
}

// CorpusEntry is one photo, the draft identified from it, and the reviewer's
// answer for where the item actually belongs.
type CorpusEntry struct {
	// Photo is the path as written in the file: relative to the corpus file's
	// own directory, so the pair can be moved or copied to another machine
	// together and still resolve.
	Photo string
	Draft ItemDraft
	// Expected is the reviewer's answer verbatim -- a box name, a box id, or
	// one of newContainerAnswers. Empty means "not reviewed yet", which is the
	// state every entry starts in and is skipped rather than counted as a
	// miss: an unanswered question is not a wrong answer.
	Expected string
	// Line is where the entry starts, so a bad answer can be reported at the
	// place the reviewer has to go and fix it.
	Line int
}

// Reviewed reports whether a human has filled in this entry's box.
func (e CorpusEntry) Reviewed() bool { return strings.TrimSpace(e.Expected) != "" }

// ExpectsNewContainer reports whether the reviewer's answer is "no existing
// box was right".
func (e CorpusEntry) ExpectsNewContainer() bool {
	return newContainerAnswers[strings.ToLower(strings.TrimSpace(e.Expected))]
}

// Corpus is a parsed corpus file.
type Corpus struct {
	// Path is the file it came from, for error messages.
	Path string
	// BoxesRef is the `boxes:` header value: where the box index snapshot
	// these answers were chosen against lives. Answers are only meaningful
	// against the inventory the reviewer was looking at, so the pairing is
	// recorded in the file rather than left to a naming convention.
	BoxesRef string
	// Top1Floor and Top3Floor are the agreement rates below which the harness
	// fails. They live in the corpus rather than in the test because they are
	// a property of this corpus -- a different set of photos has a different
	// achievable rate, and a floor in the test would have to be the minimum
	// over all of them, which is a floor that catches nothing.
	Top1Floor, Top3Floor float64
	Entries              []CorpusEntry
}

// BoxesFile resolves BoxesRef against the corpus file's directory.
func (c *Corpus) BoxesFile() string {
	if c.BoxesRef == "" {
		return ""
	}
	if filepath.IsAbs(c.BoxesRef) {
		return c.BoxesRef
	}
	return filepath.Join(filepath.Dir(c.Path), c.BoxesRef)
}

// PhotoFile resolves an entry's photo against the corpus file's directory.
func (c *Corpus) PhotoFile(e CorpusEntry) string {
	if e.Photo == "" || filepath.IsAbs(e.Photo) {
		return e.Photo
	}
	return filepath.Join(filepath.Dir(c.Path), e.Photo)
}

// ReviewedCount is how many entries carry a human answer.
func (c *Corpus) ReviewedCount() int {
	n := 0
	for _, e := range c.Entries {
		if e.Reviewed() {
			n++
		}
	}
	return n
}

// HasPhoto reports whether the corpus already covers a photo path. cmd/corpus
// uses it to resume without re-paying for identification.
func (c *Corpus) HasPhoto(rel string) bool {
	for _, e := range c.Entries {
		if e.Photo == rel {
			return true
		}
	}
	return false
}

// LoadCorpus reads and parses a corpus file.
func LoadCorpus(path string) (*Corpus, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err // os.IsNotExist must survive for the harness's skip path
	}
	defer f.Close()
	return ParseCorpus(f, path)
}

// ParseCorpus parses a corpus file. Every malformed or unrecognized line is an
// error rather than a silent skip: a reviewer who types "bxo: Tools 1" has
// done the work and would otherwise see it counted as unreviewed, which is the
// one failure mode that wastes their time instead of ours.
func ParseCorpus(r io.Reader, path string) (*Corpus, error) {
	c := &Corpus{Path: path}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	lineNo := 0
	seen := map[string]int{}
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		// Comments are whole lines only. A '#' mid-line is literal, because
		// item names and notes ("Widget #3", "shelf #2") legitimately contain
		// one and a model writes them without asking us first.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected \"key: value\", got %q", path, lineNo, line)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)

		if key == "photo" {
			if val == "" {
				return nil, fmt.Errorf("%s:%d: photo: needs a path", path, lineNo)
			}
			if prev, dup := seen[val]; dup {
				return nil, fmt.Errorf("%s:%d: photo %q already appears at line %d", path, lineNo, val, prev)
			}
			seen[val] = lineNo
			c.Entries = append(c.Entries, CorpusEntry{
				Photo: val,
				Line:  lineNo,
				// Quantity 1 rather than 0 so an entry that omits the field
				// means one item, not an item Normalize has to repair.
				Draft: ItemDraft{Quantity: 1},
			})
			continue
		}

		if len(c.Entries) == 0 {
			if err := c.setHeader(key, val, lineNo); err != nil {
				return nil, err
			}
			continue
		}
		if err := setEntryField(&c.Entries[len(c.Entries)-1], key, val, path, lineNo); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	for i := range c.Entries {
		c.Entries[i].Draft.Normalize()
	}
	return c, nil
}

func (c *Corpus) setHeader(key, val string, line int) error {
	switch key {
	case "boxes":
		c.BoxesRef = val
	case "top1-floor", "top3-floor":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("%s:%d: %s must be a number between 0 and 1: %w", c.Path, line, key, err)
		}
		if f < 0 || f > 1 {
			return fmt.Errorf("%s:%d: %s is %v, must be a rate between 0 and 1", c.Path, line, key, f)
		}
		if key == "top1-floor" {
			c.Top1Floor = f
		} else {
			c.Top3Floor = f
		}
	default:
		return fmt.Errorf("%s:%d: unknown header field %q (want boxes, top1-floor, top3-floor)", c.Path, line, key)
	}
	return nil
}

func setEntryField(e *CorpusEntry, key, val, path string, line int) error {
	switch key {
	case "name":
		e.Draft.Name = val
	case "category":
		e.Draft.Category = val
	case "size":
		if _, ok := NominalLitres[strings.ToUpper(strings.TrimSpace(val))]; !ok {
			return fmt.Errorf("%s:%d: size %q must be S, M, L or XL", path, line, val)
		}
		e.Draft.SizeBucket = val
	case "fragile":
		b, err := parseCorpusBool(val)
		if err != nil {
			return fmt.Errorf("%s:%d: fragile: %w", path, line, err)
		}
		e.Draft.Fragile = b
	case "weight":
		switch strings.ToLower(val) {
		case "light", "medium", "heavy":
			e.Draft.WeightClass = strings.ToLower(val)
		default:
			return fmt.Errorf("%s:%d: weight %q must be light, medium or heavy", path, line, val)
		}
	case "quantity":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("%s:%d: quantity %q must be a whole number: %w", path, line, val, err)
		}
		e.Draft.Quantity = n
	case "confidence":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("%s:%d: confidence %q must be a number: %w", path, line, val, err)
		}
		e.Draft.Confidence = f
	case "notes":
		e.Draft.Notes = val
	case "box":
		e.Expected = val
	default:
		return fmt.Errorf("%s:%d: unknown field %q in the entry for %s", path, line, key, e.Photo)
	}
	return nil
}

func parseCorpusBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "y", "true", "1":
		return true, nil
	case "no", "n", "false", "0", "":
		return false, nil
	}
	return false, fmt.Errorf("%q is not yes or no", v)
}

// LoadBoxes reads a box index snapshot. It accepts both a bare JSON array and
// the object GET /api/v1/boxes returns, so producing one is
//
//	curl -s localhost:8080/api/v1/boxes > boxes.json
//
// and not a transformation step the owner has to get right by hand.
func LoadBoxes(path string) ([]Box, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapped struct {
		Boxes []Box `json:"boxes"`
	}
	if err := json.Unmarshal(data, &wrapped.Boxes); err == nil {
		return wrapped.Boxes, nil
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("parsing box snapshot %s: %w", path, err)
	}
	if wrapped.Boxes == nil {
		return nil, fmt.Errorf("box snapshot %s contains no boxes", path)
	}
	return wrapped.Boxes, nil
}

// CorpusEntryResult is one scored entry.
type CorpusEntryResult struct {
	Entry CorpusEntry
	Rec   Recommendation
	// ExpectedID is the resolved box id, or NewContainerAnswer.
	ExpectedID string
	// ExpectedRank is where the reviewer's answer placed, 0-based, or -1 when
	// the engine never scored it (excluded by a hard rule, or no room).
	ExpectedRank  int
	ExpectedScore float64
	Top1, Top3    bool
}

// CorpusScore is the outcome of running the engine over a whole corpus.
type CorpusScore struct {
	Path     string
	Reviewed int
	Skipped  int // entries with no answer yet
	Top1Hits int
	Top3Hits int
	Results  []CorpusEntryResult
}

// Top1Rate and Top3Rate are the agreement rates. An empty corpus scores 0,
// which never clears a floor and never trips the ratchet.
func (s CorpusScore) Top1Rate() float64 { return rate(s.Top1Hits, s.Reviewed) }

// Top3Rate is the share of items whose answer appeared in the top three.
func (s CorpusScore) Top3Rate() float64 { return rate(s.Top3Hits, s.Reviewed) }

func rate(hits, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(hits) / float64(n)
}

// ScoreCorpus runs Recommend over every reviewed entry and reports top-1 and
// top-3 agreement with the reviewer's answers.
//
// An answer that names no box in the snapshot is an ERROR, not a miss. A typo
// scored as a miss would quietly drag the measurement down and be indis-
// tinguishable from the engine being wrong, which is exactly the confusion the
// corpus exists to remove.
func ScoreCorpus(c *Corpus, boxes []Box, w Weights) (CorpusScore, error) {
	s := CorpusScore{Path: c.Path}
	byID := make(map[string]Box, len(boxes))
	byName := map[string][]Box{}
	names := make([]string, 0, len(boxes))
	for _, b := range boxes {
		byID[b.ID] = b
		key := foldBoxName(b.Name)
		byName[key] = append(byName[key], b)
		names = append(names, b.Name)
	}
	sort.Strings(names)

	for _, e := range c.Entries {
		if !e.Reviewed() {
			s.Skipped++
			continue
		}
		r := CorpusEntryResult{Entry: e, ExpectedRank: -1, ExpectedScore: math.NaN()}
		if e.ExpectsNewContainer() {
			r.ExpectedID = NewContainerAnswer
		} else {
			b, err := resolveExpected(e.Expected, byID, byName)
			if err != nil {
				return s, fmt.Errorf("%s:%d: box: %w (boxes in %s: %s)",
					c.Path, e.Line, err, c.BoxesRef, strings.Join(names, ", "))
			}
			r.ExpectedID = b.ID
		}

		r.Rec = Recommend(e.Draft, boxes, w)
		if r.ExpectedID == NewContainerAnswer {
			// "A new container was right" is scored on the engine's own
			// fallback, which fires exactly when nothing clears MinViableScore.
			// It has no rank, so top-1 and top-3 agree by construction.
			hit := r.Rec.NewContainer != nil
			r.Top1, r.Top3 = hit, hit
		} else {
			for i, cand := range r.Rec.Candidates {
				if cand.Box.ID == r.ExpectedID {
					r.ExpectedRank, r.ExpectedScore = i, cand.Score
					break
				}
			}
			r.Top1 = r.ExpectedRank == 0
			r.Top3 = r.ExpectedRank >= 0 && r.ExpectedRank < 3
		}

		s.Reviewed++
		if r.Top1 {
			s.Top1Hits++
		}
		if r.Top3 {
			s.Top3Hits++
		}
		s.Results = append(s.Results, r)
	}
	return s, nil
}

// foldBoxName makes matching a hand-typed box name forgiving about case and
// spacing, and nothing else. Deliberately not NormalizeCategory: that folds
// aliases, and a reviewer naming a box has named a box, not a category.
func foldBoxName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func resolveExpected(answer string, byID map[string]Box, byName map[string][]Box) (Box, error) {
	if b, ok := byID[strings.TrimSpace(answer)]; ok {
		return b, nil
	}
	matches := byName[foldBoxName(answer)]
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Box{}, fmt.Errorf("%q is not a box in the snapshot", answer)
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return Box{}, fmt.Errorf("%q names %d boxes; use an id instead (%s)", answer, len(matches), strings.Join(ids, ", "))
	}
}

// Floor-check outcomes. Both are failures; they are distinguished because the
// fix is opposite -- one means the engine got worse, the other means the
// recorded floor is stale and must be raised.
var (
	// ErrCorpusRegression means measured agreement fell below the floor.
	ErrCorpusRegression = errors.New("placement agreement regressed")
	// ErrCorpusFloorStale means agreement improved by at least one whole item
	// and the floor no longer measures anything.
	ErrCorpusFloorStale = errors.New("corpus floor is stale")
)

// floorEpsilon absorbs float division noise (4/6 is not 0.6666666666666666 by
// every route). It is far smaller than one item's worth of any plausible
// corpus, so it can never hide a real regression.
const floorEpsilon = 1e-9

// FormatFloor renders a rate as a corpus file writes it. It truncates rather
// than rounding, because rounding UP records a floor the measurement that
// produced it does not clear: 4/6 printed to four places as 0.6667 is greater
// than 4/6, and the very next run of the same unchanged engine fails as a
// regression against a floor it just set. Truncation can only ever be
// generous, and never by more than a ten-thousandth.
func FormatFloor(rate float64) string {
	return strconv.FormatFloat(math.Floor(rate*10000)/10000, 'f', 4, 64)
}

// CheckFloors compares measured agreement against the floors recorded in the
// corpus and returns an error when either side of the ratchet trips.
//
// Failing when agreement IMPROVES looks perverse, so: the floor is only
// evidence while it tracks reality. Left alone, a floor of 0.60 under a corpus
// that now scores 0.95 permits a silent collapse back to 0.61 -- the check
// still passes, and the thing it was built to catch walks straight through.
// Ratcheting keeps the gap bounded. The tolerance is one item's worth
// (1/reviewed) rather than zero so a floor rounded off by hand to 0.66 is
// still accepted for a measured 0.6666, and `-corpus.update` writes the
// measured value for anyone who would rather not think about it.
//
// The measurement is deterministic -- Recommend is a pure function of the
// draft and the snapshot -- so an exact ratchet would be sound; the tolerance
// exists for the human writing the file, not for noise in the numbers.
func (s CorpusScore) CheckFloors(top1Floor, top3Floor float64) error {
	if s.Reviewed == 0 {
		return nil
	}
	slack := 1 / float64(s.Reviewed)
	var regress, stale []string
	check := func(label string, got, floor float64) {
		switch {
		case got < floor-floorEpsilon:
			regress = append(regress, fmt.Sprintf("%s agreement %.1f%% is below the recorded floor of %.1f%%", label, got*100, floor*100))
		case got >= floor+slack-floorEpsilon:
			stale = append(stale, fmt.Sprintf("%s agreement %.1f%% exceeds its floor of %.1f%% by more than one item", label, got*100, floor*100))
		}
	}
	check("top-1", s.Top1Rate(), top1Floor)
	check("top-3", s.Top3Rate(), top3Floor)

	if len(regress) > 0 {
		return fmt.Errorf("%w: %s\nThe last change made recommendations worse for real items. "+
			"Fix it, or -- if the new behavior is genuinely better and the corpus answers are wrong -- "+
			"correct the answers in %s and lower the floor deliberately, in the same commit, with a reason",
			ErrCorpusRegression, strings.Join(regress, "; "), s.Path)
	}
	if len(stale) > 0 {
		return fmt.Errorf("%w: %s\nRecord the improvement so it cannot be silently lost: "+
			"go test ./internal/placement -run TestCorpus -corpus.update\n"+
			"(or set top1-floor: %s and top3-floor: %s in %s)",
			ErrCorpusFloorStale, strings.Join(stale, "; "),
			FormatFloor(s.Top1Rate()), FormatFloor(s.Top3Rate()), s.Path)
	}
	return nil
}

// Report renders the measurement with per-item detail for every disagreement.
// A bare percentage tells nobody what to fix, so each miss shows the reviewer's
// answer, the engine's ranking, the scores, and the reasons the engine gave --
// which together are usually enough to see which weight is responsible.
func (s CorpusScore) Report(top1Floor, top3Floor float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "corpus %s: %d reviewed", s.Path, s.Reviewed)
	if s.Skipped > 0 {
		fmt.Fprintf(&b, ", %d not yet reviewed", s.Skipped)
	}
	fmt.Fprintf(&b, "\n  top-1 agreement  %d/%d  %.1f%%  (floor %.1f%%)\n",
		s.Top1Hits, s.Reviewed, s.Top1Rate()*100, top1Floor*100)
	fmt.Fprintf(&b, "  top-3 agreement  %d/%d  %.1f%%  (floor %.1f%%)\n",
		s.Top3Hits, s.Reviewed, s.Top3Rate()*100, top3Floor*100)

	misses := 0
	for _, r := range s.Results {
		if r.Top1 {
			continue
		}
		misses++
		if misses == 1 {
			b.WriteString("\ndisagreements:\n")
		}
		b.WriteString(r.detail())
	}
	if misses == 0 {
		b.WriteString("\nno disagreements\n")
	}
	return b.String()
}

func (r CorpusEntryResult) detail() string {
	var b strings.Builder
	e := r.Entry
	fmt.Fprintf(&b, "\n  %s  (line %d)\n", e.Photo, e.Line)
	fmt.Fprintf(&b, "    item:     %q  category=%s size=%s weight=%s fragile=%t qty=%d\n",
		e.Draft.Name, e.Draft.Category, e.Draft.SizeBucket, e.Draft.WeightClass, e.Draft.Fragile, e.Draft.Quantity)

	switch {
	case r.ExpectedID == NewContainerAnswer:
		b.WriteString("    expected: a NEW container\n")
	case r.ExpectedRank < 0:
		fmt.Fprintf(&b, "    expected: %s -- which the engine did not rank at all "+
			"(excluded by a hard rule, or no room)\n", e.Expected)
	default:
		fmt.Fprintf(&b, "    expected: %s -- ranked #%d, score %.3f%s\n",
			e.Expected, r.ExpectedRank+1, r.ExpectedScore, top3Note(r.Top3))
	}

	if len(r.Rec.Candidates) == 0 {
		b.WriteString("    engine:   no candidates\n")
	}
	for i, c := range r.Rec.Candidates {
		if i >= 3 {
			break
		}
		fmt.Fprintf(&b, "    engine:   #%d %-16s %.3f  (%s)\n", i+1, c.Box.Name, c.Score, strings.Join(c.Reasons, "; "))
	}
	if n := r.Rec.NewContainer; n != nil {
		fmt.Fprintf(&b, "    engine:   suggests a new container: %s (%s, %s) -- %s\n",
			n.Label, n.SizeBucket, n.Access, n.Reason)
	} else if r.ExpectedID == NewContainerAnswer {
		b.WriteString("    engine:   suggested NO new container\n")
	}
	return b.String()
}

func top3Note(top3 bool) string {
	if top3 {
		return " (top-3 hit)"
	}
	return ""
}

// UpdateCorpusFloors rewrites the two floor lines in place, touching nothing
// else in the file. A full re-serialization would be simpler and would also
// delete every comment the reviewer wrote next to their answers, which is
// their notes on the hardest calls in the corpus.
//
// It refuses to LOWER a floor. Lowering is how a regression gets laundered
// into a new baseline by whoever is annoyed at a red test; if the engine
// genuinely improved and an answer was wrong, that is a deliberate edit with a
// reason in the commit message, not a flag.
func UpdateCorpusFloors(path string, top1, top3 float64) error {
	c, err := LoadCorpus(path)
	if err != nil {
		return err
	}
	if top1 < c.Top1Floor-floorEpsilon || top3 < c.Top3Floor-floorEpsilon {
		return fmt.Errorf("refusing to lower the floors in %s (top-1 %.4f -> %.4f, top-3 %.4f -> %.4f): "+
			"that is a regression, not an update; edit the file by hand if it is intended",
			path, c.Top1Floor, top1, c.Top3Floor, top3)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Preserve the file's own line ending style rather than imposing "\n".
	nl := "\n"
	if strings.Contains(string(data), "\r\n") {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")

	want := map[string]string{
		"top1-floor": "top1-floor: " + FormatFloor(top1),
		"top3-floor": "top3-floor: " + FormatFloor(top3),
	}
	written := map[string]bool{}
	insertAt := len(lines)
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(t), "photo:") {
			insertAt = i // header ends at the first entry
			break
		}
		for key, repl := range want {
			if strings.HasPrefix(strings.ToLower(t), key+":") {
				lines[i] = repl
				written[key] = true
			}
		}
	}
	// A corpus written before floors existed, or with one hand-deleted, still
	// has to end up with both -- otherwise -corpus.update silently no-ops and
	// the next run reports the same stale floor.
	var missing []string
	for _, key := range []string{"top1-floor", "top3-floor"} {
		if !written[key] {
			missing = append(missing, want[key])
		}
	}
	if len(missing) > 0 {
		out := append([]string{}, lines[:insertAt]...)
		out = append(out, missing...)
		out = append(out, "")
		lines = append(out, lines[insertAt:]...)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, nl)), 0o644)
}

// CorpusHeader renders the top of a new corpus file: the instructions a
// reviewer needs and, when a box snapshot is to hand, the list of boxes they
// are choosing among. The list is there because the alternative is twenty
// round trips to another window to remember what the boxes are called, and a
// reviewer who guesses at a name produces an error, not an answer.
func CorpusHeader(boxesRef string, boxes []Box) string {
	var b strings.Builder
	b.WriteString(`# Boxwright placement corpus
#
# One entry per photo: the draft a vision model produced, and one field for
# you to fill in -- box:. Write where you would ACTUALLY put the item. That
# answer is the ground truth the placement engine is measured against; the
# draft above it is only what the model guessed, and correcting a wrong draft
# field is worth doing too.
#
#   box: Tools 1     an existing box, by name (case-insensitive) or by id
#   box: new         nothing existing fits; a new container is the right answer
#   box:             left blank: not reviewed yet, and simply not counted
#
# A line starting with # is a comment; a # anywhere else is ordinary text.
# Re-running the corpus tool keeps every answer and comment already here and
# only identifies photos it has not seen before.
#
# Measure with:  go test ./internal/placement -run TestCorpus -v
`)
	b.WriteString("\n")
	if boxesRef != "" {
		b.WriteString("# The box index these answers are chosen against.\n")
		fmt.Fprintf(&b, "boxes: %s\n", boxesRef)
	}
	b.WriteString(`
# Agreement below these fails the harness. Start at 0 and run the test: it
# reports the rates actually measured and tells you how to record them.
top1-floor: 0.0000
top3-floor: 0.0000
`)
	if len(boxes) > 0 {
		b.WriteString("\n# Boxes available, for reference while you answer:\n")
		b.WriteString(BoxReference(boxes))
	}
	return b.String()
}

// BoxReference renders a box snapshot as comment lines.
func BoxReference(boxes []Box) string {
	width := 0
	for _, b := range boxes {
		if len(b.Name) > width {
			width = len(b.Name)
		}
	}
	var b strings.Builder
	for _, box := range boxes {
		// An unrecorded capacity prints as "?": it is unknown, not 8.
		capacity := "?"
		if box.CapacityUnits > 0 {
			capacity = fmt.Sprint(box.CapacityUnits)
		}
		cats := make([]string, 0, len(box.Categories))
		for k, n := range box.Categories {
			cats = append(cats, fmt.Sprintf("%s x%d", k, n))
		}
		sort.Strings(cats) // map order is random; a file that changes on every run is unreviewable
		access := box.Access
		if access == "" {
			access = "-"
		}
		area := box.Area
		if area == "" {
			area = "-"
		}
		fmt.Fprintf(&b, "#   %-*s  %-12s %-6s %d/%s used  %s\n",
			width, box.Name, area, access, box.UsedUnits, capacity, strings.Join(cats, ", "))
	}
	return b.String()
}

// FormatCorpusEntry renders one entry in the file format. cmd/corpus appends
// the result; nothing rewrites an existing entry, so a reviewer's work is
// physically unreachable by the tool that produced it.
func FormatCorpusEntry(e CorpusEntry) string {
	// Normalize before writing, not after reading: a provider that returned no
	// size would otherwise produce `size:` in the file, which the parser
	// rightly rejects -- and it would reject it on a run the owner already
	// paid for. The parser stays strict; the writer never emits what it
	// cannot read back.
	d := e.Draft
	d.Normalize()
	var b strings.Builder
	b.WriteString("\n")
	fmt.Fprintf(&b, "photo: %s\n", oneLine(e.Photo))
	fmt.Fprintf(&b, "name: %s\n", oneLine(d.Name))
	fmt.Fprintf(&b, "category: %s\n", oneLine(d.Category))
	fmt.Fprintf(&b, "size: %s\n", d.SizeBucket)
	fmt.Fprintf(&b, "fragile: %s\n", yesNo(d.Fragile))
	fmt.Fprintf(&b, "weight: %s\n", d.WeightClass)
	fmt.Fprintf(&b, "quantity: %d\n", d.Quantity)
	fmt.Fprintf(&b, "confidence: %.2f\n", d.Confidence)
	fmt.Fprintf(&b, "notes: %s\n", oneLine(d.Notes))
	fmt.Fprintf(&b, "box: %s\n", oneLine(e.Expected))
	return b.String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// oneLine flattens a value so a model's newline cannot forge a new field. The
// format is line-oriented, so an unescaped newline in notes would turn the
// rest of that note into unknown keys and fail the parse of a file the user
// paid to produce.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}
