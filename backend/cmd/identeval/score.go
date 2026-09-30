package main

import (
	"math"
	"sort"
	"strings"

	"boxwright/internal/placement"
)

// itemScore is how one expected item fared in one answer.
type itemScore struct {
	Name     string `json:"name"`
	Must     bool   `json:"must"`
	Found    bool   `json:"found"`
	Quantity int    `json:"quantity"`
	// nil: not checkable (not found, or nothing to compare against).
	QuantityOK *bool `json:"quantityOk,omitempty"`
	CategoryOK *bool `json:"categoryOk,omitempty"`
	SizeOK     *bool `json:"sizeOk,omitempty"`
	BulkyOK    *bool `json:"bulkyOk,omitempty"`
	FragileOK  *bool `json:"fragileOk,omitempty"`
}

// photoScore is one answer for one photo, scored against its labels.
type photoScore struct {
	Items []itemScore `json:"items"`
	// Extras are returned items that match nothing labelled: invented, or
	// real but unlabelled (the dense scenes list their likely extras as
	// optional items so these stay meaningful).
	Extras []string `json:"extras"`
	// Stub: the answer contains a placeholder instead of an identification
	// ("placeholder", "x"). Measured on the Opus family, which sometimes
	// returns one such item for a crowded photo in about 90 output tokens.
	Stub bool `json:"stub,omitempty"`
}

// stubNames are what a model writes when it gives up on the list.
var stubNames = map[string]bool{"placeholder": true, "x": true, "item": true, "object": true, "unknown": true, "n/a": true, "": true}

func isStub(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return stubNames[n] || len([]rune(n)) <= 1
}

// scorePhoto matches each returned draft to the first expected item whose
// keywords appear in its name, in label order -- so labels list the specific
// before the general ("tape measure" before electrical "tape").
func scorePhoto(want []expected, got []placement.ItemDraft) photoScore {
	matched := make([][]placement.ItemDraft, len(want))
	var ps photoScore
	for _, d := range got {
		if isStub(d.Name) {
			ps.Stub = true
		}
		name := strings.ToLower(d.Name)
		hit := -1
		for i, e := range want {
			if matchesAny(name, e.Match) {
				hit = i
				break
			}
		}
		if hit < 0 {
			ps.Extras = append(ps.Extras, d.Name)
			continue
		}
		matched[hit] = append(matched[hit], d)
	}
	for i, e := range want {
		ds := matched[i]
		s := itemScore{Name: e.Name, Must: e.Must, Found: len(ds) > 0}
		if s.Found {
			for _, d := range ds {
				s.Quantity += max(1, d.Quantity)
			}
			s.QuantityOK = boolp(s.Quantity >= e.Quantity[0] && s.Quantity <= e.Quantity[1])
			// Majority of the matched drafts in an accepted category.
			ok := 0
			for _, d := range ds {
				if matchesExactly(placement.NormalizeCategory(d.Category), e.Categories) {
					ok++
				}
			}
			s.CategoryOK = boolp(ok*2 > len(ds) || (ok > 0 && ok*2 == len(ds)))
			if e.LongestCm != nil {
				for _, d := range ds {
					if d.DimensionsCm != nil {
						l := longest(*d.DimensionsCm)
						s.SizeOK = boolp(l >= e.LongestCm[0] && l <= e.LongestCm[1])
						break
					}
				}
			}
			if e.Bulky != nil {
				s.BulkyOK = boolp(ds[0].Bulky == *e.Bulky)
			}
			if e.Fragile != nil {
				s.FragileOK = boolp(ds[0].Fragile == *e.Fragile)
			}
		}
		ps.Items = append(ps.Items, s)
	}
	return ps
}

func matchesAny(name string, keywords []string) bool {
	for _, k := range keywords {
		if strings.Contains(name, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

func matchesExactly(key string, accepted []string) bool {
	for _, a := range accepted {
		if key == a {
			return true
		}
	}
	return false
}

func longest(d placement.Dims) float64 { return math.Max(d.L, math.Max(d.W, d.H)) }

func boolp(b bool) *bool { return &b }

// run is one call: one config, one photo, one repetition.
type run struct {
	Config    string                `json:"config"`
	Photo     string                `json:"photo"`
	Rep       int                   `json:"rep"`
	LatencyMs int64                 `json:"latencyMs"`
	InTokens  int                   `json:"inTokens"`
	OutTokens int                   `json:"outTokens"`
	USD       float64               `json:"usd"`
	Priced    bool                  `json:"priced"`
	Retried   bool                  `json:"retriedWithoutThinking,omitempty"`
	Error     string                `json:"error,omitempty"`
	Drafts    []placement.ItemDraft `json:"drafts"`
	Score     *photoScore           `json:"score,omitempty"`
}

// summary is one config across every photo and repetition.
type summary struct {
	Config      string  `json:"config"`
	Runs        int     `json:"runs"`
	Errors      int     `json:"errors"`
	Recall      float64 `json:"recall"`    // must items found
	Category    float64 `json:"category"`  // of found items, category acceptable
	Quantity    float64 `json:"quantity"`  // of found items, count in range
	Size        float64 `json:"size"`      // of found items with a known size, longest side in range
	SizeGiven   float64 `json:"sizeGiven"` // of those, how often a size was returned at all
	Flags       float64 `json:"flags"`     // bulky and fragile, where labelled
	ExtrasPer   float64 `json:"extrasPerPhoto"`
	Stubs       int     `json:"stubs"`  // answers that were a placeholder, not a list
	Stable      float64 `json:"stable"` // photos whose must-item recall was identical across reps
	LatencyP50  float64 `json:"latencyP50s"`
	LatencyP90  float64 `json:"latencyP90s"`
	OutTokens   float64 `json:"meanOutTokens"`
	USDPerPhoto float64 `json:"usdPerPhoto"`
	Priced      bool    `json:"priced"`
}

func ratio(n, d int) float64 {
	if d == 0 {
		return math.NaN()
	}
	return float64(n) / float64(d)
}

func summarize(cfg string, runs []run, sizedItems int) summary {
	s := summary{Config: cfg, Runs: len(runs), Priced: true}
	var mustFound, mustTotal, catOK, catN, qOK, qN, szOK, szN, flOK, flN, extras, scored int
	var lat []float64
	var out, usd float64
	perPhoto := map[string]map[int]bool{} // photo -> distinct must-found counts
	for _, r := range runs {
		if r.Error != "" || r.Score == nil {
			s.Errors++
			continue
		}
		scored++
		lat = append(lat, float64(r.LatencyMs)/1000)
		out += float64(r.OutTokens)
		usd += r.USD
		s.Priced = s.Priced && r.Priced
		found := 0
		for _, it := range r.Score.Items {
			if it.Must {
				mustTotal++
				if it.Found {
					mustFound++
					found++
				}
			}
			count := func(p *bool, ok, n *int) {
				if p != nil {
					*n++
					if *p {
						*ok++
					}
				}
			}
			count(it.CategoryOK, &catOK, &catN)
			count(it.QuantityOK, &qOK, &qN)
			count(it.SizeOK, &szOK, &szN)
			count(it.BulkyOK, &flOK, &flN)
			count(it.FragileOK, &flOK, &flN)
		}
		extras += len(r.Score.Extras)
		if r.Score.Stub {
			s.Stubs++
		}
		if perPhoto[r.Photo] == nil {
			perPhoto[r.Photo] = map[int]bool{}
		}
		perPhoto[r.Photo][found] = true
	}
	s.Recall = ratio(mustFound, mustTotal)
	s.Category = ratio(catOK, catN)
	s.Quantity = ratio(qOK, qN)
	s.Size = ratio(szOK, szN)
	s.SizeGiven = ratio(szN, sizedItems)
	s.Flags = ratio(flOK, flN)
	s.ExtrasPer = ratio(extras, scored)
	stable := 0
	for _, counts := range perPhoto {
		if len(counts) == 1 {
			stable++
		}
	}
	s.Stable = ratio(stable, len(perPhoto))
	sort.Float64s(lat)
	s.LatencyP50 = percentile(lat, 0.5)
	s.LatencyP90 = percentile(lat, 0.9)
	s.OutTokens = out / math.Max(1, float64(scored))
	s.USDPerPhoto = usd / math.Max(1, float64(scored))
	return s
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}
