package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// report scores every run against the current labels and writes
// <base>.md (for people) and <base>.summary.json (for comparing runs).
func report(set evalSet, runs []run, base string) error {
	labelsFor := map[string]labelledPhoto{}
	sized := 0
	for _, p := range set.labels.Photos {
		labelsFor[p.ID] = p
	}
	byConfig := map[string][]run{}
	var order []string
	for i := range runs {
		r := &runs[i]
		p, ok := labelsFor[r.Photo]
		if !ok {
			continue
		}
		if r.Error == "" {
			s := scorePhoto(p.Items, r.Drafts)
			r.Score = &s
		}
		if _, seen := byConfig[r.Config]; !seen {
			order = append(order, r.Config)
		}
		byConfig[r.Config] = append(byConfig[r.Config], *r)
	}
	// Found items with a labelled size, per run: the denominator for how
	// often a model offers a size at all.
	for _, p := range set.labels.Photos {
		for _, it := range p.Items {
			if it.LongestCm != nil {
				sized++
			}
		}
	}

	// Keep the matrix's order, which is the order a reader thinks in.
	rank := map[string]int{}
	for i, c := range set.configs {
		rank[c.Label] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return rank[order[i]] < rank[order[j]] })

	var sums []summary
	for _, c := range order {
		rs := byConfig[c]
		reps := map[int]bool{}
		for _, r := range rs {
			reps[r.Rep] = true
		}
		sums = append(sums, summarize(c, rs, sized*len(reps)))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Identification eval, %s\n\n", time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(&b, "%d calls over %d photos (labels v%d). Recall is must-find items named; the next columns are of the items found. Latency is wall time per photo, cost is list price per photo.\n\n",
		len(runs), len(set.labels.Photos), set.labels.Version)
	b.WriteString("| Config | Recall | Category | Count | Size | Size given | Flags | Extras/photo | Stable | p50 s | p90 s | Out tok | $/photo | Errors |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range sums {
		cost := "?"
		if s.Priced {
			cost = fmt.Sprintf("%.4f", s.USDPerPhoto)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %.1f | %s | %.1f | %.1f | %.0f | %s | %d/%d |\n",
			s.Config, pct(s.Recall), pct(s.Category), pct(s.Quantity), pct(s.Size), pct(s.SizeGiven), pct(s.Flags),
			s.ExtrasPer, pct(s.Stable), s.LatencyP50, s.LatencyP90, s.OutTokens, cost, s.Errors, s.Runs)
	}

	// Per photo, per config: mean must-recall, so a weakness is visible where
	// it lives rather than averaged away.
	b.WriteString("\n## Must-find recall by photo\n\n| Photo | Kind |")
	for _, c := range order {
		fmt.Fprintf(&b, " %s |", c)
	}
	b.WriteString("\n|---|---|")
	for range order {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, p := range set.labels.Photos {
		fmt.Fprintf(&b, "| %s | %s |", p.ID, p.Kind)
		for _, c := range order {
			found, total := 0, 0
			for _, r := range byConfig[c] {
				if r.Photo != p.ID || r.Score == nil {
					continue
				}
				for _, it := range r.Score.Items {
					if it.Must {
						total++
						if it.Found {
							found++
						}
					}
				}
			}
			fmt.Fprintf(&b, " %s |", pct(ratio(found, total)))
		}
		b.WriteString("\n")
	}

	// Errors, verbatim, because a config that fails is not a config that is
	// merely worse.
	var errs []string
	for _, r := range runs {
		if r.Error != "" {
			errs = append(errs, fmt.Sprintf("- %s / %s #%d: %s", r.Config, r.Photo, r.Rep, firstLine(r.Error)))
		}
	}
	if len(errs) > 0 {
		b.WriteString("\n## Errors\n\n" + strings.Join(errs, "\n") + "\n")
	}

	if err := os.WriteFile(base+".md", []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := writeJSON(base+".summary.json", sums); err != nil {
		return err
	}
	fmt.Println()
	fmt.Print(b.String())
	fmt.Printf("\nwrote %s.md and %s.summary.json\n", base, base)
	return nil
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := jsonEncoder(f)
	return enc.Encode(v)
}

func jsonEncoder(f *os.File) *json.Encoder {
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc
}
