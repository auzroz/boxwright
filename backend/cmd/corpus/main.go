// Command corpus turns a folder of photographs into a reviewable placement
// corpus: one entry per photo, carrying the draft the configured vision
// provider identified and a blank `box:` line for the answer only a human can
// give.
//
// It exists because the placement weights in internal/placement/engine.go were
// tuned by argument. Nothing in the repository can currently say whether
// raising CategoryMatch helps or hurts, so every future tuning change is a
// guess. The expensive half of fixing that is a person looking at twenty
// photos and saying where each thing really goes; this tool does the cheap
// half and gets out of their way.
//
//	corpus -photos ~/unit-photos -out corpus/real.corpus -boxes corpus/boxes.json
//	corpus -photos ~/unit-photos -out corpus/real.corpus -n 5      # cap the spend
//	corpus -photos ~/unit-photos -out corpus/real.corpus -dry      # cost nothing
//
// Then fill in each `box:` and measure with:
//
//	go test ./internal/placement -run TestCorpus -v
//
// Re-running is safe and cheap: entries already in the file are never
// re-identified and never rewritten, so a run that dies partway costs nothing
// the second time and cannot lose an answer already written.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"boxwright/internal/ai"
	"boxwright/internal/config"
	"boxwright/internal/placement"
)

// identifyTimeout bounds one photo. Generous, because a local Ollama on CPU is
// slow and a corpus run is not interactive; the point is only that a wedged
// provider cannot stall a twenty-photo run forever.
const identifyTimeout = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "corpus:", err)
		os.Exit(1)
	}
}

type options struct {
	photosDir string
	outPath   string
	boxesPath string // as typed on the command line; "" when not supplied
	limit     int
	dryRun    bool
}

func run() error {
	var o options
	flag.StringVar(&o.photosDir, "photos", "", "directory of photographs to identify (searched recursively)")
	flag.StringVar(&o.outPath, "out", "corpus.txt", "corpus file to create or extend")
	flag.StringVar(&o.boxesPath, "boxes", "", "box index snapshot: curl -s localhost:8080/api/v1/boxes > boxes.json")
	flag.IntVar(&o.limit, "n", 0, "identify at most this many new photos this run (0 = all)")
	flag.BoolVar(&o.dryRun, "dry", false, "list what would be identified without calling the model")
	flag.Parse()

	if o.photosDir == "" {
		flag.Usage()
		return errors.New("-photos is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ident, err := ai.New(cfg.AIProvider, cfg.AIBaseURL, cfg.AIAPIKey, cfg.AIModel)
	if err != nil {
		return err
	}
	if cfg.AIProvider == "none" && !o.dryRun {
		// Everywhere else in Boxwright "none" is a supported mode with a
		// manual path. Here identification IS the job, and a run that produced
		// twenty empty drafts would be worse than no run: the reviewer would
		// have to type the drafts as well as the answers.
		return fmt.Errorf("AI_PROVIDER=none, so there is nothing to identify photos with; " +
			"set a provider (see .env.example), or use -dry to see which photos would be processed")
	}

	// Ctrl-C stops after the photo in flight rather than mid-write, so the
	// file left behind is always parseable and everything already paid for is
	// already on disk.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return generate(ctx, o, ident, os.Stdout)
}

// costReporter is the optional half of ai.Identifier: a provider that knows
// what it has spent. It is declared here, not in internal/ai, because Go
// interfaces are structural -- a provider that grows a CostUSD method
// satisfies this with no import of this package -- and because Identifier
// itself must stay the smallest thing a model-agnostic backend can require. A
// local Ollama has no cost to report and should not have to say so.
type costReporter interface {
	// CostUSD is the total spent by this Identifier since it was built.
	CostUSD() float64
}

func generate(ctx context.Context, o options, ident ai.Identifier, out io.Writer) error {
	photos, err := findPhotos(o.photosDir)
	if err != nil {
		return err
	}
	if len(photos) == 0 {
		return fmt.Errorf("no images found under %s", o.photosDir)
	}

	var boxes []placement.Box
	if o.boxesPath != "" {
		if boxes, err = placement.LoadBoxes(o.boxesPath); err != nil {
			return fmt.Errorf("reading the box snapshot: %w", err)
		}
	}

	// Existing entries are the resume state. Parsing rather than grepping
	// means a corrupted file is reported now, before spending anything, not
	// after the run when the harness cannot read what we appended.
	existing := &placement.Corpus{Path: o.outPath}
	fresh := true // whether this run is creating the file, and so owes it a header
	if info, err := os.Stat(o.outPath); err == nil {
		// Size, not existence: an empty file left by a shell redirect still
		// needs the instructions, and a file with only comments in it must not
		// get a second copy of them.
		fresh = info.Size() == 0
		if !fresh {
			if existing, err = placement.LoadCorpus(o.outPath); err != nil {
				return fmt.Errorf("the corpus already at %s does not parse, so extending it would "+
					"produce a file the harness cannot read: %w", o.outPath, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	outDir := filepath.Dir(o.outPath)
	var todo []string
	for _, p := range photos {
		if existing.HasPhoto(relativeTo(outDir, p)) {
			continue
		}
		todo = append(todo, p)
	}
	fmt.Fprintf(out, "%d photos under %s; %d already in %s, %d to identify\n",
		len(photos), o.photosDir, len(photos)-len(todo), o.outPath, len(todo))
	if o.limit > 0 && len(todo) > o.limit {
		todo = todo[:o.limit]
		fmt.Fprintf(out, "limited to %d this run (-n)\n", len(todo))
	}
	if o.dryRun {
		for _, p := range todo {
			fmt.Fprintln(out, "  would identify", p)
		}
		fmt.Fprintln(out, "\nDry run: nothing was identified and nothing was written.")
		return nil
	}
	if len(todo) == 0 {
		fmt.Fprintf(out, "\nNothing to do. Fill in the box: lines in %s, then:\n"+
			"  go test ./internal/placement -run TestCorpus -v\n", o.outPath)
		return nil
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// Append-only, and never a rewrite of what is already there. The reviewer's
	// answers and their notes-in-comments are the expensive content in this
	// file; the tool that produced it should not be able to touch them.
	f, err := os.OpenFile(o.outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if fresh {
		ref := ""
		if o.boxesPath != "" {
			ref = relativeTo(outDir, o.boxesPath)
		}
		if _, err := io.WriteString(f, placement.CorpusHeader(ref, boxes)); err != nil {
			return err
		}
		existing.BoxesRef = ref
	}

	vocab := vocabulary(boxes)
	identified, failed := 0, 0
	for _, p := range todo {
		if ctx.Err() != nil {
			fmt.Fprintln(out, "interrupted; everything identified so far is saved")
			break
		}
		drafts, err := identify(ctx, ident, p, vocab)
		if err != nil {
			// One unreadable photo must not abandon a run that has already
			// spent money on the others.
			fmt.Fprintf(out, "  %-40s FAILED: %v\n", filepath.Base(p), err)
			failed++
			continue
		}
		if len(drafts) == 0 {
			// Not a failure to retry: the model looked and found nothing
			// packable. Say so, and leave the photo out rather than writing an
			// entry with nothing in it for the reviewer to answer about.
			fmt.Fprintf(out, "  %-40s no items found\n", filepath.Base(p))
			continue
		}
		// One entry per object. A shelf photographed in one shot is several
		// questions for the reviewer, not one.
		rel := relativeTo(outDir, p)
		for _, draft := range drafts {
			entry := placement.CorpusEntry{Photo: rel, Draft: draft}
			if _, err := io.WriteString(f, placement.FormatCorpusEntry(entry)); err != nil {
				return fmt.Errorf("writing %s: %w", o.outPath, err)
			}
			identified++
		}
		// Flush per photo: a crash or a kill on the next photo then costs the
		// next photo, not the whole run. Flushing after the whole group keeps
		// a photo's items together even if the run dies here.
		if err := f.Sync(); err != nil {
			return fmt.Errorf("flushing %s: %w", o.outPath, err)
		}
		names := make([]string, 0, len(drafts))
		for _, d := range drafts {
			names = append(names, fmt.Sprintf("%s (%s, %s)", d.Name, d.Category, d.SizeBucket))
		}
		fmt.Fprintf(out, "  %-40s %s%s\n",
			filepath.Base(p), strings.Join(names, "; "), spent(ident))
	}

	fmt.Fprintf(out, "\n%d identified, %d failed%s\n", identified, failed, spent(ident))
	// A corpus with no box snapshot cannot be scored, and finding that out at
	// measuring time is finding it out after the reviewing is done.
	if existing.BoxesRef == "" {
		ref := "<snapshot>"
		if o.boxesPath != "" {
			ref = relativeTo(outDir, o.boxesPath)
		}
		fmt.Fprintf(out, "\nThis corpus names no box snapshot. Add a line `boxes: %s` to the top of\n"+
			"%s, or the harness has no inventory to score the answers against.\n", ref, o.outPath)
	}
	fmt.Fprintf(out, "\nNow fill in the box: line under each entry in %s, then:\n"+
		"  go test ./internal/placement -run TestCorpus -v\n", o.outPath)
	return nil
}

// identify returns every draft the model found in one photo.
//
// A shelf photographed in one shot is several corpus entries, not one: each
// object needs its own expected box, because the whole point of the corpus is
// to ask "where would YOU put this?" of each thing individually. Collapsing
// them here would silently discard the reviewer's ability to answer for all
// but the first.
func identify(ctx context.Context, ident ai.Identifier, path string, vocab []placement.Category) ([]placement.ItemDraft, error) {
	image, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mime := mimeTypeOf(path)
	if mime == "" {
		return nil, fmt.Errorf("unsupported image type %q", filepath.Ext(path))
	}
	ctx, cancel := context.WithTimeout(ctx, identifyTimeout)
	defer cancel()
	drafts, err := ident.Identify(ctx, image, mime, vocab)
	if err != nil {
		return nil, fmt.Errorf("identifying %s: %w", filepath.Base(path), err)
	}
	return drafts, nil
}

// spent renders the running total for providers that report one, and nothing
// at all for those that do not -- "$0.00" beside a local model would be a
// measurement rather than an absence.
func spent(ident ai.Identifier) string {
	cr, ok := ident.(costReporter)
	if !ok {
		return ""
	}
	return fmt.Sprintf("  [$%.4f so far]", cr.CostUSD())
}

// vocabulary is the category list the prompt is built from: the keys the box
// snapshot is actually filed under -- which are the user's own Homebox tags --
// with the canonical seeds filling the gaps.
//
// It comes from the snapshot rather than from a live tag listing so the tool
// needs no Homebox connection, and so the vocabulary the drafts were produced
// under is the same one the harness later scores them against. Tags first, for
// the reason internal/api/categories.go gives: they are the real vocabulary
// and our seeds are only a starting point for an empty inventory.
func vocabulary(boxes []placement.Box) []placement.Category {
	seen := map[string]bool{}
	var keys []string
	for _, b := range boxes {
		for key := range b.Categories {
			if key != "" && !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys) // map order is random, and a prompt that changes per run is not reproducible

	out := make([]placement.Category, 0, len(keys)+len(placement.CanonicalCategories()))
	for _, key := range keys {
		out = append(out, placement.Category{
			Key:                key,
			Label:              placement.CategoryLabel(key),
			FrequentlyAccessed: placement.IsFrequentlyAccessed(key),
		})
	}
	for _, c := range placement.CanonicalCategories() {
		if !seen[c.Key] {
			out = append(out, c)
		}
	}
	return out
}

// imageMIME is the set of things a vision provider will accept. An extension
// not listed here is skipped silently during the walk (a photos folder often
// carries .DS_Store, sidecars and thumbnails) but is an error if named
// directly, so a typo'd path is never mistaken for an empty folder.
var imageMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".gif":  "image/gif",
	".heic": "image/heic",
	".heif": "image/heif",
}

func mimeTypeOf(path string) string { return imageMIME[strings.ToLower(filepath.Ext(path))] }

// findPhotos walks the directory in a stable order, so two runs over the same
// folder identify photos in the same sequence and -n means the same thing
// twice. WalkDir already sorts each directory's entries lexically.
func findPhotos(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// Skip dot-directories: a photos folder that happens to contain a
			// .git or a .thumbnails is not an invitation to walk it.
			if path != dir && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || mimeTypeOf(path) == "" {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", dir, err)
	}
	return out, nil
}

// relativeTo expresses a path relative to the corpus file's directory, so a
// corpus, its box snapshot and its photos can be copied to another machine
// together and still resolve. It falls back to the path as given when the two
// are on different volumes or a relation cannot be computed -- an absolute
// path that works here beats a relative one that works nowhere.
func relativeTo(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return path
	}
	return filepath.ToSlash(rel)
}
