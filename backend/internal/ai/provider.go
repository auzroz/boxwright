// Package ai provides model-agnostic vision identification.
// Providers: any OpenAI-compatible endpoint, Ollama, Anthropic, the Claude Code
// CLI (development-only, see claude_code.go), or none.
// The rest of the app must work when the provider is "none".
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"boxwright/internal/placement"
)

// ErrNoProvider is returned by the "none" provider; callers should surface
// a manual-entry path, not fail the flow.
var ErrNoProvider = errors.New("no AI provider configured (AI_PROVIDER=none); enter item details manually")

// Identifier turns a photo into reviewable item drafts.
type Identifier interface {
	// Identify returns ONE DRAFT PER distinct object in the photo, for user
	// review. Implementations must never invent certainty: unknown fields stay
	// empty and Confidence reflects it.
	//
	// A capture is a slice of a pile, not a portrait: single items get
	// photographed while packing, and a whole shelf gets photographed in one
	// shot when its contents are obvious. The single-draft contract this
	// replaces turned the second case into silent data loss -- a drill, a tape
	// measure and a level became one draft for whichever object dominated the
	// frame, and nothing told the user the other two were gone. Losing them
	// quietly is worse than refusing the photo outright.
	//
	// An EMPTY result is NOT an error. It means the model saw nothing a person
	// would pack, which the API layer turns into the same manual-entry path as
	// ErrNoProvider.
	//
	// categories is the vocabulary currently in play -- the user's Homebox
	// tags, with our seeds filling the gaps -- and is passed per call rather
	// than held on the provider because it changes every time an item is
	// filed under a new name. It is ADVISORY: implementations must let the
	// model answer with a category that is not in it (see identifyPrompt),
	// because a vocabulary the user invented is the only correct answer for
	// the things they actually own. May be empty; the prompt copes.
	Identify(ctx context.Context, image []byte, mimeType string, categories []placement.Category) ([]placement.ItemDraft, error)
}

// New builds an Identifier from configuration. Options tune how the model is
// asked (effort, thinking); providers that have no such knobs ignore them.
func New(provider, baseURL, apiKey, model string, opts ...Option) (Identifier, error) {
	var t tuning
	for _, o := range opts {
		o(&t)
	}
	if err := ValidateTuning(t.effort, t.thinking); err != nil {
		return nil, err
	}
	switch provider {
	case "openai":
		return &openAICompat{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, model: model}, nil
	case "ollama":
		return &ollama{baseURL: strings.TrimRight(baseURL, "/"), model: model}, nil
	case "anthropic":
		// Unlike the OpenAI-compatible and Ollama providers, both the endpoint
		// and a sensible model are known here, so neither has to be configured.
		if model == "" {
			model = anthropicDefaultModel
		}
		return &anthropicProvider{baseURL: anthropicBaseURL(baseURL), apiKey: apiKey, model: model, tuning: t}, nil
	case "claude-code":
		// Development-only; see the header comment in claude_code.go. baseURL
		// and apiKey are ignored on purpose: the CLI holds its own credentials
		// from an interactive login and knows its own endpoint, so there is
		// nothing here for a user to configure or for us to validate.
		return newClaudeCode(model), nil
	case "none":
		return noneProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown AI provider %q", provider)
	}
}

type noneProvider struct{}

func (noneProvider) Identify(context.Context, []byte, string, []placement.Category) ([]placement.ItemDraft, error) {
	return nil, ErrNoProvider
}

// identifyPrompt instructs vision models to emit a strict JSON ARRAY of
// objects matching placement.ItemDraft. Keep the field list in sync with that
// struct.
//
// The array is the point. Capability was never the blocker -- even a local
// gemma3:4b enumerates the objects on a shelf unprompted -- the single-object
// contract was, and it threw everything but the dominant object away. So the
// prompt has to be explicit in both directions: a photo of one thing is still
// an array (of one), and a photo of three things must not be padded to five.
// Both halves are failure modes a model falls into when only one is stated.
//
// "bulky" exists because a real user photographed a lawn mower and was told to
// put it in a box. The size buckets measure an item AGAINST a container, so
// their top end could only ever say "fills a large tote" -- and XL is 8 units
// against a default container of 8, which made a mower score as exactly
// filling an empty one. No bigger bucket fixes that, because the difference is
// of kind and not of degree: a mower is not a large tote-load, it is something
// that stands on a floor. So it is a separate yes/no question, and one a model
// answers far more reliably than it estimates a volume.
//
// The region is what makes a multi-item photo reviewable. Eight things on a
// shelf produce eight cards that were all illustrated by the same picture of
// the shelf, which tells the user nothing about which row is which -- and the
// row that most needs a picture is the one the model was least sure about.
// It is optional in both directions: a model that ignores it, or that says it
// is unsure, leaves every card showing the whole photo exactly as before.
//
// "fragile" is defined rather than assumed for a measured reason: Haiku and
// gemma3:4b both called a plastic toy guitar fragile while Sonnet and Opus both
// did not, and fragile drives a HARD exclusion in the engine -- it is the one
// field where models disagree and the disagreement changes where the item goes.
// The definition given is deliberately physical (would it break if the closed
// container were dropped) rather than a matter of taste, because taste is
// exactly what produced the split. Measured after: gemma3:4b answers false on
// that guitar 2 runs in 3, against 0 in 3 before. It moves a 4B model most of
// the way, not all of it -- worth knowing before trusting one small model's
// fragile flag as though it were a fact.
//
// The category half is built from the supplied vocabulary rather than stated
// as a constant. It used to name eleven categories we invented, measured
// against a real Homebox where eleven of the user's thirteen tags were
// something else entirely -- so the model was being told to answer "other" for
// items the user files under "Health" or "Micro Masterpieces", and category
// affinity, the engine's dominant signal, scored nothing.
//
// The list is presented as a preference, never a menu. Category is stored as a
// Homebox tag, so a new one is a legitimate, cheap outcome: the vocabulary
// grows to fit the inventory. Forcing a bad match instead is what actually
// costs something -- it poisons the affinity counts of the box it lands in.
func identifyPrompt(categories []placement.Category) string {
	var b strings.Builder
	b.WriteString(`You are cataloging physical items from a photo for a home storage inventory.
The photo may show a single item, or a whole shelf or container of them.

Respond with ONLY a JSON array, no markdown fences, holding one object per
distinct thing a person would pack:
[
  {
    "name": "what it is, 1-4 words. Commit to one name: never \"a or b\"",
    "category": "a category key; see the guidance below",
    "sizeBucket": "S, M, L, or XL, measured against a storage container: S fits in a shoebox, M in a milk crate, L in an 18-gallon tote, XL fills a large tote. Ignored when bulky is true",
    "bulky": true or false; see the definition below,
    "fragile": true or false; see the definition below,
    "weightClass": "light, medium, or heavy",
    "notes": "at most 6 words, and usually empty; see below",
    "confidence": 0.0 to 1.0,
    "quantity": how many identical copies of this same thing are visible, at least 1,
    "region": {"x": 0.0, "y": 0.0, "w": 1.0, "h": 1.0},
    "dimensionsCm": {"l": 0, "w": 0, "h": 0}
  }
]

"dimensionsCm" is your best estimate of ONE of this item's size in
centimetres, longest side first, from what the object IS: a cordless drill is
about 25 x 20 x 8, a paperback about 18 x 11 x 2. The photo has no scale in it,
so judge from the kind of object, not from how many pixels it covers. Use null
when you cannot tell what it is, and for anything bulky.

"region" is where this item is in the photo, as FRACTIONS of the image width
and height, with x,y the top-left corner and 0,0 the top-left of the photo.
Give the smallest box that contains the whole object. It is used to crop the
photo down to this one item so a person can see what you are describing, so a
box around the wrong thing is worse than no box at all: OMIT the region
entirely if you are not confident where the object is, and omit it for a photo
of a single item filling the frame.

One object is the common case: a photo of a single item is correctly answered
with a one-element array. Do NOT invent items to fill the array, and do not
split one item into its parts (a drill and its battery are one drill).
When several identical copies of the same thing are visible, emit ONE object
with quantity set to how many, rather than repeating it.

Every entry becomes a row somebody has to name, categorise and find a box for,
so list only what passes this test: WOULD SOMEONE PICK IT UP AND PUT IT IN A
BOX? If the answer is no, leave it out entirely. That excludes, whatever else
is true of them:

  - anything fixed in place: fitted shelves, blinds, a mirror or a picture on
    the wall, notes pinned to a board, a light fitting, a power socket
  - the furniture the scene is made of and the surface things are resting on:
    the desk, the table, the chair someone sits on, the floor, a rug
  - the background: walls, doors, windows, and the room itself

Being in use is NOT a reason to leave something out. A laptop somebody is
typing on is still a laptop somebody owns and may want to find later.

Being thorough is not the goal; being right about the packable things is. Ten
entries where three of them are the desk, the chair and the wall is a worse
answer than three entries.

If nothing in the photo is something a person would pack, respond with [].

"notes" stays EMPTY unless there is a brand, model number or serial you can
actually read in the photo, or damage worth recording. It is not a description:
the name, category and size already say what the thing is, and repeating that
back costs the user money for nothing. Six words is the ceiling.

"bulky" means this does not go inside a container AT ALL -- a lawn mower, a
bicycle, a floor lamp, a dining chair, a ladder. Ask yourself only this: could
somebody put it inside a storage box or tote? If no, bulky is true and the size
bucket does not matter. If it would fit in a box, however large, bulky is
false. Being heavy is not the same as being bulky: a box of books is heavy and
goes in a container.

"fragile" means ONE thing: would this be damaged if the closed container it is
in were dropped from waist height? Glass, ceramic, screens, lenses, unprotected
electronics and anything with a delicate mechanism are fragile. Sturdy plastic,
metal, wood, fabric and paper are not, even when the object is a musical
instrument, a toy, or something a person would handle carefully for other
reasons. Judge the material and construction, not the sentimental or monetary
value.
`)
	// With no vocabulary to show -- a brand-new Homebox carrying no tags --
	// the closing paragraph must not refer to a list that is not there, or the
	// model is left resolving a dangling reference instead of naming the item.
	if vocab := placement.VocabularyOf(categories); vocab != "" {
		b.WriteString("\nCategories already in this inventory: ")
		b.WriteString(vocab)
		b.WriteString(`

That list is NOT exhaustive, and it is not a menu. Prefer a category from it when one genuinely fits the item. When none does, propose a new category instead of forcing a bad match. A wrong-but-listed category is worse than an accurate new one.
`)
	} else {
		b.WriteString("\nThis inventory has no categories yet, so name one that fits.\n")
	}
	b.WriteString(`
A proposed category is a short lowercase noun, hyphenated if it needs more than one word, such as "sewing" or "pet-supplies".
If you can see an item but cannot identify it, use name "unknown item" and confidence 0.0 rather than dropping it.
Never guess a brand or model you cannot read.`)
	return b.String()
}

// parseDrafts extracts the item drafts from model output, tolerating stray
// markdown fences and surrounding prose.
//
// It accepts three shapes, in descending order of how much the model did as
// it was told:
//
//   - the array it was asked for;
//   - a BARE OBJECT, read as a one-element array. This is the likely failure
//     mode for a small local model, and the whole reason for this change is
//     that losing a capture is the worst outcome available -- so tolerate it.
//     One item is what the user photographed either way;
//   - an object with a single array-valued key ({"items": [...]}), which is
//     the other way a model "helpfully" wraps the answer. Same reasoning.
//
// Everything else is an error: an unparseable reply is a real failure and the
// caller has to know, because there is nothing to review.
func parseDrafts(content string) ([]placement.ItemDraft, error) {
	content = jsonPayload(content)

	if strings.HasPrefix(content, "[") {
		return unmarshalDrafts(content)
	}

	// A single-key wrapper, before falling back to reading the object as one
	// draft: unmarshalling {"items":[...]} into an ItemDraft succeeds and
	// yields a blank draft, so this has to be checked, not merely attempted.
	if inner, ok := wrappedArray(content); ok {
		return unmarshalDrafts(inner)
	}

	var d placement.ItemDraft
	if err := json.Unmarshal([]byte(content), &d); err != nil {
		return nil, fmt.Errorf("model returned unparseable drafts: %w", err)
	}
	fromModel(&d)
	return []placement.ItemDraft{d}, nil
}

// fromModel normalises a draft that came from a vision model.
//
// Its size is the model's estimate, whatever the reply claims. A model that
// writes "dimensionsSource": "lidar" -- the schemaless providers can write
// anything -- must not promote a guess from a photo with no scale in it into a
// measurement that excludes containers.
func fromModel(d *placement.ItemDraft) {
	d.DimensionsSource = ""
	if d.DimensionsCm != nil {
		d.DimensionsSource = placement.DimsVision
	}
	d.Normalize()
}

func unmarshalDrafts(content string) ([]placement.ItemDraft, error) {
	var drafts []placement.ItemDraft
	if err := json.Unmarshal([]byte(content), &drafts); err != nil {
		return nil, fmt.Errorf("model returned unparseable drafts: %w", err)
	}
	for i := range drafts {
		fromModel(&drafts[i])
	}
	// Never nil: an empty result travels all the way to the app as [], and a
	// nil slice would marshal to null and red-screen a client calling .map().
	if drafts == nil {
		drafts = []placement.ItemDraft{}
	}
	return drafts, nil
}

// wrappedArray unwraps {"items": [...]} and its synonyms: any object whose one
// and only key holds an array. Restricting it to a lone key is what keeps a
// genuine draft (no array-valued field, and always several keys) from being
// mistaken for a wrapper.
func wrappedArray(content string) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &obj); err != nil || len(obj) != 1 {
		return "", false
	}
	for _, raw := range obj {
		if v := strings.TrimSpace(string(raw)); strings.HasPrefix(v, "[") {
			return v, true
		}
	}
	return "", false
}

// jsonPayload strips markdown fences and any prose the model wrapped the JSON
// in, returning the outermost array or object it can find.
func jsonPayload(content string) string {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	// Try candidates and keep the first that is actually JSON, rather than
	// assuming the first bracket opens the payload. That assumption loses the
	// reply whenever prose before the JSON contains a bracket of its own --
	// "Based on the photo [a workbench], here is the item: {...}" extracts
	// "[a workbench]" and discards the draft. Models put brackets in prose
	// often enough that guessing is not good enough for a capture.
	if json.Valid([]byte(content)) {
		return content
	}
	if arr := spanBetween(content, '[', ']'); arr != "" && json.Valid([]byte(arr)) {
		return arr
	}
	// A reply that OPENS an array and does not close it is truncated -- the
	// max_tokens cutoff on a shelf full of items. Falling through to the
	// object extractor below would return the first item and silently drop
	// every one after it, which is precisely the loss this contract exists to
	// prevent. Hand back the raw content so the parse fails loudly instead.
	if strings.HasPrefix(content, "[") {
		return content
	}
	if obj := spanBetween(content, '{', '}'); obj != "" && json.Valid([]byte(obj)) {
		return obj
	}
	return content
}

// spanBetween returns the text from the first open to the last close, or "".
func spanBetween(s string, open, close byte) string {
	start := strings.IndexByte(s, open)
	if start < 0 {
		return ""
	}
	end := strings.LastIndexByte(s, close)
	if end <= start {
		return ""
	}
	return s[start : end+1]
}
