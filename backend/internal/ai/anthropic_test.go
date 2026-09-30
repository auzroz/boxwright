package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boxwright/internal/placement"
)

// There is no API key on this machine, so every test here runs against a fake
// Messages endpoint. The request assertions are the contract: they are what
// would catch a wire-format drift before it reaches a paid endpoint.

// anthropicFake serves one canned response and records the request it saw.
type anthropicFake struct {
	server *httptest.Server
	path   string
	header http.Header
	body   map[string]any
}

func newAnthropicFake(t *testing.T, status int, response string) *anthropicFake {
	t.Helper()
	f := &anthropicFake{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.path = r.URL.Path
		f.header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &f.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func newAnthropicProvider(t *testing.T, baseURL, apiKey, model string) Identifier {
	t.Helper()
	p, err := New("anthropic", baseURL, apiKey, model)
	if err != nil {
		t.Fatalf("New(anthropic): %v", err)
	}
	return p
}

const anthropicOKReply = `{"id":"msg_1","type":"message","role":"assistant",
  "content":[{"type":"text","text":"[{\"name\":\"cordless drill\",\"category\":\"tools\",\"sizeBucket\":\"M\",\"fragile\":false,\"weightClass\":\"medium\",\"notes\":\"\",\"confidence\":0.9,\"quantity\":1}]"}],
  "stop_reason":"end_turn"}`

// The request is the part we cannot verify against the real API here, so pin
// every field of it: headers, model, the base64 image and its media type.
func TestAnthropicRequestConstruction(t *testing.T) {
	f := newAnthropicFake(t, http.StatusOK, anthropicOKReply)
	p := newAnthropicProvider(t, f.server.URL, "sk-ant-test", "claude-haiku-4-5")

	image := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}
	if _, err := p.Identify(context.Background(), image, "image/png", liveVocabulary()); err != nil {
		t.Fatalf("Identify: %v", err)
	}

	if f.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", f.path)
	}
	if got := f.header.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := f.header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
	if got := f.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	// The Anthropic API authenticates with x-api-key; a stray bearer header
	// would mean the OpenAI provider was copied without adapting the auth.
	if got := f.header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header should not be sent, got %q", got)
	}
	if got := f.body["model"]; got != "claude-haiku-4-5" {
		t.Errorf("model = %v, want the configured model", got)
	}
	if got := f.body["max_tokens"]; got != float64(anthropicMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got, anthropicMaxTokens)
	}

	content := userContent(t, f.body)
	if len(content) != 2 {
		t.Fatalf("want an image block and a text block, got %d blocks", len(content))
	}
	img, _ := content[0].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("first block = %v, want the image", img)
	}
	src, _ := img["source"].(map[string]any)
	if src["type"] != "base64" {
		t.Errorf("source.type = %v, want base64", src["type"])
	}
	if src["media_type"] != "image/png" {
		t.Errorf("media_type = %v, want the uploaded mime", src["media_type"])
	}
	data, _ := src["data"].(string)
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("source.data is not base64: %v", err)
	}
	if string(decoded) != string(image) {
		t.Errorf("image bytes round-tripped wrong: %x", decoded)
	}
	txt, _ := content[1].(map[string]any)
	if txt["text"] != identifyPrompt(liveVocabulary()) {
		t.Errorf("text block = %q, want the prompt built from the supplied vocabulary", txt["text"])
	}
}

// Structured outputs are sent as output_config.format, not the deprecated
// top-level output_format, and the schema must now describe an ARRAY of
// ItemDrafts -- an object schema would have the decoder itself veto the
// multi-item answer the prompt asks for.
func TestAnthropicRequestConstrainsOutputToTheDraftSchema(t *testing.T) {
	f := newAnthropicFake(t, http.StatusOK, anthropicOKReply)
	p := newAnthropicProvider(t, f.server.URL, "sk-ant-test", "")

	if _, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary()); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if _, ok := f.body["output_format"]; ok {
		t.Error("output_format is deprecated API-wide; use output_config.format")
	}
	cfg, ok := f.body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing: %v", f.body)
	}
	format, ok := cfg["format"].(map[string]any)
	if !ok {
		t.Fatalf("output_config.format missing: %v", cfg)
	}
	if format["type"] != "json_schema" {
		t.Errorf("format.type = %v, want json_schema", format["type"])
	}
	schema, _ := format["schema"].(map[string]any)
	if schema["type"] != "array" {
		t.Fatalf("schema.type = %v, want array: one draft per object in the photo", schema["type"])
	}
	element, ok := schema["items"].(map[string]any)
	if !ok {
		t.Fatalf("schema.items missing: %v", schema)
	}
	props, _ := element["properties"].(map[string]any)
	// Every field the PROMPT asks for has to be here. The object is closed, so
	// a field the schema does not name cannot be generated at all -- asking
	// for one in the prompt and forbidding it here makes it silently missing
	// on the one provider that constrains its output.
	for _, field := range []string{"name", "category", "sizeBucket", "fragile", "bulky",
		"weightClass", "notes", "confidence", "quantity", "region", "dimensionsCm"} {
		if _, ok := props[field]; !ok {
			t.Errorf("element schema is missing ItemDraft field %q", field)
		}
	}
	if element["additionalProperties"] != false {
		t.Error("schema should close additionalProperties so the reply is exactly the draft")
	}

	// The subset requires every declared property to appear in required, so an
	// optional field has to be expressed as nullable instead of omitted.
	inRequired := map[string]bool{}
	for _, f := range stringsOf(element["required"]) {
		inRequired[f] = true
	}
	for field := range props {
		if !inRequired[field] {
			t.Errorf("%q is declared but not in required; the json_schema subset rejects that", field)
		}
	}
	region, _ := props["region"].(map[string]any)
	nullable := false
	for _, t2 := range stringsOf(region["type"]) {
		if t2 == "null" {
			nullable = true
		}
	}
	if !nullable {
		t.Errorf("region.type = %v; it must accept null, or a model that cannot see the object "+
			"has no way to say so and must invent a box", region["type"])
	}
}

// stringsOf reads a JSON array of strings, which comes back as []any once the
// request body has been through the wire, and as []string straight off the
// schema builder.
func stringsOf(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

// The prompt and the schema are two statements of the same contract, in two
// files, and nothing but this connects them. A field added to one and not the
// other fails silently: the model simply cannot answer what it was asked.
func TestEveryFieldTheIdentifyPromptAsksForIsInTheSchema(t *testing.T) {
	prompt := identifyPrompt(liveVocabulary())
	props, _ := itemDraftSchema()["properties"].(map[string]any)
	for field := range props {
		if !strings.Contains(prompt, `"`+field+`"`) {
			t.Errorf("the schema declares %q but the prompt never asks for it", field)
		}
	}
	// And the other way: the prompt's own JSON skeleton is the list of keys a
	// model is told to emit.
	for _, field := range []string{"name", "category", "sizeBucket", "fragile", "bulky",
		"weightClass", "notes", "confidence", "quantity", "region", "dimensionsCm"} {
		if !strings.Contains(prompt, `"`+field+`"`) {
			t.Fatalf("test is stale: the prompt no longer asks for %q", field)
		}
		if _, ok := props[field]; !ok {
			t.Errorf("the prompt asks for %q but the schema forbids it, so it can never be answered", field)
		}
	}
}

// The multi-item path, end to end on the provider that has a schema: three
// objects on a shelf come back as three drafts, in order.
func TestAnthropicReturnsEveryObjectInTheReply(t *testing.T) {
	const shelf = `{"content":[{"type":"text","text":"[` +
		`{\"name\":\"cordless drill\",\"category\":\"tools\",\"sizeBucket\":\"M\",\"weightClass\":\"heavy\",\"quantity\":1},` +
		`{\"name\":\"tape measure\",\"category\":\"tools\",\"sizeBucket\":\"S\",\"weightClass\":\"light\",\"quantity\":1},` +
		`{\"name\":\"mason jar\",\"category\":\"kitchen\",\"sizeBucket\":\"S\",\"weightClass\":\"light\",\"quantity\":12}` +
		`]"}],"stop_reason":"end_turn"}`

	f := newAnthropicFake(t, http.StatusOK, shelf)
	p := newAnthropicProvider(t, f.server.URL, "k", "")
	drafts, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(drafts) != 3 {
		t.Fatalf("got %d drafts, want 3: %+v", len(drafts), drafts)
	}
	if drafts[1].Name != "tape measure" {
		t.Errorf("drafts[1] = %q, want the tape measure in the order the model listed it", drafts[1].Name)
	}
	// Twelve identical jars are one draft with a quantity, not twelve drafts.
	if drafts[2].Quantity != 12 {
		t.Errorf("drafts[2].Quantity = %d, want 12", drafts[2].Quantity)
	}
}

// A model that ignores the array schema and answers with the bare object is
// the failure mode this contract change introduces. Accepting it as one draft
// keeps the capture; refusing it would lose a photo the user may not be able
// to retake, because the item is already in the box.
func TestAnthropicToleratesABareObject(t *testing.T) {
	const bare = `{"content":[{"type":"text","text":"{\"name\":\"mug\",\"category\":\"kitchen\",` +
		`\"sizeBucket\":\"S\",\"fragile\":true,\"weightClass\":\"light\",\"confidence\":0.8}"}],` +
		`"stop_reason":"end_turn"}`

	f := newAnthropicFake(t, http.StatusOK, bare)
	p := newAnthropicProvider(t, f.server.URL, "k", "")
	drafts, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(drafts) != 1 || drafts[0].Name != "mug" {
		t.Fatalf("got %+v, want the one mug read as a single-element array", drafts)
	}
}

// AI_MODEL is optional for this provider; leaving it unset must not send an
// empty model string.
func TestAnthropicDefaultsModelAndHost(t *testing.T) {
	f := newAnthropicFake(t, http.StatusOK, anthropicOKReply)
	p := newAnthropicProvider(t, f.server.URL, "sk-ant-test", "")
	if _, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary()); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if got := f.body["model"]; got != "claude-sonnet-5-5" {
		t.Errorf("model = %v, want the claude-sonnet-5-5 default (chosen by cmd/identeval)", got)
	}

	// With no AI_BASE_URL at all the provider must still target Anthropic.
	unset, err := New("anthropic", "", "sk-ant-test", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := unset.(*anthropicProvider).baseURL; got != anthropicDefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", got, anthropicDefaultBaseURL)
	}
}

// The host must stay overridable so a proxy or gateway keeps working, and a
// base URL copied from the OpenAI-style examples must not double the /v1.
func TestAnthropicBaseURLOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
	}{
		{"bare host", ""},
		{"trailing slash", "/"},
		{"openai-style /v1 suffix", "/v1"},
		{"trailing slash after /v1", "/v1/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnthropicFake(t, http.StatusOK, anthropicOKReply)
			p := newAnthropicProvider(t, f.server.URL+tc.suffix, "k", "")
			if _, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary()); err != nil {
				t.Fatalf("Identify: %v", err)
			}
			if f.path != "/v1/messages" {
				t.Errorf("path = %q, want /v1/messages", f.path)
			}
		})
	}
}

// The API rejects a media_type it does not know, and multipart uploads carry
// parameters and odd spellings.
func TestAnthropicMediaType(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"image/png", "image/png"},
		{"image/jpeg", "image/jpeg"},
		{"image/gif", "image/gif"},
		{"image/webp", "image/webp"},
		{"IMAGE/PNG", "image/png"},
		{"image/jpeg; charset=binary", "image/jpeg"},
		{"image/jpg", "image/jpeg"},
		{"application/octet-stream", "image/jpeg"},
		{"", "image/jpeg"},
	} {
		if got := anthropicMediaType(tc.in); got != tc.want {
			t.Errorf("anthropicMediaType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAnthropicReplies(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     placementWant
	}{
		{
			name:     "plain json",
			response: anthropicOKReply,
			want:     placementWant{name: "cordless drill", category: "tools", size: "M", weight: "medium", quantity: 1, confidence: 0.9},
		},
		{
			name: "fenced json with prose",
			response: `{"content":[{"type":"text","text":"Here you go:\n` + "```json" +
				`\n{\"name\":\"ski boots\",\"category\":\"Sports Outdoor\",\"sizeBucket\":\"l\",\"weightClass\":\"HEAVY\",\"confidence\":0.4}\n` +
				"```" + `"}],"stop_reason":"end_turn"}`,
			// Normalize() must still run even though a schema was requested:
			// it lowercases the category, hyphenates it, uppercases the bucket
			// and floors the quantity at 1.
			want: placementWant{name: "ski boots", category: "sports-outdoor", size: "L", weight: "heavy", quantity: 1, confidence: 0.4},
		},
		{
			name: "out-of-vocabulary values are clamped",
			response: `{"content":[{"type":"text","text":"{\"name\":\"vase\",\"category\":\"  Decor \",` +
				`\"sizeBucket\":\"enormous\",\"weightClass\":\"featherlight\",\"confidence\":7}"}],"stop_reason":"end_turn"}`,
			want: placementWant{name: "vase", category: "decor", size: "M", weight: "medium", quantity: 1, confidence: 1},
		},
		{
			name: "thinking block precedes the text block",
			response: `{"content":[{"type":"thinking","thinking":"the user photographed a mug"},` +
				`{"type":"text","text":"{\"name\":\"mug\",\"category\":\"kitchen\",\"sizeBucket\":\"S\",\"fragile\":true,\"weightClass\":\"light\",\"confidence\":0.8}"}],` +
				`"stop_reason":"end_turn"}`,
			want: placementWant{name: "mug", category: "kitchen", size: "S", weight: "light", quantity: 1, confidence: 0.8},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnthropicFake(t, http.StatusOK, tc.response)
			p := newAnthropicProvider(t, f.server.URL, "k", "")
			drafts, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary())
			if err != nil {
				t.Fatalf("Identify: %v", err)
			}
			if len(drafts) != 1 {
				t.Fatalf("got %d drafts, want 1: %+v", len(drafts), drafts)
			}
			tc.want.check(t, drafts[0])
		})
	}
}

// A refusal and an empty reply are both HTTP 200 with nothing to review, so
// both must degrade to the manual-entry path rather than failing the flow.
func TestAnthropicDegradesToManualEntry(t *testing.T) {
	tests := []struct {
		name, response, wantIn string
	}{
		{
			name:     "refusal",
			response: `{"content":[],"stop_reason":"refusal","stop_details":{"category":"cyber","explanation":"declined"}}`,
			wantIn:   "declined",
		},
		{
			name:     "refusal with content still present",
			response: `{"content":[{"type":"text","text":"{\"name\":\"bomb\"}"}],"stop_reason":"refusal"}`,
			wantIn:   "declined",
		},
		{
			name:     "empty content array",
			response: `{"content":[],"stop_reason":"end_turn"}`,
			wantIn:   "no text content",
		},
		{
			name:     "no text block among the content",
			response: `{"content":[{"type":"thinking","thinking":"hmm"}],"stop_reason":"end_turn"}`,
			wantIn:   "no text content",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnthropicFake(t, http.StatusOK, tc.response)
			p := newAnthropicProvider(t, f.server.URL, "k", "")
			_, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary())
			if err == nil {
				t.Fatal("want an error, got a draft")
			}
			if !errors.Is(err, ErrNoProvider) {
				t.Errorf("error must wrap ErrNoProvider so the app offers manual entry, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should explain the cause (%q)", err, tc.wantIn)
			}
		})
	}
}

// Real failures must NOT masquerade as the manual-entry degradation: a 500 or
// a broken body is a bug or an outage and should surface as one.
func TestAnthropicHardFailures(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		wantIn   string
	}{
		{"http error", http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "status 401"},
		{"malformed body", http.StatusOK, `{"content":[{"type":"text",`, "decode"},
		{"text is not json", http.StatusOK, `{"content":[{"type":"text","text":"I see a drill."}],"stop_reason":"end_turn"}`, "unparseable"},
		{"truncated at the token limit", http.StatusOK, `{"content":[{"type":"text","text":"{\"name\":\"cord"}],"stop_reason":"max_tokens"}`, "token limit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnthropicFake(t, tc.status, tc.response)
			p := newAnthropicProvider(t, f.server.URL, "k", "")
			_, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary())
			if err == nil {
				t.Fatal("want an error, got a draft")
			}
			if errors.Is(err, ErrNoProvider) {
				t.Errorf("a hard failure must not read as manual-entry degradation: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should contain %q", err, tc.wantIn)
			}
		})
	}
}

// An API key must never reach an error string that ends up in a log or an
// HTTP response body.
func TestAnthropicErrorsDoNotLeakTheKeyOrTheImage(t *testing.T) {
	const key = "sk-ant-super-secret"
	f := newAnthropicFake(t, http.StatusForbidden, `{"error":{"message":"forbidden"}}`)
	p := newAnthropicProvider(t, f.server.URL, key, "")
	_, err := p.Identify(context.Background(), []byte("SECRETIMAGEBYTES"), "image/jpeg", liveVocabulary())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error leaked the API key: %v", err)
	}
	if strings.Contains(err.Error(), "SECRETIMAGEBYTES") ||
		strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("SECRETIMAGEBYTES"))) {
		t.Errorf("error leaked the image: %v", err)
	}
}

// A gateway may authenticate on our behalf, so an empty key sends no header
// rather than an empty one.
func TestAnthropicOmitsEmptyAPIKeyHeader(t *testing.T) {
	f := newAnthropicFake(t, http.StatusOK, anthropicOKReply)
	p := newAnthropicProvider(t, f.server.URL, "", "")
	if _, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary()); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if _, ok := f.header["X-Api-Key"]; ok {
		t.Error("an unset AI_API_KEY should send no x-api-key header at all")
	}
}

// placementWant is the post-Normalize() shape a reply must produce. Asserting
// the normalized vocabulary is what proves Normalize() ran: a structured-output
// schema fixes the shape, not the casing, the spelling or the ranges.
type placementWant struct {
	name       string
	category   string
	size       string
	weight     string
	quantity   int
	confidence float64
}

func (w placementWant) check(t *testing.T, got placement.ItemDraft) {
	t.Helper()
	if got.Name != w.name {
		t.Errorf("Name = %q, want %q", got.Name, w.name)
	}
	if got.Category != w.category {
		t.Errorf("Category = %q, want %q (NormalizeCategory)", got.Category, w.category)
	}
	if got.SizeBucket != w.size {
		t.Errorf("SizeBucket = %q, want %q", got.SizeBucket, w.size)
	}
	if got.WeightClass != w.weight {
		t.Errorf("WeightClass = %q, want %q", got.WeightClass, w.weight)
	}
	if got.Quantity != w.quantity {
		t.Errorf("Quantity = %d, want %d", got.Quantity, w.quantity)
	}
	if got.Confidence != w.confidence {
		t.Errorf("Confidence = %v, want %v", got.Confidence, w.confidence)
	}
}

// userContent digs the content array out of the recorded request body.
func userContent(t *testing.T, body map[string]any) []any {
	t.Helper()
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("want exactly one user message, got %v", body["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role = %v, want user", msg["role"])
	}
	content, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content is not an array: %v", msg["content"])
	}
	return content
}

// The Messages API's json_schema subset does not accept numerical or string
// constraints (minimum, maximum, multipleOf, minLength, maxLength) and rejects
// additionalProperties set to anything but false. The official SDKs strip
// unsupported keywords before sending; a hand-rolled client has to not emit
// them in the first place, or the very first real request 400s -- which no
// httptest fake would ever catch.
func TestStructuredOutputSchemaStaysInsideTheSupportedSubset(t *testing.T) {
	raw, err := json.Marshal(itemDraftFormat())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	for _, banned := range []string{
		`"minimum"`, `"maximum"`, `"multipleOf"`, `"minLength"`, `"maxLength"`,
		`"minItems"`, `"maxItems"`, `"pattern"`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("schema contains %s, which the Messages API json_schema subset rejects: %s",
				banned, body)
		}
	}
	if !strings.Contains(body, `"additionalProperties":false`) {
		t.Errorf("every object in the schema must set additionalProperties:false: %s", body)
	}
}

// Extended thinking is declined on purpose: this is extraction against a fixed
// schema, and leaving it on cost 1212 extra output tokens and 60 extra seconds
// on a measured photo, for an answer that was no better.
// Measured on the Opus 5 generation, and still the default for a family the
// eval has not shown to do better with thinking.
func TestThinkingIsDeclined(t *testing.T) {
	f := newAnthropicFake(t, http.StatusOK, `{"content":[{"type":"text","text":"[]"}],"stop_reason":"end_turn"}`)
	p := newAnthropicProvider(t, f.server.URL, "k", "claude-opus-5")
	_, _ = p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary())

	thinking, ok := f.body["thinking"].(map[string]any)
	if !ok {
		t.Fatal("no thinking parameter: the reply will carry deliberation nobody reads and " +
			"the user waits a minute for it")
	}
	if thinking["type"] != "disabled" {
		t.Errorf("thinking = %v, want disabled", thinking)
	}
}

// AI_MODEL is free text pointing at a model list that changes, so a model that
// has never had extended thinking must not turn an optimisation into every
// capture failing.
func TestAThinkingRejectionIsRetriedWithoutIt(t *testing.T) {
	var bodies [][]byte
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"thinking: unsupported parameter for this model"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"[{\"name\":\"drill\",\"category\":\"tools\"}]"}],"stop_reason":"end_turn"}`))
	}))
	t.Cleanup(srv.Close)

	p := newAnthropicProvider(t, srv.URL, "k", "")
	drafts, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(drafts) != 1 || drafts[0].Name != "drill" {
		t.Fatalf("drafts = %+v, want the capture to have survived", drafts)
	}
	if calls != 2 {
		t.Fatalf("%d calls, want exactly one retry", calls)
	}
	var second map[string]any
	json.Unmarshal(bodies[1], &second)
	if _, present := second["thinking"]; present {
		t.Error("the retry still carried the parameter that was just rejected")
	}
}

// And the retry happens ONCE. A model that rejects everything must surface the
// error, not loop.
func TestAPersistentRejectionIsNotRetriedForever(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"thinking is not supported"}}`))
	}))
	t.Cleanup(srv.Close)

	p := newAnthropicProvider(t, srv.URL, "k", "")
	if _, err := p.Identify(context.Background(), []byte("x"), "image/jpeg", liveVocabulary()); err == nil {
		t.Fatal("want an error once the retry has also failed")
	}
	if calls != 2 {
		t.Errorf("%d calls, want 2 (the attempt and one retry)", calls)
	}
}
