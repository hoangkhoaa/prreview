package watch

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"prreview/internal/ai"
	"prreview/internal/config"
	"prreview/internal/github"
)

// RunHeadless runs the watch loop without a UI, logging to w. It exits
// cleanly on SIGINT/SIGTERM. Used by `prreview watch --daemon`.
func RunHeadless(cfg *config.Config, opts Options, w io.Writer) error {
	if opts.Interval <= 0 {
		opts.Interval = 15 * time.Second
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	logger := log.New(w, "", log.LstdFlags)

	if err := github.CheckGH(); err != nil {
		return err
	}
	user, err := github.CurrentUser()
	if err != nil {
		return err
	}
	seen, err := LoadStore()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scope := "direct requests only"
	if opts.IncludeTeams {
		scope = "direct + team requests"
	}
	if len(opts.Repos) > 0 {
		scope += fmt.Sprintf(", repos: %s", strings.Join(opts.Repos, ", "))
	}
	logger.Printf("watch started: user=%s model=%s depth=%s interval=%s auto-approve=%v concurrency=%d (%s)",
		user, cfg.Model, cfg.Depth, opts.Interval, opts.AutoApprove, opts.Concurrency, scope)

	var (
		mu       sync.Mutex
		inflight = map[string]bool{}
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, opts.Concurrency)

	poll := func() {
		refs, err := github.SearchReviewRequested(opts.IncludeTeams, opts.Repos)
		if err != nil {
			logger.Printf("poll error: %v", err)
			return
		}
		for _, ref := range refs {
			key := ref.Key()
			mu.Lock()
			busy := inflight[key]
			if !busy && !seen.Ignored(key) {
				inflight[key] = true
			}
			mu.Unlock()
			if busy || seen.Ignored(key) {
				continue
			}

			wg.Add(1)
			go func(ref github.PRRef) {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inflight, ref.Key())
					mu.Unlock()
				}()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				reviewOne(ctx, cfg, opts, user, seen, ref, logger)
			}(ref)
		}
	}

	poll()
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Printf("shutting down, waiting for running reviews…")
			wg.Wait()
			logger.Printf("watch stopped")
			return nil
		case <-ticker.C:
			poll()
		}
	}
}

func reviewOne(ctx context.Context, cfg *config.Config, opts Options, user string,
	seen *Store, ref github.PRRef, logger *log.Logger) {

	key := ref.Key()
	logf := func(format string, args ...any) {
		logger.Printf("[%s] %s", key, fmt.Sprintf(format, args...))
	}

	pr, err := github.FetchPR(ref.URL)
	if err != nil {
		logf("fetch failed: %v", err)
		return
	}
	if pr.State != "OPEN" {
		logf("skipped: PR is %s", pr.State)
		seen.Set(key, pr.HeadOid)
		return
	}
	if pr.HeadOid != "" && seen.Get(key) == pr.HeadOid {
		return // already reviewed this head
	}

	logf("reviewing %q by %s (+%d -%d, %d files)", pr.Title, pr.Author, pr.Additions, pr.Deletions, pr.ChangedFiles)
	start := time.Now()

	rctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, err := ai.Analyze(rctx, cfg, pr, func(stage string) { logf("%s", stage) })
	if err != nil {
		if ctx.Err() != nil {
			logf("canceled")
		} else {
			logf("review failed: %v", err)
		}
		return
	}

	state := "COMMENT"
	if len(result.Findings) == 0 && opts.AutoApprove && user != "" && pr.Author != user {
		state = "APPROVE"
	}
	body := result.Summary + "\n\n---\n_Automated review by prreview watch._"
	fallback, err := github.PostReview(pr, state, body, result.Comments())
	if err != nil {
		logf("post failed: %v", err)
		return
	}
	seen.Set(key, pr.HeadOid)
	note := ""
	if fallback {
		note = " (line comments folded into body)"
	}
	logf("posted %s with %d findings in %s%s", state, len(result.Findings), time.Since(start).Round(time.Second), note)
}
