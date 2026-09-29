package placement

import "strings"

// Category is one entry in the canonical category vocabulary.
//
// The vocabulary used to live as prose inside the vision prompt in
// internal/ai, which meant nothing could validate against it, the UI could not
// render it, and the frequently-accessed set was a second hand-typed copy of
// four of these strings -- a typo there silently disabled access scoring with
// no test failing. It is data here so there is exactly one copy.
type Category struct {
	// Key is the canonical form: lowercase, hyphenated, and already in the
	// shape NormalizeCategory produces.
	Key string `json:"key"`
	// Label is the human form for pickers and headings.
	Label string `json:"label"`
	// FrequentlyAccessed marks categories you come back to often, which the
	// engine rewards for easy-to-reach placement and prefers for a new
	// container's access zone.
	FrequentlyAccessed bool `json:"frequentlyAccessed"`
}

// OtherCategory is the fallback key for an item that carries no category at
// all. It is a real member of the vocabulary, not a sentinel.
const OtherCategory = "other"

// canonicalCategories is the ordered vocabulary. Order is meaningful: it is
// the order the vision prompt lists and a picker should render, running from
// the most common storage-unit contents to the catch-all.
//
// Adding an entry here adds it to the prompt, the canonical lookup and the
// frequently-accessed set at once. That is the point.
var canonicalCategories = []Category{
	{Key: "tools", Label: "Tools", FrequentlyAccessed: true},
	{Key: "electronics", Label: "Electronics"},
	{Key: "kitchen", Label: "Kitchen"},
	{Key: "clothing", Label: "Clothing"},
	{Key: "books-media", Label: "Books & Media"},
	{Key: "decor", Label: "Decor"},
	{Key: "seasonal-holiday", Label: "Seasonal & Holiday", FrequentlyAccessed: true},
	{Key: "sports-outdoor", Label: "Sports & Outdoor", FrequentlyAccessed: true},
	{Key: "documents", Label: "Documents", FrequentlyAccessed: true},
	{Key: "toys-games", Label: "Toys & Games"},
	{Key: OtherCategory, Label: "Other"},
}

// canonicalIndex and frequentlyAccessed are DERIVED from canonicalCategories
// so the three can never disagree.
var (
	canonicalIndex     = map[string]Category{}
	frequentlyAccessed = map[string]bool{}
)

func init() {
	for _, c := range canonicalCategories {
		canonicalIndex[c.Key] = c
		if c.FrequentlyAccessed {
			frequentlyAccessed[c.Key] = true
		}
	}
}

// categoryAliases fold spellings a vision model or a human tagger plausibly
// produces onto the canonical key, so they earn affinity instead of silently
// scoring zero. CategoryMatch is weight 5.0, the dominant signal, and it fires
// on exact key equality: without this, a draft categorized "power tools" never
// matches the box the tools are already in, and the user is told to buy a
// container forever.
//
// Deliberately conservative. An alias is only listed where the two names
// cannot plausibly mean different things in a storage unit, because this map
// also folds Homebox TAG names (the box index builds its categories by running
// tag names through NormalizeCategory), so an over-eager alias would merge two
// groups a user deliberately keeps apart.
var categoryAliases = map[string]string{
	"tool":           "tools",
	"power-tools":    "tools",
	"hand-tools":     "tools",
	"electronic":     "electronics",
	"kitchenware":    "kitchen",
	"cookware":       "kitchen",
	"clothes":        "clothing",
	"apparel":        "clothing",
	"book":           "books-media",
	"books":          "books-media",
	"home-decor":     "decor",
	"holiday":        "seasonal-holiday",
	"christmas":      "seasonal-holiday",
	"sports":         "sports-outdoor",
	"sporting-goods": "sports-outdoor",
	"document":       "documents",
	"paperwork":      "documents",
	"toy":            "toys-games",
	"toys":           "toys-games",
	"misc":           OtherCategory,
	"miscellaneous":  OtherCategory,
	"uncategorized":  OtherCategory,
	"unknown":        OtherCategory,
	"n/a":            OtherCategory,
	"none":           OtherCategory,
}

// NormalizeCategory folds a category or tag name into the form used as a map
// key: lowercased, runs of whitespace collapsed to single hyphens, then any
// known alias applied. It is what lets a Homebox tag named "Seasonal Holiday"
// match a draft category of "seasonal-holiday".
//
// It deliberately does NOT coerce an unrecognized category to "other", and
// that is the load-bearing decision in this file.
//
// The tempting argument for coercion is that it would fence the vision model
// in: a hallucinated category could never reach the engine. The argument
// against is that this same function is what the box index runs over Homebox
// TAG names to build Box.Categories. The category space is therefore not the
// canonical eleven -- it is the canonical eleven UNION whatever tags this user
// actually invented. Coercing the unrecognized would flatten every one of
// those user tags to "other" on BOTH sides: the box holding 132 pieces tagged
// "micro-masterpieces" would index as an "other" box, the item drafted from
// that same tag would arrive as "other", and every distinct collection in the
// inventory would collapse into one indistinguishable misc pile. That is not a
// guardrail, it is the destruction of the strongest affinity signal we have.
//
// The hallucination case does not actually need coercion either. An
// unrecognized category simply matches no box, so it scores no affinity and
// falls through to the empty-box bonus or the new-container suggestion -- the
// correct outcome for a genuinely novel thing, and a visible one for a
// hallucination. Coercion would instead file the hallucination confidently
// into the misc box, which is strictly worse.
//
// So canonicity is ADVISORY, never a gate. The canonical list steers the model
// at generation time via the prompt (cheap, lossless, before any data exists)
// and labels the UI; it never rewrites data on the way in. Only the empty
// string becomes "other", because empty genuinely carries no information.
//
// Aliases are deliberately NOT applied here. This value is written: the API
// layer feeds it to Homebox's ResolveTag, so folding "books" to "books-media"
// would stop matching a user's existing "Books" tag and create a near-
// duplicate "Books Media" beside it -- Boxwright writing its own vocabulary
// into the sole system of record, on a word the user typed by hand. Aliases
// belong at COMPARISON time; see MatchCategory.
func NormalizeCategory(s string) string {
	s = foldCategoryKey(s)
	if s == "" {
		return OtherCategory
	}
	return s
}

// MatchCategory folds a name for COMPARISON, applying the alias table on top
// of the shape fold. Both sides of an affinity check must go through it or the
// aliases do nothing: an item categorised "books" only meets a box indexed
// from the tag "Books & Media" if each is reduced to the same key.
//
// Nothing this function returns is ever written to Homebox. That separation is
// the whole point -- it lets aliases improve matching without editing the
// user's vocabulary.
func MatchCategory(s string) string {
	s = NormalizeCategory(s)
	if canonical, ok := categoryAliases[s]; ok {
		return canonical
	}
	return s
}

// foldCategoryKey applies the shape half of normalization without the alias
// lookup: lowercase, whitespace runs collapsed to single hyphens. Split out
// because the alias table's own keys must already be in this form to ever be
// looked up, and a test asserts exactly that -- which it cannot do through
// NormalizeCategory, since that would resolve the alias it is checking.
//
// Fields rather than ReplaceAll so a tag typed with a double space or a tab
// folds to the same key as its single-spaced twin.
func foldCategoryKey(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), "-")
}

// CanonicalCategories returns the ordered vocabulary. The returned slice is a
// copy: it is the single source of truth for the vision prompt and the UI, and
// a caller that appended to a shared backing array would silently change what
// the model is asked for.
func CanonicalCategories() []Category {
	out := make([]Category, len(canonicalCategories))
	copy(out, canonicalCategories)
	return out
}

// CategoryKeys returns just the canonical keys, in vocabulary order.
func CategoryKeys() []string {
	out := make([]string, 0, len(canonicalCategories))
	for _, c := range canonicalCategories {
		out = append(out, c.Key)
	}
	return out
}

// VocabularyOf renders any category list as the comma-separated form a prompt
// embeds, e.g. "tools, electronics, ... , other".
//
// It takes the list rather than reading the canonical one because the
// vocabulary the model is shown is NOT the canonical one: it is whatever the
// user's Homebox tags say, with the seeds filling the gaps. Eleven of this
// user's thirteen tags fall outside anything we ship, so a prompt built from
// the constant list would be asking the model to answer in a language the
// inventory does not speak.
func VocabularyOf(cats []Category) string {
	keys := make([]string, 0, len(cats))
	for _, c := range cats {
		if c.Key != "" {
			keys = append(keys, c.Key)
		}
	}
	return strings.Join(keys, ", ")
}

// CategoryVocabulary renders the canonical seed keys. It is the fallback for
// callers with no live vocabulary to hand -- see VocabularyOf.
func CategoryVocabulary() string { return VocabularyOf(canonicalCategories) }

// IsCanonicalCategory reports whether a category is part of the shipped
// vocabulary. It normalizes first, so "Tools" and "power tools" both report
// true. A false result is NOT a rejection -- see NormalizeCategory for why a
// user's own tag is a legitimate category -- it only means the string is not
// one this build ships a label and prompt entry for.
func IsCanonicalCategory(s string) bool {
	_, ok := canonicalIndex[MatchCategory(s)]
	return ok
}

// CategoryLabel returns the human label for a category, falling back to the
// key itself for a user-invented one so a UI always has something to render.
func CategoryLabel(s string) string {
	key := MatchCategory(s)
	if c, ok := canonicalIndex[key]; ok {
		return c.Label
	}
	return titleCase(strings.ReplaceAll(key, "-", " "))
}

// titleCase uppercases the first letter of each space-separated word.
// (strings.Title is deprecated; we only need ASCII behavior.)
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// IsFrequentlyAccessed reports whether a category is one you come back to
// often, and so should sit in an easy-to-reach spot. Derived from the
// vocabulary table, never hand-maintained.
func IsFrequentlyAccessed(s string) bool { return frequentlyAccessed[MatchCategory(s)] }
