package placement

import "testing"

// A canonical key must be a FIXED POINT of NormalizeCategory. Every key is
// both what the vision model is asked to emit and what the box index produces
// from a Homebox tag, so a key the normalizer rewrites would match nothing on
// either side while looking perfectly correct in the table.
func TestCanonicalKeysSurviveNormalizationUnchanged(t *testing.T) {
	for _, c := range CanonicalCategories() {
		if got := NormalizeCategory(c.Key); got != c.Key {
			t.Errorf("category %q normalizes to %q; canonical keys must be fixed points", c.Key, got)
		}
		if c.Label == "" {
			t.Errorf("category %q has no human label", c.Key)
		}
	}
}

func TestCanonicalKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range CanonicalCategories() {
		if seen[c.Key] {
			t.Errorf("duplicate canonical category %q", c.Key)
		}
		seen[c.Key] = true
	}
	if len(seen) != len(canonicalIndex) {
		t.Errorf("derived index has %d entries for %d unique keys", len(canonicalIndex), len(seen))
	}
}

// The frequently-accessed set used to be a second hand-typed list of four
// strings sitting next to the scoring code, where a typo disabled access
// scoring silently. It is now derived, and this pins the derivation in both
// directions: the flag in the table is the only thing that decides membership.
func TestFrequentlyAccessedIsDerivedFromTheTable(t *testing.T) {
	for _, c := range CanonicalCategories() {
		if got := IsFrequentlyAccessed(c.Key); got != c.FrequentlyAccessed {
			t.Errorf("IsFrequentlyAccessed(%q) = %v, table says %v", c.Key, got, c.FrequentlyAccessed)
		}
	}
	for key := range frequentlyAccessed {
		if !IsCanonicalCategory(key) {
			t.Errorf("frequently-accessed set contains non-canonical %q", key)
		}
	}
	// The four the engine has always rewarded with front-zone placement.
	// Listed explicitly so quietly flipping a flag in the table is a failing
	// test rather than a silent behavior change to a shipped heuristic.
	want := map[string]bool{"tools": true, "seasonal-holiday": true, "sports-outdoor": true, "documents": true}
	if len(frequentlyAccessed) != len(want) {
		t.Fatalf("frequently-accessed set = %v, want exactly %v", frequentlyAccessed, want)
	}
	for key := range want {
		if !IsFrequentlyAccessed(key) {
			t.Errorf("%q should be frequently accessed", key)
		}
	}
	// It normalizes, so the accessor works on raw model output too.
	if !IsFrequentlyAccessed("Power Tools") {
		t.Error(`IsFrequentlyAccessed("Power Tools") should fold to tools`)
	}
}

func TestNormalizeCategoryFolding(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty is the only coercion", "", "other"},
		{"whitespace only is empty", "   \t ", "other"},
		{"case folded", "Tools", "tools"},
		{"spaces become hyphens", "Seasonal Holiday", "seasonal-holiday"},
		{"repeated whitespace collapses", "Seasonal  \t Holiday", "seasonal-holiday"},
		{"surrounding whitespace trimmed", "  Documents  ", "documents"},
		{"canonical key untouched", "books-media", "books-media"},
		{"user tag preserved", "Micro Masterpieces", "micro-masterpieces"},
		// Aliases must NOT apply here: this value is written to Homebox as a
		// tag, and rewriting it creates a near-duplicate of the user's own.
		{"alias NOT applied on the write path", "power tools", "power-tools"},
		{"misc NOT coerced on the write path", "Miscellaneous", "miscellaneous"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeCategory(tc.in); got != tc.want {
				t.Fatalf("NormalizeCategory(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestMatchCategoryAppliesAliases(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"alias folds to canonical", "power tools", "tools"},
		{"singular alias folds", "Toy", "toys-games"},
		{"misc folds to other", "Miscellaneous", "other"},
		{"canonical key untouched", "books-media", "books-media"},
		{"unknown user tag preserved", "Micro Masterpieces", "micro-masterpieces"},
		{"empty becomes other", "", "other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchCategory(tc.in); got != tc.want {
				t.Fatalf("MatchCategory(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The reason the two functions exist separately. NormalizeCategory feeds
// Homebox's ResolveTag, which matches existing tags case-insensitively. If an
// alias fired there, a user who types "books" against their own "Books" tag
// would get a second tag called "Books Media" beside it, and every book they
// catalogue afterwards would be invisible under their existing filter.
func TestWritePathNeverRewritesAUsersOwnVocabulary(t *testing.T) {
	for _, typed := range []string{"Books", "Toys", "Sports", "Clothes", "Misc"} {
		written := NormalizeCategory(typed)
		if written != foldCategoryKey(typed) {
			t.Errorf("NormalizeCategory(%q) = %q, which is not just a shape fold; "+
				"this string is written to Homebox as a tag", typed, written)
		}
		// The matcher may still map it somewhere canonical -- that is its job.
		if MatchCategory(typed) == written && categoryAliases[written] != "" {
			t.Errorf("MatchCategory(%q) failed to apply its alias", typed)
		}
	}
}

// An alias that is itself a canonical key would shadow that key, and one
// pointing at a non-canonical target would create a phantom category nothing
// else in the system knows about.
func TestAliasesPointAtCanonicalKeysAndDoNotShadowThem(t *testing.T) {
	for alias, target := range categoryAliases {
		if _, ok := canonicalIndex[alias]; ok {
			t.Errorf("alias %q shadows the canonical key of the same name", alias)
		}
		if _, ok := canonicalIndex[target]; !ok {
			t.Errorf("alias %q resolves to non-canonical %q", alias, target)
		}
		// The lookup happens on an already-folded string, so an alias key
		// carrying a capital or a space is dead code that never matches.
		if alias != foldCategoryKey(alias) {
			t.Errorf("alias key %q is not in folded form; it can never be looked up", alias)
		}
	}
}

// THE DECISION, PINNED. A category we do not recognize is preserved, never
// coerced to "other".
//
// NormalizeCategory is shared by both sides of the affinity match: the item
// draft AND the box index, which builds Box.Categories by folding Homebox tag
// names. Coercing the unrecognized would therefore not fence the model in, it
// would flatten every user-invented tag to "other" on both sides -- the box
// holding 132 pieces tagged "micro-masterpieces" would index as an "other"
// box, and the single strongest affinity signal in this inventory would be
// gone. This test states that intent so a future "tighten up Normalize" change
// has to argue with it rather than silently win.
func TestUnknownCategoryIsPreservedNotCoercedToOther(t *testing.T) {
	d := ItemDraft{Name: "tiny painting", Category: "Micro Masterpieces"}
	d.Normalize()
	if d.Category != "micro-masterpieces" {
		t.Fatalf("Category = %q, want %q: a user's own Homebox tag is a legitimate category", d.Category, "micro-masterpieces")
	}
	if IsCanonicalCategory(d.Category) {
		t.Fatal("fixture is wrong: micro-masterpieces should not be canonical")
	}
}

// The consequence of that decision, end to end: an item categorized with a
// user's own tag lands in the box that already holds that collection. Under
// coercion it would score zero there and be filed into the misc box instead,
// which the second half asserts is what "other" genuinely does.
func TestUserInventedTagKeepsItsAffinity(t *testing.T) {
	inventory := []Box{
		{
			ID: "art", Name: "Art 1", Access: "normal",
			CapacityL: litres(40), FillPct: pctOf(20, 40), FillSource: FillObserved,
			Categories: map[string]int{"micro-masterpieces": 10},
		},
		{
			ID: "misc", Name: "Misc 1", Access: "easy",
			CapacityL: litres(40), FillPct: pctOf(10, 40), FillSource: FillObserved,
			Categories: map[string]int{"other": 5},
		},
	}

	rec := Recommend(ItemDraft{Name: "tiny painting", Category: "Micro Masterpieces", SizeBucket: "S"}, inventory, DefaultWeights())
	if len(rec.Candidates) == 0 || rec.Candidates[0].Box.ID != "art" {
		t.Fatalf("user-tagged item should go to the box holding that tag, got %+v", rec.Candidates)
	}
	if rec.NewContainer != nil {
		t.Error("a box with real affinity exists; should not suggest buying a container")
	}

	// Same inventory, genuinely uncategorized item: "other" is a real
	// category and matches the misc box. That is precisely why coercing
	// unknown categories to it would be destructive rather than neutral.
	rec = Recommend(ItemDraft{Name: "thing", Category: "", SizeBucket: "S"}, inventory, DefaultWeights())
	if len(rec.Candidates) == 0 || rec.Candidates[0].Box.ID != "misc" {
		t.Fatalf("uncategorized item should fall to the misc box, got %+v", rec.Candidates)
	}
}

// CategoryMatch is weight 5.0 and fires on exact key equality, so before the
// alias table a draft categorized "power tools" earned no affinity from the
// tools tote at all and the engine told the user to buy another container.
func TestAliasedCategoryEarnsAffinity(t *testing.T) {
	tools := []Box{{
		ID: "b1", Name: "Tools 1", Access: "easy",
		CapacityL: litres(8), FillPct: pctOf(4, 8), FillSource: FillObserved,
		Categories: map[string]int{"tools": 6},
	}}
	for _, spelling := range []string{"tools", "Tools", "power tools", "Hand Tools"} {
		rec := Recommend(ItemDraft{Name: "cordless drill", Category: spelling, SizeBucket: "M"}, tools, DefaultWeights())
		if len(rec.Candidates) == 0 {
			t.Fatalf("%q: no candidates", spelling)
		}
		if rec.NewContainer != nil {
			t.Errorf("%q: suggested a new container despite an existing tools tote", spelling)
		}
		if got := rec.Candidates[0].Score; got < DefaultWeights().MinViableScore {
			t.Errorf("%q: score %v below MinViableScore; category affinity did not fire", spelling, got)
		}
	}
}

func TestIsCanonicalCategory(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"tools", true},
		{"Tools", true},
		{"power tools", true}, // folds through the alias table
		{"", true},            // becomes "other", which is canonical
		{"books-media", true},
		{"micro-masterpieces", false},
		{"power-drill", false},
	}
	for _, tc := range tests {
		if got := IsCanonicalCategory(tc.in); got != tc.want {
			t.Errorf("IsCanonicalCategory(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCategoryLabel(t *testing.T) {
	if got := CategoryLabel("books-media"); got != "Books & Media" {
		t.Errorf("CategoryLabel(books-media) = %q", got)
	}
	if got := CategoryLabel("power tools"); got != "Tools" {
		t.Errorf("CategoryLabel(power tools) = %q", got)
	}
	// A user's own tag still has to render as something.
	if got := CategoryLabel("Micro Masterpieces"); got != "Micro Masterpieces" {
		t.Errorf("CategoryLabel(Micro Masterpieces) = %q", got)
	}
	if got := CategoryLabel(""); got != "Other" {
		t.Errorf("CategoryLabel(empty) = %q", got)
	}
}

// internal/ai builds its identify prompt from CategoryVocabulary() instead of
// restating the list. That handoff crosses a package boundary the ai tests
// cannot check from this side, so pin the exact rendering here: it is the
// string that reaches the vision model, and both its content and its order are
// part of the prompt's behavior.
func TestCategoryVocabularyRendersTheShippedPromptList(t *testing.T) {
	const want = "tools, electronics, kitchen, clothing, books-media, decor, " +
		"seasonal-holiday, sports-outdoor, documents, toys-games, other"
	if got := CategoryVocabulary(); got != want {
		t.Fatalf("CategoryVocabulary() = %q\nwant %q", got, want)
	}
}

// The vocabulary is the source of truth for the prompt; a caller must not be
// able to reach into the backing array and change what the model is asked for.
func TestCanonicalCategoriesReturnsACopy(t *testing.T) {
	got := CanonicalCategories()
	got[0].Key = "vandalized"
	got[0].FrequentlyAccessed = false
	if again := CanonicalCategories(); again[0].Key == "vandalized" {
		t.Fatal("CanonicalCategories() shares its backing array with the package vocabulary")
	}
	if !IsFrequentlyAccessed("tools") {
		t.Fatal("mutating the returned slice changed the derived frequently-accessed set")
	}
	if keys := CategoryKeys(); keys[0] != "tools" {
		t.Fatalf("CategoryKeys()[0] = %q after caller mutation", keys[0])
	}
}
