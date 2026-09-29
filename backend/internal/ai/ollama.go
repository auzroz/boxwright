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

// ollama speaks the native Ollama /api/chat format with inline images.
// Suggested local models: gemma3:4b (easiest), qwen-vl variants (best OCR),
// moondream (small hardware). See docs/ARCHITECTURE.md.
type ollama struct {
	baseURL string // e.g. http://ollama:11434
	model   string
}

func (p *ollama) Identify(ctx context.Context, image []byte, mimeType string, categories []placement.Category) ([]placement.ItemDraft, error) {
	reqBody := map[string]any{
		"model":  p.model,
		"stream": false,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": identifyPrompt(categories),
				"images":  []string{base64.StdEncoding.EncodeToString(image)},
			},
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Local models on CPU can be slow; allow a generous timeout.
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("ollama request: status %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("ollama decode: %w", err)
	}
	return parseDrafts(out.Message.Content)
}
