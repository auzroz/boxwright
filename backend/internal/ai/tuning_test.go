package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlanForEachModelFamily(t *testing.T) {
	for _, tc := range []struct {
		name         string
		model        string
		tune         tuning
		wantThinking string // "" = field omitted
		wantEffort   string
		wantMax      int
	}{
		// The measured default for extraction: as little thinking as allowed.
		{"opus 5 as today", "claude-opus-5", tuning{}, "disabled", "", anthropicMaxTokens},
		{"sonnet 5 low", "claude-sonnet-5", tuning{effort: "low"}, "disabled", "low", anthropicMaxTokens},
		// 5.5: `disabled` is a 400 on Opus 5.5 at every effort; Sonnet 5.5's
		// lowest is between_tools.
		{"opus 5.5 cannot turn thinking off", "claude-opus-5-5", tuning{effort: "low"}, "", "low", anthropicMaxTokensThinking},
		{"sonnet 5.5 lowest", "claude-sonnet-5-5", tuning{effort: "low"}, "between_tools", "low", anthropicMaxTokens},
		{"sonnet 5.5 adaptive", "claude-sonnet-5-5", tuning{effort: "medium", thinking: "adaptive"}, "adaptive", "medium", anthropicMaxTokensThinking},
		// Neither off setting is accepted at xhigh or max.
		{"sonnet 5.5 at max", "claude-sonnet-5-5", tuning{effort: "max"}, "", "max", anthropicMaxTokensThinking},
		{"opus 5 at xhigh", "claude-opus-5", tuning{effort: "xhigh"}, "", "xhigh", anthropicMaxTokensThinking},
		{"fable 5.1", "claude-fable-5-1", tuning{effort: "low"}, "", "low", anthropicMaxTokensThinking},
		// Haiku 4.5 takes no effort and has no adaptive mode.
		{"haiku ignores effort", "claude-haiku-4-5-20251001", tuning{effort: "low"}, "disabled", "", anthropicMaxTokens},
		{"haiku adaptive is its own default", "claude-haiku-4-5", tuning{thinking: "adaptive"}, "", "", anthropicMaxTokens},
		// Unknown: exactly what every model got before the table.
		{"unknown model", "claude-something-9", tuning{}, "disabled", "", anthropicMaxTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planFor(tc.model, tc.tune)
			got := ""
			if p.thinking != nil {
				got, _ = p.thinking["type"].(string)
			}
			if got != tc.wantThinking {
				t.Errorf("thinking %q, want %q", got, tc.wantThinking)
			}
			if p.effort != tc.wantEffort {
				t.Errorf("effort %q, want %q", p.effort, tc.wantEffort)
			}
			if p.maxTokens != tc.wantMax {
				t.Errorf("max_tokens %d, want %d", p.maxTokens, tc.wantMax)
			}
		})
	}
}

func TestPriceForTakesTheLongestPrefix(t *testing.T) {
	for model, want := range map[string][2]float64{
		"claude-opus-5-5":           {4, 20},
		"claude-opus-5":             {5, 25},
		"claude-opus-5-20260101":    {5, 25},
		"claude-sonnet-5-5":         {2, 10},
		"claude-fable-5-1":          {10, 50},
		"claude-haiku-4-5-20251001": {1, 5},
	} {
		got, ok := priceFor(model)
		if !ok || got != want {
			t.Errorf("%s: %v (%v), want %v", model, got, ok, want)
		}
	}
	if _, ok := priceFor("claude-unknown"); ok {
		t.Error("an unpriced model must say so rather than borrow a price")
	}
}

func TestValidateTuning(t *testing.T) {
	if err := ValidateTuning("low", "adaptive"); err != nil {
		t.Fatal(err)
	}
	if ValidateTuning("lowest", "") == nil || ValidateTuning("", "off") == nil {
		t.Fatal("a typo must fail at startup, not on every photo")
	}
}

// The request carries the plan, and the usage sink sees what was spent.
func TestRequestCarriesEffortAndReportsUsage(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Write([]byte(`{"content":[{"type":"text","text":"[]"}],"stop_reason":"end_turn","usage":{"input_tokens":1000,"output_tokens":100}}`))
	}))
	t.Cleanup(srv.Close)

	id, err := New("anthropic", srv.URL, "k", "claude-sonnet-5-5", WithEffort("low"))
	if err != nil {
		t.Fatal(err)
	}
	var u Usage
	if _, err := id.Identify(WithUsage(context.Background(), &u), []byte{0xff, 0xd8}, "image/jpeg", nil); err != nil {
		t.Fatal(err)
	}
	oc, _ := body["output_config"].(map[string]any)
	if oc["effort"] != "low" {
		t.Errorf("output_config.effort = %v, want low", oc["effort"])
	}
	th, _ := body["thinking"].(map[string]any)
	if th["type"] != "between_tools" {
		t.Errorf("thinking = %v, want between_tools", body["thinking"])
	}
	// $2/M in, $10/M out.
	if u.InputTokens != 1000 || u.OutputTokens != 100 || !u.PriceKnown || u.USD < 0.00299 || u.USD > 0.00301 {
		t.Errorf("usage %+v", u)
	}
}
