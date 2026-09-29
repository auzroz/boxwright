package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"boxwright/internal/placement"
)

// openAICompat speaks the OpenAI chat-completions vision format, which is
// also served by many local gateways (LiteLLM, llama.cpp server, vLLM,
// OpenRouter, LM Studio).
type openAICompat struct {
	baseURL string // e.g. https://api.openai.com/v1
	apiKey  string
	model   string
}

func (p *openAICompat) Identify(ctx context.Context, image []byte, mimeType string, categories []placement.Category) ([]placement.ItemDraft, error) {
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(image))

	reqBody := map[string]any{
		"model": p.model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": identifyPrompt(categories)},
					{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
				},
			},
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("openai-compatible request: status %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openai-compatible decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("openai-compatible: empty choices")
	}
	return parseDrafts(out.Choices[0].Message.Content)
}
