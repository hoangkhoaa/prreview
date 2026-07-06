package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultModel     = "claude-opus-4-8"
	DefaultMaxTokens = 16000
	DefaultEffort    = "high"
	DefaultDepth     = "max"
)

// ModelProfile is one named model configuration; the active profile's fields
// override the top-level ones.
type ModelProfile struct {
	// Provider is "anthropic" or "openai" (OpenAI-compatible endpoints:
	// vLLM, Ollama, LiteLLM, DeepSeek, ...).
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url,omitempty"`
	// APIKey may be empty to reuse the top-level api_key.
	APIKey string `json:"api_key,omitempty"`
	Model  string `json:"model"`
}

// Config holds user configuration, stored at ~/.config/prreview/config.json.
type Config struct {
	APIKey string `json:"api_key"`
	// Provider is "anthropic" (default) or "openai" for OpenAI-compatible
	// self-hosted endpoints (vLLM, Ollama, LiteLLM, ...).
	Provider string `json:"provider,omitempty"`
	// BaseURL overrides the API endpoint, e.g. https://llm.internal.example.com
	BaseURL   string `json:"base_url,omitempty"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// Models are named model profiles; DefaultModelName selects the active one.
	Models           map[string]ModelProfile `json:"models,omitempty"`
	DefaultModelName string                  `json:"default_model,omitempty"`
	// Effort controls reasoning depth on Anthropic models: low|medium|high|xhigh|max.
	Effort string `json:"effort,omitempty"`
	// Depth controls review intelligence vs cost: fast | standard | max.
	Depth string `json:"depth,omitempty"`
	// SkipVerify disables the adversarial verification pass (faster, more
	// false positives).
	SkipVerify bool `json:"skip_verify,omitempty"`
	// WatchIncludeTeams also auto-reviews PRs where a team you belong to is
	// requested. Default false: only direct (user) review requests.
	WatchIncludeTeams bool `json:"watch_include_teams,omitempty"`
	// WatchRepos limits auto-watch to these repos (owner/name). Empty = all.
	WatchRepos []string `json:"watch_repos,omitempty"`

	envKey string // API key from environment, wins over profiles
}

// NormalizeRepos trims, dedupes, and validates owner/name entries.
func NormalizeRepos(repos []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, r := range repos {
		r = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(r, "https://"), "github.com/"))
		r = strings.Trim(r, "/")
		if r == "" || seen[r] {
			continue
		}
		parts := strings.Split(r, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid repo %q — expected owner/name", r)
		}
		seen[r] = true
		out = append(out, r)
	}
	return out, nil
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "prreview", "config.json"), nil
}

// Load reads the config file. Env vars PRREVIEW_API_KEY / ANTHROPIC_API_KEY
// override the stored API key; PRREVIEW_BASE_URL overrides the endpoint.
func Load() (*Config, error) {
	cfg := &Config{}

	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err == nil {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("invalid config file %s: %w", p, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if v := os.Getenv("PRREVIEW_API_KEY"); v != "" {
		cfg.APIKey = v
		cfg.envKey = v
	} else if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" && cfg.APIKey == "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("PRREVIEW_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}

	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = DefaultMaxTokens
	}
	if cfg.Effort == "" {
		cfg.Effort = DefaultEffort
	}
	if cfg.Depth == "" {
		cfg.Depth = DefaultDepth
	}
	return cfg, nil
}

func Save(cfg *Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o600)
}

// Resolved returns a copy of the config with the named model profile applied
// to the flat fields. name == "" uses DefaultModelName (or the sole profile
// if exactly one exists). Env-provided API keys always win. The returned
// config is for running reviews — never Save it, or resolved values would
// overwrite the stored base config.
func (c *Config) Resolved(name string) (*Config, error) {
	out := *c
	if name == "" {
		name = c.DefaultModelName
	}
	if name == "" && len(c.Models) == 1 {
		for n := range c.Models {
			name = n
		}
	}
	if name != "" {
		p, ok := c.Models[name]
		if !ok {
			return nil, fmt.Errorf("unknown model profile %q — see: prreview model list", name)
		}
		if p.Provider != "" {
			out.Provider = p.Provider
		}
		out.BaseURL = p.BaseURL
		if p.Model != "" {
			out.Model = p.Model
		}
		if p.APIKey != "" {
			out.APIKey = p.APIKey
		}
	}
	if c.envKey != "" {
		out.APIKey = c.envKey
	}
	return &out, nil
}

// MaskKey returns the API key with the middle hidden, for display.
func MaskKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 12 {
		return strings.Repeat("*", len(key))
	}
	return key[:8] + "..." + key[len(key)-4:]
}
