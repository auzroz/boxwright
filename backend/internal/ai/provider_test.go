package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boxwright/internal/placement"
)

// liveVocabulary is the shape the API layer hands a provider: the user's own
// Homebox tags, which mostly are NOT in our canonical seed list.
func liveVocabulary() []placement.Category {
	return []placement.Category{
		{Key: "micro-masterpieces", Label: "Micro Masterpieces"},
		{Key: "health", Label: "Health"},
		{Key: "servers", Label: "Servers"},
		{Key: "tools", Label: "Tools", FrequentlyAccessed: true},
	}
}

// The prompt used to restate a hardcoded eleven-word vocabulary, which is how
// a model was told to answer "other" for an item the user would have tagged
// "Health". It must now be built from whatever list it is handed.
func TestIdentifyPromptListsTheSuppliedCategories(t *testing.T) {
	got := identifyPrompt(liveVocabulary())

	for _, key := range []string{"micro-masterpieces", "health", "servers", "tools"} {
		if !strings.Contains(got, key) {
			t.Errorf("prompt omits the supplied category %q:\n%s", key, got)
		}
	}
	// A canonical seed that was NOT supplied proves the list is the argument
	// and not a constant that happens to contain the same words.
	if strings.Contains(got, "seasonal-holiday") {
		t.Errorf("prompt names seasonal-holiday, which was not supplied; it is still hardcoded:\n%s", got)
	}
}

// The whole point of showing the list is that the model may go outside it.
func TestIdentifyPromptInvitesANewCategory(t *testing.T) {
	got := strings.ToLower(identifyPrompt(liveVocabulary()))

	for _, phrase := range []string{"not exhaustive", "propose"} {
		if !strings.Contains(got, phrase) {
			t.Errorf("prompt never says %q; a model will treat the list as closed:\n%s", phrase, got)
		}
	}
}

// An empty vocabulary is reachable (a brand-new Homebox with no tags and no
// seeds passed). The prompt must still be a usable instruction, not a dangling
// "choose one of:".
func TestIdentifyPromptWithoutCategoriesStillAsksForOne(t *testing.T) {
	got := identifyPrompt(nil)

	if !strings.Contains(got, `"category"`) {
		t.Errorf("prompt no longer asks for a category at all:\n%s", got)
	}
	if strings.Contains(got, "in this inventory:") {
		t.Errorf("prompt introduces a category list it does not have:\n%s", got)
	}
}

// The feature: a model that answers with a category nobody listed must reach
// the draft intact. Folding it to "other" -- or to a canonical neighbour --
// would be Boxwright overwriting the user's vocabulary with its own.
func TestCategoryOutsideTheVocabularySurvivesToTheDraft(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"content": `{"name":"Resin diorama","category":"Micro Masterpieces","sizeBucket":"S",` +
					`"fragile":true,"weightClass":"light","notes":"","confidence":0.8}`,
			}}},
		})
	}))
	t.Cleanup(srv.Close)

	p, err := New("openai", srv.URL, "k", "gpt-x")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	drafts, err := p.Identify(context.Background(), []byte("jpeg"), "image/jpeg", liveVocabulary())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(drafts))
	}

	// Shape-folded (lowercase, hyphenated) but not remapped: that fold is what
	// ResolveTag matches the user's existing "Micro Masterpieces" tag with.
	if drafts[0].Category != "micro-masterpieces" {
		t.Errorf("category = %q, want %q kept as the model named it", drafts[0].Category, "micro-masterpieces")
	}

	// And the vocabulary really travelled to the model.
	prompt := firstPromptText(t, sent)
	if !strings.Contains(prompt, "servers") {
		t.Errorf("request prompt omits the supplied vocabulary:\n%s", prompt)
	}
}

// firstPromptText digs the text part out of an OpenAI chat request body.
func firstPromptText(t *testing.T, body map[string]any) string {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("no messages in request body %v", body)
	}
	first, _ := msgs[0].(map[string]any)
	parts, _ := first["content"].([]any)
	for _, p := range parts {
		m, _ := p.(map[string]any)
		if m["type"] == "text" {
			s, _ := m["text"].(string)
			return s
		}
	}
	t.Fatalf("no text part in %v", parts)
	return ""
}

// The "none" provider still has to satisfy the widened interface, and still
// has to send the caller to manual entry rather than failing hard.
func TestNoneProviderStillPointsAtManualEntry(t *testing.T) {
	p, err := New("none", "", "", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Identify(context.Background(), nil, "", liveVocabulary()); err != ErrNoProvider {
		t.Errorf("err = %v, want ErrNoProvider", err)
	}
}

// The photo of a shelf is the case the single-item contract silently lost, so
// the prompt has to ask for an array -- and has to say, in the same breath,
// that one object is normal and that padding the array is not.
func TestIdentifyPromptAsksForAnArrayWithoutInvitingInvention(t *testing.T) {
	got := strings.ToLower(identifyPrompt(liveVocabulary()))

	for _, phrase := range []string{
		"json array",      // the shape itself
		"one-element",     // a single item is still an array
		"do not invent",   // ... but do not pad it
		"background",      // the room is not the inventory
		"quantity",        // twelve mason jars are one draft, not twelve
		"respond with []", // an empty shelf is a legal answer
	} {
		if !strings.Contains(got, phrase) {
			t.Errorf("prompt never says %q:\n%s", phrase, got)
		}
	}
	// The old singular instruction is what produced one draft for three
	// objects; leaving it in would contradict everything above.
	if strings.Contains(got, "only a json object") {
		t.Errorf("prompt still demands a single JSON object:\n%s", got)
	}
}

// parseDrafts is the last line of defence against a capture being lost. A
// model that answers with a bare object, or wraps the array in one, has still
// told us what it saw; refusing that reply would throw away a photo the user
// may not be able to take again because the item is already in the box.
func TestParseDrafts(t *testing.T) {
	const drill = `{"name":"cordless drill","category":"Tools","sizeBucket":"M","weightClass":"heavy","confidence":0.9}`
	const level = `{"name":"spirit level","category":"tools","sizeBucket":"L","weightClass":"light","confidence":0.7}`

	tests := []struct {
		name      string
		content   string
		wantNames []string
		wantErr   bool
	}{
		{"the array it was asked for", "[" + drill + "," + level + "]",
			[]string{"cordless drill", "spirit level"}, false},
		{"single-element array", "[" + drill + "]", []string{"cordless drill"}, false},
		{"empty array is not an error", "[]", nil, false},
		{"bare object, tolerated as one element", drill, []string{"cordless drill"}, false},
		{"array wrapped in an object", `{"items":[` + drill + `,` + level + `]}`,
			[]string{"cordless drill", "spirit level"}, false},
		{"fenced", "```json\n[" + drill + "]\n```", []string{"cordless drill"}, false},
		{"prose on both sides", "Sure!\n[" + drill + "]\nHope that helps.",
			[]string{"cordless drill"}, false},
		{"prose around a bare object", "I see: " + drill + " -- that's all.",
			[]string{"cordless drill"}, false},
		{"not json at all", "I can see a drill and a level.", nil, true},
		{"truncated array", "[" + drill, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDrafts(tc.content)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDrafts: %v", err)
			}
			if got == nil {
				t.Fatal("nil slice: it would marshal to null and red-screen the app")
			}
			if len(got) != len(tc.wantNames) {
				t.Fatalf("got %d drafts %+v, want %d", len(got), got, len(tc.wantNames))
			}
			for i, want := range tc.wantNames {
				if got[i].Name != want {
					t.Errorf("drafts[%d].Name = %q, want %q", i, got[i].Name, want)
				}
				// Every element is normalized, not just the first: the second
				// draft's "L"/"light" and a title-cased category have to fold
				// exactly like the first's or the engine scores them wrong.
				if got[i].Quantity < 1 || got[i].Category != strings.ToLower(got[i].Category) {
					t.Errorf("drafts[%d] was not normalized: %+v", i, got[i])
				}
			}
		})
	}
}

// The end to end of the array contract on a provider that has no schema to
// lean on: three objects in, three drafts out.
func TestOpenAIReturnsEveryObjectInTheReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"content": `[{"name":"cordless drill","category":"tools","sizeBucket":"M","weightClass":"heavy"},
				             {"name":"tape measure","category":"tools","sizeBucket":"S","weightClass":"light"},
				             {"name":"spirit level","category":"tools","sizeBucket":"L","weightClass":"light"}]`,
			}}},
		})
	}))
	t.Cleanup(srv.Close)

	p, err := New("openai", srv.URL, "k", "gpt-x")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	drafts, err := p.Identify(context.Background(), []byte("jpeg"), "image/jpeg", liveVocabulary())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(drafts) != 3 {
		t.Fatalf("got %d drafts, want all 3 -- the other two were the ones being lost: %+v", len(drafts), drafts)
	}
	if drafts[0].Name != "cordless drill" || drafts[2].Name != "spirit level" {
		t.Errorf("drafts came back in the wrong order: %+v", drafts)
	}
}

// An empty shelf is an answer, not a failure. The API layer turns it into the
// manual-entry 422; the provider must not turn it into an error first.
func TestEmptyArrayIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "[]"}}},
		})
	}))
	t.Cleanup(srv.Close)

	p, _ := New("openai", srv.URL, "k", "gpt-x")
	drafts, err := p.Identify(context.Background(), []byte("jpeg"), "image/jpeg", nil)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(drafts) != 0 {
		t.Errorf("got %+v, want no drafts", drafts)
	}
}

// Models put brackets in prose. Assuming the first bracket opens the payload
// throws the draft away whenever they do -- and losing a capture to a sentence
// is the kind of failure nobody would ever diagnose from the error message.
func TestJSONPayloadSurvivesProseContainingBrackets(t *testing.T) {
	drill := `{"name":"cordless drill","category":"tools","sizeBucket":"M","fragile":false,"weightClass":"heavy","notes":"","confidence":0.9,"quantity":1}`
	tests := []struct{ name, content string }{
		{"prose with a bracket before the object",
			"Based on the photo [a workbench], here is the item:\n" + drill},
		{"prose after the array",
			"[" + drill + "] Hope that helps!"},
		{"prose both sides of the array",
			"I can see 1 item [clearly]:\n[" + drill + "]\nLet me know if you need more."},
		{"fenced with a lead-in",
			"Sure — here's the JSON:\n```json\n[" + drill + "]\n```"},
		{"bare object", drill},
		{"bare array", "[" + drill + "]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDrafts(tc.content)
			if err != nil {
				t.Fatalf("parseDrafts: %v", err)
			}
			if len(got) != 1 || got[0].Name != "cordless drill" {
				t.Errorf("got %+v, want one cordless drill", got)
			}
		})
	}
}

// A model's size for an item is its estimate from what the object is, and is
// marked so -- whatever the reply claims about where it came from. A guess
// from a photo with no scale in it must never become a measurement that
// rules containers out.
func TestAModelsSizeIsAlwaysAnEstimate(t *testing.T) {
	reply := `[
	  {"name":"drill","category":"tools","sizeBucket":"M","quantity":1,
	   "dimensionsCm":{"l":20,"w":25,"h":8},"dimensionsSource":"lidar"},
	  {"name":"mystery","category":"other","sizeBucket":"S","quantity":1,"dimensionsCm":null},
	  {"name":"rope","category":"other","sizeBucket":"M","quantity":1,"dimensionsCm":{"l":0,"w":0,"h":0}}
	]`
	drafts, err := parseDrafts(reply)
	if err != nil {
		t.Fatalf("parseDrafts: %v", err)
	}
	if d := drafts[0]; d.DimensionsCm == nil || d.DimensionsCm.String() != "25x20x8" || d.DimensionsSource != placement.DimsVision {
		t.Errorf("drill = %v from %q, want 25x20x8 from vision", d.DimensionsCm, d.DimensionsSource)
	}
	for _, d := range drafts[1:] {
		if d.DimensionsCm != nil || d.DimensionsSource != "" {
			t.Errorf("%s = %v from %q, want no size", d.Name, d.DimensionsCm, d.DimensionsSource)
		}
	}
}
