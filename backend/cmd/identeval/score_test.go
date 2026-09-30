package main

import (
	"math"
	"testing"

	"boxwright/internal/placement"
)

func TestTheCommittedSetLoads(t *testing.T) {
	s, err := loadSet("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.labels.Photos) < 20 || len(s.configs) == 0 {
		t.Fatalf("%d photos, %d configs", len(s.labels.Photos), len(s.configs))
	}
	for id, src := range s.sources {
		if len(src.SHA256) != 64 || src.License == "" || src.Author == "" || src.Page == "" {
			t.Errorf("%s: every photo needs a pinned hash and its attribution", id)
		}
	}
}

func d(name, cat string, qty int) placement.ItemDraft {
	return placement.ItemDraft{Name: name, Category: cat, Quantity: qty}
}

func TestScorePhoto(t *testing.T) {
	yes, cm := true, [2]float64{20, 35}
	want := []expected{
		{Name: "tape measure", Match: []string{"tape measure", "measuring tape"}, Categories: []string{"tools"}, Quantity: [2]int{1, 1}, Must: true},
		{Name: "electrical tape", Match: []string{"tape"}, Categories: []string{"tools", "electronics"}, Quantity: [2]int{1, 1}},
		{Name: "wrenches", Match: []string{"wrench"}, Categories: []string{"tools"}, Quantity: [2]int{6, 9}, Must: true},
		{Name: "drill", Match: []string{"drill"}, Categories: []string{"tools"}, Quantity: [2]int{1, 1}, Must: true, LongestCm: &cm, Fragile: &yes},
	}
	drill := d("Cordless Drill", "Tools", 1)
	drill.DimensionsCm = &placement.Dims{L: 28, W: 22, H: 7}
	got := []placement.ItemDraft{
		d("Measuring tape", "tools", 1),
		d("Electrical tape roll", "electronics", 1),
		d("Combination wrench", "tools", 4),
		d("Combination wrench (large)", "hardware", 3),
		drill,
		d("Sticker", "office", 1),
	}
	s := scorePhoto(want, got)
	byName := map[string]itemScore{}
	for _, it := range s.Items {
		byName[it.Name] = it
	}
	// Specific before general: the measuring tape is not electrical tape.
	if !byName["tape measure"].Found || !byName["electrical tape"].Found {
		t.Errorf("tape matching: %+v", s.Items)
	}
	// Quantities sum across drafts; categories need a majority (a tie passes).
	w := byName["wrenches"]
	if w.Quantity != 7 || !*w.QuantityOK || !*w.CategoryOK {
		t.Errorf("wrenches %+v", w)
	}
	dr := byName["drill"]
	if !*dr.SizeOK || *dr.FragileOK || !*dr.CategoryOK {
		t.Errorf("drill %+v (size in range, fragile wrong, category normalised)", dr)
	}
	if len(s.Extras) != 1 || s.Extras[0] != "Sticker" {
		t.Errorf("extras %v", s.Extras)
	}
}

func TestSummarize(t *testing.T) {
	want := []expected{{Name: "drill", Match: []string{"drill"}, Categories: []string{"tools"}, Quantity: [2]int{1, 1}, Must: true}}
	hit := scorePhoto(want, []placement.ItemDraft{d("drill", "tools", 1)})
	miss := scorePhoto(want, []placement.ItemDraft{d("hammer", "tools", 1)})
	runs := []run{
		{Config: "c", Photo: "p", Rep: 1, LatencyMs: 1000, OutTokens: 100, USD: 0.01, Priced: true, Score: &hit},
		{Config: "c", Photo: "p", Rep: 2, LatencyMs: 3000, OutTokens: 300, USD: 0.03, Priced: true, Score: &miss},
		{Config: "c", Photo: "p", Rep: 3, Error: "status 500"},
	}
	s := summarize("c", runs, 0)
	if s.Recall != 0.5 || s.Errors != 1 || s.Stable != 0 || s.ExtrasPer != 0.5 {
		t.Errorf("%+v", s)
	}
	if math.Abs(s.USDPerPhoto-0.02) > 1e-9 || s.OutTokens != 200 || s.LatencyP50 != 1 {
		t.Errorf("%+v", s)
	}
}
