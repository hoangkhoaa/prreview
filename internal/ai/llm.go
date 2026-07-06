package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"prreview/internal/config"
)

// llmRequest is one completion call. sharedContext is the large, stable
// context (PR metadata + files + diff) reused across pipeline stages — the
// Anthropic client marks it for prompt caching so verify/summary calls read
// the cache written by the finder calls.
type llmRequest struct {
	system        string
	sharedContext string
	instruction   string
	maxTokens     int
}

type llm interface {
	complete(ctx context.Context, req llmRequest) (string, error)
}

func newLLM(cfg *config.Config) (llm, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("no API key configured — run: prreview config")
	}
	switch cfg.Provider {
	case "", "anthropic":
		return newAnthropicLLM(cfg), nil
	case "openai":
		if cfg.BaseURL == "" {
			return nil, errors.New(`provider "openai" needs base_url — run: prreview config set base_url <url>`)
		}
		return newOpenAILLM(cfg), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (anthropic | openai)", cfg.Provider)
	}
}

// --- Anthropic ---

type anthropicLLM struct {
	client anthropic.Client
	cfg    *config.Config
}

func newAnthropicLLM(cfg *config.Config) *anthropicLLM {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &anthropicLLM{client: anthropic.NewClient(opts...), cfg: cfg}
}

func (a *anthropicLLM) complete(ctx context.Context, req llmRequest) (string, error) {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	system := []anthropic.TextBlockParam{{Text: req.system}}
	if req.sharedContext != "" {
		system = append(system, anthropic.TextBlockParam{
			Text:         req.sharedContext,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		})
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.cfg.Model),
		MaxTokens: int64(req.maxTokens),
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		System:    system,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.instruction)),
		},
	}
	if a.cfg.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffort(a.cfg.Effort),
		}
	}

	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		var apierr *anthropic.Error
		if errors.As(err, &apierr) {
			switch apierr.StatusCode {
			case 401:
				return "", errors.New("Anthropic API: invalid API key — run: prreview config")
			case 429:
				return "", errors.New("Anthropic API: rate limited, try again shortly")
			case 404:
				return "", fmt.Errorf("Anthropic API: unknown model %q — run: prreview config", a.cfg.Model)
			}
		}
		return "", fmt.Errorf("Anthropic API: %w", err)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	return text.String(), nil
}

// --- OpenAI-compatible (self-hosted: vLLM, Ollama, LiteLLM, ...) ---

type openaiLLM struct {
	cfg    *config.Config
	base   string
	client *http.Client
}

func newOpenAILLM(cfg *config.Config) *openaiLLM {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return &openaiLLM{
		cfg:    cfg,
		base:   base,
		client: &http.Client{Timeout: 10 * time.Minute},
	}
}

func (o *openaiLLM) complete(ctx context.Context, req llmRequest) (string, error) {
	system := req.system
	if req.sharedContext != "" {
		system += "\n\n" + req.sharedContext
	}
	payload := map[string]any{
		"model": o.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": req.instruction},
		},
		"max_tokens":  req.maxTokens,
		"temperature": 0.2,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("LLM endpoint %s: %w", o.base, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("LLM endpoint returned %d: %s", resp.StatusCode, msg)
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("parse LLM response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("LLM endpoint returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}
