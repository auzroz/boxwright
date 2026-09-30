package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// source is one photo: where it comes from and what it must hash to.
type source struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Page       string `json:"page"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Bytes      int    `json:"bytes"`
	License    string `json:"license"`
	LicenseURL string `json:"licenseUrl"`
	Author     string `json:"author"`
}

// expected is one thing a photo contains, as a person labelled it.
type expected struct {
	Name       string      `json:"name"`
	Match      []string    `json:"match"`
	Categories []string    `json:"categories"`
	Quantity   [2]int      `json:"quantity"`
	Must       bool        `json:"must"`
	Bulky      *bool       `json:"bulky,omitempty"`
	Fragile    *bool       `json:"fragile,omitempty"`
	LongestCm  *[2]float64 `json:"longestCm,omitempty"`
}

type labelledPhoto struct {
	ID    string     `json:"id"`
	Kind  string     `json:"kind"`
	Items []expected `json:"items"`
}

type labels struct {
	Version    int             `json:"version"`
	Categories []string        `json:"categories"`
	Photos     []labelledPhoto `json:"photos"`
}

// config is one way of asking: a model, an effort, a thinking mode.
type config struct {
	Label    string `json:"label"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

type evalSet struct {
	sources map[string]source
	labels  labels
	configs []config
}

func loadSet(dir string) (evalSet, error) {
	var s evalSet
	var src struct {
		Photos []source `json:"photos"`
	}
	if err := readJSON(filepath.Join(dir, "sources.json"), &src); err != nil {
		return s, err
	}
	s.sources = map[string]source{}
	for _, p := range src.Photos {
		s.sources[p.ID] = p
	}
	if err := readJSON(filepath.Join(dir, "labels.json"), &s.labels); err != nil {
		return s, err
	}
	var m struct {
		Configs []config `json:"configs"`
	}
	if err := readJSON(filepath.Join(dir, "matrix.json"), &m); err != nil {
		return s, err
	}
	s.configs = m.Configs
	for _, p := range s.labels.Photos {
		if _, ok := s.sources[p.ID]; !ok {
			return s, fmt.Errorf("labels.json: photo %q is not in sources.json", p.ID)
		}
		for _, it := range p.Items {
			if len(it.Match) == 0 || it.Quantity[0] < 1 || it.Quantity[1] < it.Quantity[0] {
				return s, fmt.Errorf("labels.json: %s/%s needs match keywords and a quantity [min, max]", p.ID, it.Name)
			}
		}
	}
	return s, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// userAgent follows Wikimedia's policy: say who is fetching and why.
const userAgent = "BoxwrightEval/1 (https://github.com/auzroz/boxwright; model evaluation)"

// fetchPhoto returns a photo's bytes from the cache, downloading it first if
// needed, and refuses one whose hash does not match: a changed photo is a
// different test, and results from it would not compare with earlier runs.
func fetchPhoto(ctx context.Context, cacheDir string, s source) ([]byte, error) {
	path := filepath.Join(cacheDir, s.ID+".jpg")
	if b, err := os.ReadFile(path); err == nil && hashOf(b) == s.SHA256 {
		return b, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", s.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", s.ID, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", s.ID, err)
	}
	if got := hashOf(b); got != s.SHA256 {
		return nil, fmt.Errorf("fetch %s: sha256 %s, want %s (the file changed upstream; re-pin it deliberately)", s.ID, got, s.SHA256)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return nil, err
	}
	return b, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
