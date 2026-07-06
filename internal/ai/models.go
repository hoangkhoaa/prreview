package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"prreview/internal/config"
)

// ListModels queries the provider's model listing endpoint (/v1/models for
// both Anthropic and OpenAI-compatible APIs) and returns the available model
// IDs, sorted.
func ListModels(ctx context.Context, cfg *config.Config) ([]string, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("an API key is required to list models")
	}
	client := &http.Client{Timeout: 30 * time.Second}

	var (
		url     string
		headers map[string]string
	)
	switch cfg.Provider {
	case "", "anthropic":
		base := strings.TrimRight(cfg.BaseURL, "/")
		if base == "" {
			base = "https://api.anthropic.com"
		}
		url = base + "/v1/models?limit=100"
		headers = map[string]string{
			"x-api-key":         cfg.APIKey,
			"anthropic-version": "2023-06-01",
		}
	case "openai":
		if cfg.BaseURL == "" {
			return nil, errors.New("base URL is required to list models")
		}
		base := strings.TrimRight(cfg.BaseURL, "/")
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		url = base + "/models"
		headers = map[string]string{"Authorization": "Bearer " + cfg.APIKey}
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("list models: endpoint returned %d: %s", resp.StatusCode, msg)
	}

	// Both APIs use {"data": [{"id": "..."}]}.
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("endpoint returned no models")
	}
	sort.Strings(models)
	return models, nil
}
