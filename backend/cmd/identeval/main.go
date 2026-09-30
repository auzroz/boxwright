// Command identeval measures how well each vision model identifies what is in
// a photo, so the choice of AI_MODEL, AI_EFFORT and AI_THINKING is a
// measurement rather than an argument -- and can be measured again, the same
// way, whenever a model is released.
//
// The set is twenty openly licensed Wikimedia Commons photos (testdata/
// sources.json, fetched and hash-checked into a cache, never committed as
// images) with hand-written labels (testdata/labels.json). Each configuration
// in testdata/matrix.json identifies every photo -runs times through the SAME
// provider code the server uses, and each answer is scored for:
//
//   - recall: the labelled must-find items it named;
//
//   - category, quantity, size and the bulky/fragile flags of what it found;
//
//   - extras: things it named that match nothing labelled;
//
//   - stability across repetitions, latency, output tokens and cost.
//
//     make identeval                              # the whole matrix, 3 runs each
//     go run ./cmd/identeval -configs "Sonnet 5.5" -runs 1
//     go run ./cmd/identeval -dry                 # fetch and verify photos only
//     go run ./cmd/identeval -rescore results/x.jsonl   # re-score after fixing labels
//
// Every call is appended to a .jsonl as it finishes, so a run that dies
// partway resumes with -resume and pays for nothing twice. The key is read
// from AI_API_KEY (or ANTHROPIC_API_KEY) and never printed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"boxwright/internal/ai"
	"boxwright/internal/placement"
)

func main() {
	if err := runMain(); err != nil {
		fmt.Fprintln(os.Stderr, "identeval:", err)
		os.Exit(1)
	}
}

type options struct {
	setDir, cacheDir, outDir string
	configs, photos          string
	runs, parallel           int
	maxUSD                   float64
	dry                      bool
	resume, rescore          string
}

func runMain() error {
	var o options
	cache, _ := os.UserCacheDir()
	flag.StringVar(&o.setDir, "set", "cmd/identeval/testdata", "directory holding sources.json, labels.json and matrix.json")
	flag.StringVar(&o.cacheDir, "cache", filepath.Join(cache, "boxwright-identeval"), "where fetched photos are kept")
	flag.StringVar(&o.outDir, "out", "cmd/identeval/results", "where results are written")
	flag.StringVar(&o.configs, "configs", "", "only configs whose label or model contains one of these (comma-separated)")
	flag.StringVar(&o.photos, "photos", "", "only these photo ids (comma-separated)")
	flag.IntVar(&o.runs, "runs", 3, "repetitions of every photo per config")
	flag.IntVar(&o.parallel, "parallel", 4, "calls in flight at once")
	flag.Float64Var(&o.maxUSD, "max-usd", 40, "stop starting new calls once this much has been spent")
	flag.BoolVar(&o.dry, "dry", false, "fetch and verify the photos, print the plan, call no model")
	flag.StringVar(&o.resume, "resume", "", "continue an interrupted .jsonl, skipping calls already in it")
	flag.StringVar(&o.rescore, "rescore", "", "score a .jsonl again against the current labels, calling no model")
	flag.Parse()

	set, err := loadSet(o.setDir)
	if err != nil {
		return err
	}
	if o.rescore != "" {
		runs, err := readRuns(o.rescore)
		if err != nil {
			return err
		}
		return report(set, runs, strings.TrimSuffix(o.rescore, ".jsonl"))
	}

	configs := filterConfigs(set.configs, o.configs)
	photos := filterPhotos(set.labels.Photos, o.photos)
	if len(configs) == 0 || len(photos) == 0 {
		return errors.New("nothing to run: no configs or photos match the filters")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	images := map[string][]byte{}
	for _, p := range photos {
		b, err := fetchPhoto(ctx, o.cacheDir, set.sources[p.ID])
		if err != nil {
			return err
		}
		images[p.ID] = b
	}
	calls := len(configs) * len(photos) * o.runs
	fmt.Printf("%d photos verified; %d configs x %d photos x %d runs = %d calls (spend capped at $%.0f)\n",
		len(photos), len(configs), len(photos), o.runs, calls, o.maxUSD)
	if o.dry {
		for _, c := range configs {
			fmt.Printf("  %-50s %s effort=%q thinking=%q\n", c.Label, c.Model, c.Effort, c.Thinking)
		}
		return nil
	}

	key := os.Getenv("AI_API_KEY")
	if key == "" {
		key = os.Getenv("ANTHROPIC_API_KEY")
	}
	if key == "" && os.Getenv("AI_BASE_URL") == "" {
		return errors.New("set AI_API_KEY (or ANTHROPIC_API_KEY), e.g. `AI_API_KEY=$(op read op://...) make identeval`")
	}
	idents := map[string]ai.Identifier{}
	for _, c := range configs {
		id, err := ai.New("anthropic", os.Getenv("AI_BASE_URL"), key, c.Model, ai.WithEffort(c.Effort), ai.WithThinking(c.Thinking))
		if err != nil {
			return fmt.Errorf("%s: %w", c.Label, err)
		}
		idents[c.Label] = id
	}

	var done map[string]bool
	path := o.resume
	if path != "" {
		prior, err := readRuns(path)
		if err != nil {
			return err
		}
		done = map[string]bool{}
		for _, r := range prior {
			if r.Error == "" {
				done[runKey(r.Config, r.Photo, r.Rep)] = true
			}
		}
		fmt.Printf("resuming %s: %d calls already done\n", path, len(done))
	} else {
		if err := os.MkdirAll(o.outDir, 0o755); err != nil {
			return err
		}
		path = filepath.Join(o.outDir, "identeval-"+time.Now().UTC().Format("20060102-150405")+".jsonl")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	vocab := make([]placement.Category, 0, len(set.labels.Categories))
	for _, k := range set.labels.Categories {
		vocab = append(vocab, placement.Category{Key: k, Label: strings.ToUpper(k[:1]) + strings.ReplaceAll(k[1:], "-", " ")})
	}

	type job struct {
		cfg   config
		photo labelledPhoto
		rep   int
	}
	jobs := make(chan job)
	var mu sync.Mutex
	spent, finished := 0.0, 0
	var wg sync.WaitGroup
	for w := 0; w < o.parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				r := call(ctx, idents[j.cfg.Label], j.cfg, j.photo, j.rep, images[j.photo.ID], vocab)
				mu.Lock()
				spent += r.USD
				finished++
				line, _ := json.Marshal(r)
				f.Write(append(line, '\n'))
				status := fmt.Sprintf("%d items", len(r.Drafts))
				if r.Error != "" {
					status = "ERROR " + firstLine(r.Error)
				}
				fmt.Printf("[%d/%d $%.2f] %-45s %-18s #%d %5.1fs %s\n", finished, calls, spent,
					j.cfg.Label, j.photo.ID, j.rep, float64(r.LatencyMs)/1000, status)
				mu.Unlock()
			}
		}()
	}
	// Reps outermost and configs innermost, so every config sees the same
	// time of day and the same rate limits rather than one running at 3 a.m.
feed:
	for rep := 1; rep <= o.runs; rep++ {
		for _, p := range photos {
			for _, c := range configs {
				if done[runKey(c.Label, p.ID, rep)] {
					continue
				}
				mu.Lock()
				over := spent >= o.maxUSD
				mu.Unlock()
				if over {
					fmt.Printf("spend reached $%.2f; stopping (continue with -resume %s -max-usd N)\n", spent, path)
					break feed
				}
				select {
				case jobs <- job{c, p, rep}:
				case <-ctx.Done():
					break feed
				}
			}
		}
	}
	close(jobs)
	wg.Wait()
	f.Close()

	runs, err := readRuns(path)
	if err != nil {
		return err
	}
	return report(set, runs, strings.TrimSuffix(path, ".jsonl"))
}

// call identifies one photo, retrying briefly when the API says to back off.
func call(ctx context.Context, id ai.Identifier, c config, p labelledPhoto, rep int, image []byte, vocab []placement.Category) run {
	r := run{Config: c.Label, Photo: p.ID, Rep: rep}
	for attempt := 0; ; attempt++ {
		var u ai.Usage
		start := time.Now()
		drafts, err := id.Identify(ai.WithUsage(ctx, &u), image, "image/jpeg", vocab)
		r.LatencyMs = time.Since(start).Milliseconds()
		r.InTokens, r.OutTokens, r.USD, r.Priced, r.Retried = u.InputTokens, u.OutputTokens, u.USD, u.PriceKnown, u.RetriedWithoutThinking
		if err != nil && attempt < 4 && transient(err) && ctx.Err() == nil {
			time.Sleep(time.Duration(5<<attempt) * time.Second)
			continue
		}
		if err != nil {
			r.Error = err.Error()
			return r
		}
		r.Drafts = drafts
		return r
	}
}

func transient(err error) bool {
	s := err.Error()
	for _, t := range []string{"status 429", "status 500", "status 502", "status 503", "status 529", "overloaded", "timeout", "EOF", "connection reset"} {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func runKey(cfg, photo string, rep int) string { return fmt.Sprintf("%s\x00%s\x00%d", cfg, photo, rep) }

func readRuns(path string) ([]run, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var runs []run
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r run
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		runs = append(runs, r)
	}
	return runs, sc.Err()
}

func filterConfigs(all []config, filter string) []config {
	if filter == "" {
		return all
	}
	var out []config
	for _, c := range all {
		for _, f := range strings.Split(filter, ",") {
			f = strings.TrimSpace(f)
			if f != "" && (strings.Contains(c.Label, f) || strings.Contains(c.Model, f)) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func filterPhotos(all []labelledPhoto, filter string) []labelledPhoto {
	if filter == "" {
		return all
	}
	want := map[string]bool{}
	for _, id := range strings.Split(filter, ",") {
		want[strings.TrimSpace(id)] = true
	}
	var out []labelledPhoto
	for _, p := range all {
		if want[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
