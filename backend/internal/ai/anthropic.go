package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"boxwright/internal/placement"
)

const (
	// anthropicDefaultBaseURL is only a default: AI_BASE_URL still wins, so a
	// proxy, gateway or on-prem relay works without patching the provider.
	anthropicDefaultBaseURL = "https://api.anthropic.com"

	// anthropicDefaultModel is Claude Opus 5 ($5/$25 per Mtok). Aliases carry
	// no date suffix -- appending one pins a snapshot that eventually retires.
	anthropicDefaultModel = "claude-opus-5"

	// anthropicVersion is the required API version header, not a model version.
	anthropicVersion = "2023-06-01"

	// anthropicMaxTokens bounds the reply. One draft is ~200 tokens of JSON,
	// but the contract is now an ARRAY: a shelf photographed in one shot can
	// be a dozen or more, and a truncated reply is an unparseable one, so the
	// whole capture is lost rather than degraded. max_tokens is a ceiling, not
	// a reservation -- unused headroom costs nothing -- so it is sized for the
	// case this feature exists to serve rather than the common one.
	anthropicMaxTokens = 8192
)

// anthropicProvider speaks the Anthropic Messages API directly over net/http.
// The official SDK is deliberately not used: it would pull in OpenTelemetry,
// gRPC, protobuf and the AWS SDK to send one POST, which is an unacceptable
// dependency surface for a project whose first rule is "no telemetry".
type anthropicProvider struct {
	baseURL string // e.g. https://api.anthropic.com (no trailing /v1)
	apiKey  string
	model   string
}

func (p *anthropicProvider) Identify(ctx context.Context, image []byte, mimeType string, categories []placement.Category) ([]placement.ItemDraft, error) {
	return p.identify(ctx, p.requestBody(image, mimeType, categories), false)
}

// thinkingOff declines extended thinking. See requestBody for the measurement.
var thinkingOff = map[string]any{"type": "disabled"}

func (p *anthropicProvider) requestBody(image []byte, mimeType string, categories []placement.Category) map[string]any {
	reqBody := map[string]any{
		"model":      p.model,
		"max_tokens": anthropicMaxTokens,
		// Extended thinking is declined, and it is worth saying why because
		// the default is the other way.
		//
		// This is extraction against a fixed schema, not a problem: the model
		// looks at a photo and writes down what is in it. Measured on one
		// cluttered photo, leaving thinking on cost 1212 EXTRA output tokens
		// -- more than the JSON itself -- for an answer that was no better,
		// 46% of the output tokens were the model deliberating about a list of
		// objects it could already see.
		//
		// Latency, honestly: one run came back in 13 seconds and three more in
		// 72-77, so declining thinking buys tokens, not speed. A 16-item photo
		// takes over a minute either way, which is the number to watch when
		// somebody is standing in a storage unit waiting to file a box.
		"thinking": thinkingOff,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					// Image first: putting the picture before the question
					// measurably improves single-image answers.
					{"type": "image", "source": map[string]any{
						"type":       "base64",
						"media_type": anthropicMediaType(mimeType),
						"data":       base64.StdEncoding.EncodeToString(image),
					}},
					{"type": "text", "text": identifyPrompt(categories)},
				},
			},
		},
		// Structured outputs constrain the reply to an array of ItemDrafts, so
		// a chatty model cannot wrap the JSON in prose and cannot answer with
		// the bare object the other providers have to be defended against.
		// parseDrafts still runs: the schema guarantees shape, never
		// vocabulary.
		"output_config": map[string]any{"format": itemDraftFormat()},
	}

	return reqBody
}

// identify sends a prepared body. retriedWithoutThinking guards the one retry
// below so a stubborn model cannot put this in a loop.
func (p *anthropicProvider) identify(ctx context.Context, reqBody map[string]any, retriedWithoutThinking bool) ([]placement.ItemDraft, error) {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	// A gateway may authenticate for us, so an empty key is not an error here.
	if p.apiKey != "" {
		req.Header.Set("x-api-key", p.apiKey)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	// A model that has never had extended thinking may reject the parameter
	// outright, and AI_MODEL is a free-text env var pointing at a model list
	// that changes. Rejected once, we drop the option and go again rather than
	// leaving every capture failing on an optimisation. Retried at most once,
	// and only for this specific complaint.
	if resp.StatusCode == http.StatusBadRequest && !retriedWithoutThinking {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		if strings.Contains(strings.ToLower(string(body)), "thinking") {
			slog.Default().Warn("anthropic rejected the thinking parameter; retrying without it "+
				"(replies will cost more and take longer on this model)", "model", p.model)
			delete(reqBody, "thinking")
			return p.identify(ctx, reqBody, true)
		}
		return nil, fmt.Errorf("anthropic request: status %d: %s", resp.StatusCode, string(body))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// The bounded snippet is the API's own error body; the request (image
		// bytes, API key) is never echoed into an error or a log line.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("anthropic request: status %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		Content     []anthropicBlock `json:"content"`
		StopReason  string           `json:"stop_reason"`
		StopDetails struct {
			Category string `json:"category"`
		} `json:"stop_details"`
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("anthropic decode: %w", err)
	}

	// Logged for every call, before any of the failure paths below return.
	// A photo is a few thousand input tokens and the price differs 5x between
	// models, so "how much is this costing" is a question that needs an answer
	// from the meter rather than from arithmetic done once in a README.
	// Tokens are what the API reports; the dollar figure is derived and says
	// so, because a price list in a source file goes stale silently.
	out.Usage.log(p.model)

	// A refusal is HTTP 200 with no usable answer, so it must be checked before
	// content is read. Wrapping ErrNoProvider degrades it to the manual-entry
	// path the app already has for AI_PROVIDER=none, which keeps the "rules
	// first, AI optional" principle true without any new plumbing.
	if out.StopReason == "refusal" {
		return nil, fmt.Errorf(
			"anthropic declined to describe this photo (%s); %w",
			orUnknown(out.StopDetails.Category), ErrNoProvider)
	}

	text, ok := firstTextBlock(out.Content)
	if !ok {
		// Also covers an empty content array: nothing to review either way.
		return nil, fmt.Errorf(
			"anthropic returned no text content (stop_reason %s); %w",
			orUnknown(out.StopReason), ErrNoProvider)
	}
	if out.StopReason == "max_tokens" {
		// Truncated JSON would otherwise surface as an unparseable-draft error
		// that points at the model instead of at the token budget.
		return nil, fmt.Errorf(
			"anthropic reply hit the %d-token limit before finishing the draft", anthropicMaxTokens)
	}
	return parseDrafts(text)
}

// anthropicUsage is what the reply cost, as the API itself reports it.
type anthropicUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

// anthropicPricing is USD per million tokens, input and output, for the models
// this project documents. Deliberately partial: a model that is not here logs
// its tokens and no dollar figure, which is better than a confident number
// derived from a guess.
var anthropicPricing = map[string][2]float64{
	"claude-opus-5":    {5, 25},
	"claude-sonnet-5":  {2, 10},
	"claude-haiku-4-5": {1, 5},
	"claude-fable-5-1": {5, 25},
}

// log records what one call used, and what that is worth if we know the price.
func (u anthropicUsage) log(model string) {
	attrs := []any{
		"model", model,
		"inputTokens", u.InputTokens,
		"outputTokens", u.OutputTokens,
	}
	if u.CacheReadTokens > 0 || u.CacheCreationTokens > 0 {
		attrs = append(attrs, "cacheReadTokens", u.CacheReadTokens,
			"cacheCreationTokens", u.CacheCreationTokens)
	}
	// Match a dated snapshot (claude-haiku-4-5-20251001) to its alias, so
	// pinning a version does not silently lose the price.
	price, known := anthropicPricing[model]
	if !known {
		for alias, p := range anthropicPricing {
			if strings.HasPrefix(model, alias) {
				price, known = p, true
				break
			}
		}
	}
	if known {
		// Cache reads are billed at a tenth; cache writes at 1.25x. Neither is
		// used here today, but counting them at the input rate would overstate
		// the bill the day someone turns caching on.
		usd := (float64(u.InputTokens)*price[0] +
			float64(u.CacheCreationTokens)*price[0]*1.25 +
			float64(u.CacheReadTokens)*price[0]*0.1 +
			float64(u.OutputTokens)*price[1]) / 1e6
		attrs = append(attrs, "estimatedUSD", fmt.Sprintf("%.5f", usd))
	}
	slog.Default().Info("anthropic call", attrs...)
}

// anthropicBlock is one element of the response content array. Only text
// blocks are of interest; thinking and tool blocks are skipped.
type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// firstTextBlock finds the reply text. Index 0 is not safe: on thinking-enabled
// models the thinking blocks come first in the content array.
func firstTextBlock(blocks []anthropicBlock) (string, bool) {
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return b.Text, true
		}
	}
	return "", false
}

// itemDraftFormat is the output_config.format value: a JSON schema for an
// ARRAY of placement.ItemDrafts, as identifyPrompt describes it.
//
// The array is the contract, so it is the root of the schema. There are
// deliberately no minItems/maxItems -- both because the json_schema subset
// rejects them and because both bounds would be wrong: an empty array is the
// honest answer for a photo of an empty shelf, and a photo of a full shelf has
// no useful upper bound.
func itemDraftFormat() map[string]any {
	return map[string]any{
		"type": "json_schema",
		"schema": map[string]any{
			"type":  "array",
			"items": itemDraftSchema(),
		},
	}
}

// itemDraftSchema is one element: an object mirroring placement.ItemDraft.
//
// Category is deliberately a free string rather than an enum, and must stay
// one. Box categories come from user-defined Homebox tags, and identifyPrompt
// asks the model to propose a new category when nothing in the supplied list
// fits; an enum here would make that instruction unsatisfiable at the decoder
// level -- the schema would silently veto the feature the prompt requests.
// NormalizeCategory folds the casing and spacing instead.
//
// sizeBucket and weightClass keep their enums because those vocabularies are
// ours, closed, and mean something to the engine's arithmetic. The schema
// subset also forbids minLength/pattern (as it does minimum/maximum below) and
// requires additionalProperties:false, so there is no middle ground to reach
// for here anyway.
func itemDraftSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":       map[string]any{"type": "string"},
			"category":   map[string]any{"type": "string"},
			"sizeBucket": map[string]any{"type": "string", "enum": []string{"S", "M", "L", "XL"}},
			"fragile":    map[string]any{"type": "boolean"},
			// Declared here as well as in the prompt, or the closed object
			// below makes it impossible to answer. See the region field.
			"bulky":       map[string]any{"type": "boolean"},
			"weightClass": map[string]any{"type": "string", "enum": []string{"light", "medium", "heavy"}},
			"notes":       map[string]any{"type": "string"},
			// No minimum/maximum: numerical constraints are NOT in the
			// Messages API's json_schema subset and risk a 400. The
			// official SDKs strip them and validate client-side; we get
			// the same guarantee for free because Normalize() clamps
			// confidence to [0,1] and floors quantity at 1 on every
			// provider's output anyway.
			"confidence": map[string]any{"type": "number"},
			// Quantity exists so twelve identical mason jars are one draft
			// rather than twelve. Without it in the schema an enumerating
			// model has no way to say "and eleven more like it" and lists
			// them, which is the array contract's own failure mode.
			"quantity": map[string]any{"type": "integer"},
			// Where the item is in the photo, so the app can crop to it.
			//
			// It has to be HERE, not only in the prompt: this schema closes
			// the object with additionalProperties:false, so a key the schema
			// does not name cannot be generated at all. Asking for a region in
			// the prompt while forbidding it in the schema makes the feature
			// silently inert on the one provider that has a schema -- which is
			// the provider meant for deployment.
			//
			// Optional is expressed as NULLABLE, because the subset requires
			// every declared property to appear in required. A JSON null
			// decodes to a nil *Region, and an all-zero object is dropped by
			// Region.normalized() on w <= 0, so both ways of saying "I don't
			// know" already degrade to showing the whole photo.
			"region": map[string]any{
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"x": map[string]any{"type": "number"},
					"y": map[string]any{"type": "number"},
					"w": map[string]any{"type": "number"},
					"h": map[string]any{"type": "number"},
				},
				"required":             []string{"x", "y", "w", "h"},
				"additionalProperties": false,
			},
			// The model's estimate of one item's size, from what it is. Here
			// for the same reason as region -- the closed object could not
			// otherwise produce it -- and nullable for the same reason. It is
			// only ever an estimate: fromModel marks it "vision", which feeds
			// how much room the item takes and never excludes a container.
			"dimensionsCm": map[string]any{
				"type": []string{"object", "null"},
				"properties": map[string]any{
					"l": map[string]any{"type": "number"},
					"w": map[string]any{"type": "number"},
					"h": map[string]any{"type": "number"},
				},
				"required":             []string{"l", "w", "h"},
				"additionalProperties": false,
			},
		},
		"required": []string{"name", "category", "sizeBucket", "fragile", "bulky",
			"weightClass", "notes", "confidence", "quantity", "region", "dimensionsCm"},
		"additionalProperties": false,
	}
}

// anthropicMediaType narrows a browser/multipart Content-Type to one of the
// four media types the Messages API accepts. Parameters ("image/jpeg;
// charset=binary") and unknown types would be rejected outright, so fall back
// to the same JPEG assumption the upload handler already makes -- guessing has
// a chance of working, a 400 does not.
func anthropicMediaType(mimeType string) string {
	m := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.Index(m, ";"); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	switch m {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return m
	case "image/jpg":
		return "image/jpeg"
	}
	return "image/jpeg"
}

// anthropicBaseURL applies the default and tolerates an AI_BASE_URL copied from
// the OpenAI-style examples: the Messages path already carries /v1, so a base
// ending in /v1 would otherwise POST to /v1/v1/messages.
func anthropicBaseURL(baseURL string) string {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if u == "" {
		return anthropicDefaultBaseURL
	}
	return strings.TrimSuffix(u, "/v1")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
