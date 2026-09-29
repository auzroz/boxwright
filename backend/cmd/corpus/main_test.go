package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxwright/internal/placement"
)

// stubIdentifier stands in for a vision provider. It records what it was asked
// so the tests can assert on the thing that actually costs money: how many
// photos were sent.
type stubIdentifier struct {
	seen     []string // image payloads, which the fixtures make identifying
	failOn   map[string]bool
	costEach float64
	total    float64
	perPhoto map[string]int // image payload -> objects found; absent means 1
}

// perPhoto lets a test say how many objects a photo yields, so the multi-item
// path (a shelf in one shot) is exercised alongside the single-item one.
func (s *stubIdentifier) Identify(_ context.Context, image []byte, mime string, cats []placement.Category) ([]placement.ItemDraft, error) {
	body := string(image)
	s.seen = append(s.seen, body)
	s.total += s.costEach
	if s.failOn[body] {
		return nil, errors.New("model refused")
	}
	n := s.perPhoto[body]
	if n == 0 {
		n = 1
	}
	drafts := make([]placement.ItemDraft, 0, n)
	for i := 0; i < n; i++ {
		name := "Item from " + body
		if n > 1 {
			name = fmt.Sprintf("Item %d from %s", i+1, body)
		}
		drafts = append(drafts, placement.ItemDraft{
			Name: name, Category: "tools", SizeBucket: "M",
			WeightClass: "medium", Confidence: 0.9, Quantity: 1,
			Notes: "mime=" + mime + " vocab=" + placement.VocabularyOf(cats),
		})
	}
	return drafts, nil
}

func (s *stubIdentifier) CostUSD() float64 { return s.total }

func photoDir(t *testing.T, root string, names ...string) string {
	t.Helper()
	dir := filepath.Join(root, "photos")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func boxSnapshot(t *testing.T, root string) string {
	t.Helper()
	p := filepath.Join(root, "boxes.json")
	body := `[{"id":"t","name":"Tools 1","area":"Row A","access":"easy","capacityUnits":8,"usedUnits":4,
	           "categories":{"tools":6,"micro-masterpieces":3}}]`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateWritesAReviewableCorpus(t *testing.T) {
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg", "b.png")
	out := filepath.Join(root, "real.corpus")
	id := &stubIdentifier{costEach: 0.03}

	var log bytes.Buffer
	o := options{photosDir: photos, outPath: out, boxesPath: boxSnapshot(t, root)}
	if err := generate(context.Background(), o, id, &log); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// The file the reviewer opens must parse with the harness's own parser --
	// otherwise the run is paid for and unusable.
	c, err := placement.LoadCorpus(out)
	if err != nil {
		t.Fatalf("the corpus we just wrote does not parse: %v", err)
	}
	if len(c.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(c.Entries))
	}
	if c.BoxesRef != "boxes.json" {
		t.Errorf("boxes ref = %q, want the snapshot beside the corpus", c.BoxesRef)
	}
	if _, err := placement.LoadBoxes(c.BoxesFile()); err != nil {
		t.Errorf("the recorded boxes ref does not resolve: %v", err)
	}
	for _, e := range c.Entries {
		if e.Reviewed() {
			t.Errorf("%s came with an answer already filled in; the human writes that", e.Photo)
		}
		if !strings.HasPrefix(e.Photo, "photos/") {
			t.Errorf("photo %q should be recorded relative to the corpus file", e.Photo)
		}
		if _, err := os.Stat(c.PhotoFile(e)); err != nil {
			t.Errorf("recorded photo path does not resolve back to the file: %v", err)
		}
	}
	// The prompt vocabulary must include the user's own tag, not just our seeds.
	if !strings.Contains(c.Entries[0].Draft.Notes, "micro-masterpieces") {
		t.Errorf("the box snapshot's own categories are missing from the prompt: %q", c.Entries[0].Draft.Notes)
	}
	if !strings.Contains(c.Entries[0].Draft.Notes, "mime=image/jpeg") {
		t.Errorf("mime type not derived from the extension: %q", c.Entries[0].Draft.Notes)
	}
	// The header has to tell the reviewer what the boxes are called, or they
	// are guessing at the one field only they can fill in.
	body := readFile(t, out)
	if !strings.Contains(body, "Tools 1") {
		t.Errorf("header does not list the boxes:\n%s", body)
	}
	if got := log.String(); !strings.Contains(got, "$0.0600") {
		t.Errorf("running cost not reported: %q", got)
	}
}

func TestGenerateResumesWithoutPayingTwice(t *testing.T) {
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg")
	out := filepath.Join(root, "real.corpus")
	id := &stubIdentifier{costEach: 0.03}
	o := options{photosDir: photos, outPath: out, boxesPath: boxSnapshot(t, root)}

	if err := generate(context.Background(), o, id, &bytes.Buffer{}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The reviewer answers, and leaves a note to themselves.
	body := readFile(t, out) + "\n"
	body = strings.Replace(body, "box: \n", "box: Tools 1\n", 1)
	body = strings.Replace(body, "box:\n", "box: Tools 1\n", 1)
	body += "# a note the reviewer would be furious to lose\n"
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// A second photo arrives; a second run must identify only that one.
	if err := os.WriteFile(filepath.Join(photos, "b.jpg"), []byte("b.jpg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(context.Background(), o, id, &bytes.Buffer{}); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if want := []string{"a.jpg", "b.jpg"}; !equal(id.seen, want) {
		t.Fatalf("identified %v, want %v -- a resumed run must not re-pay for a photo", id.seen, want)
	}
	c, err := placement.LoadCorpus(out)
	if err != nil {
		t.Fatalf("parsing after resume: %v", err)
	}
	if len(c.Entries) != 2 {
		t.Fatalf("want 2 entries after resume, got %d", len(c.Entries))
	}
	if c.Entries[0].Expected != "Tools 1" {
		t.Errorf("resume lost the reviewer's answer: %q", c.Entries[0].Expected)
	}
	if !strings.Contains(readFile(t, out), "furious to lose") {
		t.Error("resume lost the reviewer's comment")
	}
	// A third run with nothing new must spend nothing at all.
	before := len(id.seen)
	if err := generate(context.Background(), o, id, &bytes.Buffer{}); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(id.seen) != before {
		t.Errorf("a run with no new photos identified %d more", len(id.seen)-before)
	}
}

func TestGenerateWritesTheHeaderOnce(t *testing.T) {
	// The header is instructions, not data. A second copy in the middle of the
	// file is noise in the one place the reviewer is reading closely.
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg")
	out := filepath.Join(root, "real.corpus")
	o := options{photosDir: photos, outPath: out, boxesPath: boxSnapshot(t, root)}
	if err := generate(context.Background(), o, &stubIdentifier{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photos, "b.jpg"), []byte("b.jpg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(context.Background(), o, &stubIdentifier{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readFile(t, out), "top1-floor:"); n != 1 {
		t.Fatalf("header written %d times", n)
	}
}

func TestGenerateWarnsWhenTheCorpusNamesNoSnapshot(t *testing.T) {
	// Without a snapshot the answers cannot be scored, and discovering that at
	// measuring time is discovering it after all the reviewing is done.
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg")
	var log bytes.Buffer
	o := options{photosDir: photos, outPath: filepath.Join(root, "real.corpus")}
	if err := generate(context.Background(), o, &stubIdentifier{}, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "names no box snapshot") {
		t.Errorf("no warning about the missing snapshot:\n%s", log.String())
	}
}

func TestGenerateRefusesAnUnparseableCorpus(t *testing.T) {
	// Discovering the file is broken AFTER paying for identification is the
	// one ordering that wastes the owner's money.
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg")
	out := filepath.Join(root, "real.corpus")
	if err := os.WriteFile(out, []byte("photo: x.jpg\nbxo: Tools 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := &stubIdentifier{}
	err := generate(context.Background(), options{photosDir: photos, outPath: out}, id, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(id.seen) != 0 {
		t.Errorf("spent %d identifications before noticing the file was broken", len(id.seen))
	}
}

func TestGenerateSurvivesOnePhotoFailing(t *testing.T) {
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg", "b.jpg", "c.jpg")
	out := filepath.Join(root, "real.corpus")
	id := &stubIdentifier{failOn: map[string]bool{"b.jpg": true}}

	var log bytes.Buffer
	if err := generate(context.Background(), options{photosDir: photos, outPath: out}, id, &log); err != nil {
		t.Fatalf("one bad photo must not abandon a paid run: %v", err)
	}
	c, err := placement.LoadCorpus(out)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if len(c.Entries) != 2 {
		t.Fatalf("want the 2 photos that worked, got %d", len(c.Entries))
	}
	if !strings.Contains(log.String(), "FAILED") || !strings.Contains(log.String(), "1 failed") {
		t.Errorf("the failure was not reported:\n%s", log.String())
	}
	// The next run retries only the one that failed.
	before := len(id.seen)
	id.failOn = nil
	if err := generate(context.Background(), options{photosDir: photos, outPath: out}, id, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := id.seen[before:]; !equal(got, []string{"b.jpg"}) {
		t.Errorf("retried %v, want just the failed photo", got)
	}
}

func TestGenerateDryRunWritesAndSpendsNothing(t *testing.T) {
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg")
	out := filepath.Join(root, "real.corpus")
	id := &stubIdentifier{}

	var log bytes.Buffer
	o := options{photosDir: photos, outPath: out, dryRun: true}
	if err := generate(context.Background(), o, id, &log); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(id.seen) != 0 {
		t.Errorf("a dry run identified %d photos", len(id.seen))
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a dry run created %s", out)
	}
	if !strings.Contains(log.String(), "would identify") {
		t.Errorf("dry run said nothing useful:\n%s", log.String())
	}
}

func TestGenerateLimitsSpendPerRun(t *testing.T) {
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg", "b.jpg", "c.jpg")
	id := &stubIdentifier{}
	o := options{photosDir: photos, outPath: filepath.Join(root, "real.corpus"), limit: 2}
	if err := generate(context.Background(), o, id, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !equal(id.seen, []string{"a.jpg", "b.jpg"}) {
		t.Fatalf("-n did not cap the run: identified %v", id.seen)
	}
}

func TestGenerateStopsOnCancellation(t *testing.T) {
	// Ctrl-C must leave a parseable file holding everything already paid for.
	root := t.TempDir()
	photos := photoDir(t, root, "a.jpg", "b.jpg")
	out := filepath.Join(root, "real.corpus")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id := &stubIdentifier{}
	if err := generate(ctx, options{photosDir: photos, outPath: out}, id, &bytes.Buffer{}); err != nil {
		t.Fatalf("cancellation is not a failure: %v", err)
	}
	if len(id.seen) != 0 {
		t.Errorf("identified %d photos after cancellation", len(id.seen))
	}
	if _, err := placement.LoadCorpus(out); err != nil {
		t.Fatalf("the file left behind must still parse: %v", err)
	}
}

func TestFindPhotosSkipsWhatIsNotAPhoto(t *testing.T) {
	root := t.TempDir()
	dir := photoDir(t, root, "b.jpg", "a.JPEG", "notes.txt", ".DS_Store", "c.heic")
	// A cache directory of thumbnails is not part of the corpus.
	if err := os.MkdirAll(filepath.Join(dir, ".thumbs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".thumbs", "t.jpg"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findPhotos(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}
	// Lexical order, so -n means the same photos on a second run.
	if !equal(names, []string{"a.JPEG", "b.jpg", "c.heic"}) {
		t.Fatalf("found %v", names)
	}
}

func TestVocabularyPutsTheUsersTagsFirst(t *testing.T) {
	boxes := []placement.Box{{Categories: map[string]int{"micro-masterpieces": 3, "tools": 6}}}
	got := vocabulary(boxes)
	if len(got) < 2 || got[0].Key != "micro-masterpieces" || got[1].Key != "tools" {
		t.Fatalf("user tags must lead the vocabulary, got %+v", got[:min(2, len(got))])
	}
	seen := map[string]int{}
	for _, c := range got {
		seen[c.Key]++
	}
	if seen["tools"] != 1 {
		t.Errorf("a tag that is also a seed must appear once, got %d", seen["tools"])
	}
	if seen["kitchen"] != 1 {
		t.Error("canonical seeds should still fill the gaps")
	}
	// An empty inventory still gets the seeds, or the prompt has no guidance.
	if len(vocabulary(nil)) != len(placement.CanonicalCategories()) {
		t.Error("an empty snapshot should fall back to the canonical seeds")
	}
}

func TestSpentReportsNothingWhenTheProviderCannot(t *testing.T) {
	if got := spent(&stubIdentifier{total: 0.25}); got != "  [$0.2500 so far]" {
		t.Errorf("cost line = %q", got)
	}
	// A provider with no notion of cost must produce no cost text at all --
	// "$0.00" beside a local model reads as a measurement, not an absence.
	if got := spent(noCostIdentifier{}); got != "" {
		t.Errorf("want no cost text, got %q", got)
	}
}

// noCostIdentifier is an ai.Identifier with no CostUSD method at all.
type noCostIdentifier struct{}

func (noCostIdentifier) Identify(context.Context, []byte, string, []placement.Category) ([]placement.ItemDraft, error) {
	return nil, nil
}

func TestRelativeToFallsBackToAbsolute(t *testing.T) {
	// A photo folder outside the corpus directory must still resolve, so an
	// absolute path is better than a "../../.." that breaks when the corpus
	// moves.
	root := t.TempDir()
	inside := filepath.Join(root, "corpus", "photos", "a.jpg")
	if got, want := relativeTo(filepath.Join(root, "corpus"), inside), "photos/a.jpg"; got != want {
		t.Errorf("relativeTo = %q, want %q", got, want)
	}
	outside := filepath.Join(root, "elsewhere", "a.jpg")
	got := relativeTo(filepath.Join(root, "corpus"), outside)
	if !filepath.IsAbs(got) {
		t.Errorf("relativeTo = %q, want an absolute path", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
