package ai

import (
	"context"
	"fmt"
	"strings"
)

// Option adjusts how a provider asks its model to work. Only the Anthropic
// provider reads them today; the others ignore them.
type Option func(*tuning)

type tuning struct {
	effort   string
	thinking string
}

// WithEffort sets the reasoning effort ("low" ... "max"), on models that take
// one. Empty leaves the model's own default.
func WithEffort(effort string) Option { return func(t *tuning) { t.effort = effort } }

// WithThinking sets how much the model may think before answering: "auto" (or
// empty), the least the model allows, or "adaptive", the model decides.
func WithThinking(mode string) Option { return func(t *tuning) { t.thinking = mode } }

// Efforts are the effort levels the Messages API accepts.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// ThinkingModes are the values WithThinking accepts.
var ThinkingModes = []string{"auto", "adaptive"}

// ValidateTuning checks an effort and thinking mode as configured, so a typo
// in AI_EFFORT fails at startup instead of on every photo.
func ValidateTuning(effort, thinking string) error {
	if effort != "" && !contains(Efforts, effort) {
		return fmt.Errorf("effort must be one of %s; got %q", strings.Join(Efforts, ", "), effort)
	}
	if thinking != "" && !contains(ThinkingModes, thinking) {
		return fmt.Errorf("thinking must be one of %s; got %q", strings.Join(ThinkingModes, ", "), thinking)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// anthropicFamily is what one model family accepts, from the Messages API
// documentation (models overview and the effort page, read 2026-09-30).
//
// This is extraction against a fixed schema, so the default is always the
// LEAST thinking a model allows (see requestBody for the measurement behind
// that). What "least" means changed with the 5.5 generation, which is why this
// table exists: `thinking: disabled` is a 400 on Claude Opus 5.5 at every
// effort, and Claude Sonnet 5.5 replaced it with `between_tools`. Sending the
// old value cost a rejected request per photo, then a retry at the model's
// default effort.
type anthropicFamily struct {
	prefix string
	// effort: the model takes output_config.effort.
	effort bool
	// lowest is the thinking value that turns up-front thinking off, or "" when
	// it cannot be turned off (adaptive thinking is always on).
	lowest string
}

// anthropicFamilies is matched longest prefix first, so claude-opus-5-5 is
// never mistaken for claude-opus-5.
var anthropicFamilies = []anthropicFamily{
	{prefix: "claude-sonnet-5-5", effort: true, lowest: "between_tools"},
	{prefix: "claude-opus-5-5", effort: true, lowest: ""},
	{prefix: "claude-fable-5", effort: true, lowest: ""},
	{prefix: "claude-mythos-5", effort: true, lowest: ""},
	{prefix: "claude-sonnet-5", effort: true, lowest: "disabled"},
	{prefix: "claude-opus-5", effort: true, lowest: "disabled"},
	{prefix: "claude-opus-4-8", effort: true, lowest: "disabled"},
	{prefix: "claude-opus-4-7", effort: true, lowest: "disabled"},
	{prefix: "claude-opus-4-6", effort: true, lowest: "disabled"},
	{prefix: "claude-sonnet-4-6", effort: true, lowest: "disabled"},
	{prefix: "claude-haiku-4-5", effort: false, lowest: "disabled"},
}

func familyFor(model string) (anthropicFamily, bool) {
	for _, f := range anthropicFamilies {
		if strings.HasPrefix(model, f.prefix) {
			return f, true
		}
	}
	return anthropicFamily{}, false
}

// anthropicPlan is what one request asks for.
type anthropicPlan struct {
	thinking  map[string]any // nil: leave the field out
	effort    string         // "": leave it out
	maxTokens int
}

// planFor turns a model and the configured tuning into request parameters.
func planFor(model string, t tuning) anthropicPlan {
	fam, known := familyFor(model)
	if !known {
		// An unknown model gets what every model got before this table: the
		// old way of declining thinking, with the retry in identify for a
		// model that rejects it. Effort is passed through if asked for.
		p := anthropicPlan{thinking: map[string]any{"type": "disabled"}, effort: t.effort, maxTokens: anthropicMaxTokens}
		if t.thinking == "adaptive" {
			p.thinking = nil
			p.maxTokens = anthropicMaxTokensThinking
		}
		return p
	}
	p := anthropicPlan{maxTokens: anthropicMaxTokens}
	if fam.effort {
		p.effort = t.effort
	}
	// Neither "off" setting is accepted at the two highest efforts.
	high := t.effort == "xhigh" || t.effort == "max"
	switch {
	case t.thinking == "adaptive":
		if fam.effort {
			p.thinking = map[string]any{"type": "adaptive"}
		} else {
			// No adaptive mode (Haiku 4.5): leaving the field out is its own
			// default, which does not think.
			p.thinking = nil
		}
	case fam.lowest != "" && !high:
		p.thinking = map[string]any{"type": fam.lowest}
	default:
		p.thinking = nil
	}
	// Thinking counts toward max_tokens. Where it can happen, leave room for
	// it on top of a long list of items.
	if (p.thinking == nil && fam.effort) || (p.thinking != nil && p.thinking["type"] == "adaptive") {
		p.maxTokens = anthropicMaxTokensThinking
	}
	return p
}

// Usage is what one identification cost, for a caller that asks for it with
// WithUsage. The server only logs it; the evaluation harness reads it.
type Usage struct {
	Model        string
	InputTokens  int
	OutputTokens int
	// USD is derived from the price table; PriceKnown is false when the model
	// is not in it.
	USD        float64
	PriceKnown bool
	// RetriedWithoutThinking is set when the model rejected the thinking
	// parameter and the call was made again without it.
	RetriedWithoutThinking bool
}

type usageKey struct{}

// WithUsage returns a context under which a provider records what its call
// cost into u.
func WithUsage(ctx context.Context, u *Usage) context.Context {
	return context.WithValue(ctx, usageKey{}, u)
}

func usageFrom(ctx context.Context) *Usage {
	u, _ := ctx.Value(usageKey{}).(*Usage)
	return u
}
