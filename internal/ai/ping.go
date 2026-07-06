package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"prreview/internal/config"
)

// Ping sends a minimal completion to verify the API key, endpoint, and model
// are all working. Returns the round-trip latency and a short description.
func Ping(ctx context.Context, cfg *config.Config) (time.Duration, string, error) {
	client, err := newLLM(cfg)
	if err != nil {
		return 0, "", err
	}
	start := time.Now()
	out, err := client.complete(ctx, llmRequest{
		system:      "You are a connectivity check.",
		instruction: "Reply with exactly: OK",
		maxTokens:   64,
	})
	elapsed := time.Since(start)
	if err != nil {
		return elapsed, "", err
	}
	reply := strings.TrimSpace(out)
	if len(reply) > 40 {
		reply = reply[:40] + "…"
	}
	desc := fmt.Sprintf("%s responded in %s", cfg.Model, elapsed.Round(time.Millisecond))
	if reply != "" {
		desc += fmt.Sprintf(" (%q)", reply)
	}
	return elapsed, desc, nil
}
