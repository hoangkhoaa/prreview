// Package server implements the prreview web UI: a local HTTP server with a
// job engine that reviews PRs (submitted manually or discovered by watch
// polling) and lets the user post the review from the browser.
package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"prreview/internal/ai"
	"prreview/internal/config"
	"prreview/internal/github"
	"prreview/internal/watch"
)

// Job statuses.
const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusAwaiting = "awaiting" // review done, waiting for user decision
	StatusPosted   = "posted"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusSkipped  = "skipped"
)

// Job is one review task.
type Job struct {
	ID         int64        `json:"id"`
	Key        string       `json:"key"` // owner/repo#num
	URL        string       `json:"url"`
	Title      string       `json:"title"`
	Author     string       `json:"author"`
	Source     string       `json:"source"` // manual | watch
	Status     string       `json:"status"`
	Stage      string       `json:"stage"`
	OwnPR      bool         `json:"ownPr"`
	Summary    string       `json:"summary"`
	Verdict    string       `json:"verdict"`
	Findings   []ai.Finding `json:"findings"`
	Candidates int          `json:"candidates"`
	Files      int          `json:"files"`
	Additions  int          `json:"additions"`
	Deletions  int          `json:"deletions"`
	Changed    int          `json:"changedFiles"`
	Posted     string       `json:"posted"`
	Error      string       `json:"error"`
	StartedAt  time.Time    `json:"startedAt"`
	FinishedAt time.Time    `json:"finishedAt"`

	cancel context.CancelFunc
	pr     *github.PR
	result *ai.Result
}

// WatchState is the auto-review poller state.
type WatchState struct {
	Enabled     bool          `json:"enabled"`
	AutoApprove bool          `json:"autoApprove"`
	Interval    time.Duration `json:"-"`
	IntervalSec int           `json:"intervalSec"`
	// IncludeTeams also picks up team review requests; default false =
	// only PRs where the user is directly requested.
	IncludeTeams bool `json:"includeTeams"`
	// Repos whitelists auto-review to these owner/name repos. Empty = all.
	Repos      []string  `json:"repos"`
	LastPollAt time.Time `json:"lastPollAt"`
	LastError  string    `json:"lastError"`
}

// Engine manages review jobs.
type Engine struct {
	mu      sync.Mutex
	base    *config.Config // unresolved config (profiles intact)
	cfg     *config.Config // resolved active config
	jobs    []*Job
	nextID  int64
	sem     chan struct{}
	user    string
	seen    *watch.Store
	watch   WatchState
	watchCh chan struct{} // wakes the poll loop when settings change
	halted  bool          // kill switch: refuse all new work

	repoCache   []string
	repoCacheAt time.Time
}

// NewEngine builds the engine with the resolved default model. An
// unconfigured setup (no API key yet) is allowed — the web UI shows an
// onboarding screen and Submit is rejected until configured.
func NewEngine(base *config.Config, interval time.Duration, concurrency int) (*Engine, error) {
	resolved, err := base.Resolved("")
	if err != nil {
		// e.g. stale default_model — fall back to flat config so the UI
		// can start and let the user fix it in the onboarding screen.
		flat := *base
		resolved = &flat
	}
	seen, err := watch.LoadStore()
	if err != nil {
		return nil, err
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	e := &Engine{
		base: base,
		cfg:  resolved,
		sem:  make(chan struct{}, concurrency),
		seen: seen,
		watch: WatchState{
			Interval:     interval,
			IntervalSec:  int(interval.Seconds()),
			IncludeTeams: base.WatchIncludeTeams,
			Repos:        base.WatchRepos,
		},
		watchCh: make(chan struct{}, 1),
	}
	go func() {
		if err := github.CheckGH(); err == nil {
			if u, err := github.CurrentUser(); err == nil {
				e.mu.Lock()
				e.user = u
				e.mu.Unlock()
			}
		}
	}()
	go e.pollLoop()
	return e, nil
}

// User returns the authenticated GitHub login (may be empty briefly).
func (e *Engine) User() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.user
}

// ActiveConfig returns the resolved config snapshot.
func (e *Engine) ActiveConfig() *config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// BaseConfig returns the stored (unresolved) config.
func (e *Engine) BaseConfig() *config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.base
}

// SetModel switches the default model profile and re-resolves.
func (e *Engine) SetModel(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.base.Models[name]; !ok {
		return fmt.Errorf("unknown model profile %q", name)
	}
	e.base.DefaultModelName = name
	if err := config.Save(e.base); err != nil {
		return err
	}
	resolved, err := e.base.Resolved("")
	if err != nil {
		return err
	}
	e.cfg = resolved
	return nil
}

// SetDepth updates the review depth.
func (e *Engine) SetDepth(depth string) error {
	if depth != "fast" && depth != "standard" && depth != "max" {
		return fmt.Errorf("depth must be fast, standard, or max")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.base.Depth = depth
	if err := config.Save(e.base); err != nil {
		return err
	}
	e.cfg.Depth = depth
	return nil
}

// Watch returns the current watch state.
func (e *Engine) Watch() WatchState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.watch
}

// SetWatch toggles the auto-review poller.
func (e *Engine) SetWatch(enabled, autoApprove bool) {
	e.mu.Lock()
	e.watch.Enabled = enabled
	e.watch.AutoApprove = autoApprove
	e.mu.Unlock()
	select {
	case e.watchCh <- struct{}{}:
	default:
	}
}

// SetWatchFilters updates the review-request scope and repo whitelist,
// persisting them to the config file.
func (e *Engine) SetWatchFilters(includeTeams bool, repos []string) error {
	normalized, err := config.NormalizeRepos(repos)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.watch.IncludeTeams = includeTeams
	e.watch.Repos = normalized
	e.base.WatchIncludeTeams = includeTeams
	e.base.WatchRepos = normalized
	if err := config.Save(e.base); err != nil {
		return err
	}
	select {
	case e.watchCh <- struct{}{}:
	default:
	}
	return nil
}

// DiscoverModels lists the models available at an endpoint. Field/key
// fallback semantics match TestConnection.
func (e *Engine) DiscoverModels(ctx context.Context, name, provider, baseURL, apiKey string) ([]string, error) {
	e.mu.Lock()
	c := *e.cfg
	c.Provider = provider
	c.BaseURL = strings.TrimSpace(baseURL)
	key := strings.TrimSpace(apiKey)
	if key == "" {
		if p, ok := e.base.Models[strings.TrimSpace(name)]; ok && p.APIKey != "" {
			key = p.APIKey
		} else {
			key = e.base.APIKey
		}
	}
	c.APIKey = key
	e.mu.Unlock()

	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ai.ListModels(tctx, &c)
}

// SetupProfiles creates one profile per model ID, all sharing the same
// provider/endpoint/key — for "add several models under one key". The first
// model becomes the default when makeDefault is set (or none exists yet).
func (e *Engine) SetupProfiles(provider, baseURL, apiKey, depth string, models []string, makeDefault bool) error {
	if len(models) == 0 {
		return fmt.Errorf("no models selected")
	}
	for i, m := range models {
		if err := e.SetupProfile(m, provider, baseURL, apiKey, m, depth,
			makeDefault && i == 0); err != nil {
			return err
		}
	}
	return nil
}

// Jobs returns a snapshot of all jobs, newest first.
func (e *Engine) Jobs() []*Job {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Job, len(e.jobs))
	copy(out, e.jobs)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// JobsPage returns one page of jobs, newest first, optionally filtered by
// status ("" or "all" = every status; "active" = queued+running+awaiting).
// Returns the page, the total after filtering, and per-status counts.
func (e *Engine) JobsPage(status string, page, per int) (jobs []*Job, total int, counts map[string]int) {
	all := e.Jobs()
	counts = map[string]int{}
	for _, j := range all {
		counts[j.Status]++
	}
	var filtered []*Job
	for _, j := range all {
		switch status {
		case "", "all":
			filtered = append(filtered, j)
		case "active":
			if j.Status == StatusQueued || j.Status == StatusRunning || j.Status == StatusAwaiting {
				filtered = append(filtered, j)
			}
		default:
			if j.Status == status {
				filtered = append(filtered, j)
			}
		}
	}
	total = len(filtered)
	if per <= 0 {
		per = 10
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * per
	if start >= total {
		return []*Job{}, total, counts
	}
	end := min(start+per, total)
	return filtered[start:end], total, counts
}

func (e *Engine) job(id int64) *Job {
	for _, j := range e.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (e *Engine) findByKey(key string) *Job {
	for _, j := range e.jobs {
		if j.Key == key && j.Status != StatusCanceled && j.Status != StatusFailed {
			return j
		}
	}
	return nil
}

// Configured reports whether reviews can run (an API key and model exist).
func (e *Engine) Configured() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg.APIKey != "" && e.cfg.Model != ""
}

// SetupProfile creates/updates a named model profile from the web UI and
// makes it the default — used for first-run onboarding and "add model".
func (e *Engine) SetupProfile(name, provider, baseURL, apiKey, modelID, depth string, makeDefault bool) error {
	name = strings.TrimSpace(name)
	modelID = strings.TrimSpace(modelID)
	if name == "" || modelID == "" {
		return fmt.Errorf("profile name and model ID are required")
	}
	if provider != "anthropic" && provider != "openai" {
		return fmt.Errorf("provider must be anthropic or openai")
	}
	if provider == "openai" && strings.TrimSpace(baseURL) == "" {
		return fmt.Errorf("base URL is required for OpenAI-compatible endpoints")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.base.Models == nil {
		e.base.Models = map[string]config.ModelProfile{}
	}
	p := config.ModelProfile{
		Provider: provider,
		BaseURL:  strings.TrimSpace(baseURL),
		Model:    modelID,
	}
	if k := strings.TrimSpace(apiKey); k != "" {
		p.APIKey = k
	} else if old, ok := e.base.Models[name]; ok {
		p.APIKey = old.APIKey // keep existing key when editing
	}
	if p.APIKey == "" && e.base.APIKey == "" {
		return fmt.Errorf("an API key is required")
	}
	e.base.Models[name] = p
	if makeDefault || e.base.DefaultModelName == "" {
		e.base.DefaultModelName = name
	}
	if depth == "fast" || depth == "standard" || depth == "max" {
		e.base.Depth = depth
	}
	if err := config.Save(e.base); err != nil {
		return err
	}
	resolved, err := e.base.Resolved("")
	if err != nil {
		return err
	}
	e.cfg = resolved
	return nil
}

// AccessibleRepos lists repos the user can access, cached for 5 minutes.
func (e *Engine) AccessibleRepos() ([]string, error) {
	e.mu.Lock()
	if len(e.repoCache) > 0 && time.Since(e.repoCacheAt) < 5*time.Minute {
		cached := e.repoCache
		e.mu.Unlock()
		return cached, nil
	}
	e.mu.Unlock()

	repos, err := github.ListAccessibleRepos()
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.repoCache = repos
	e.repoCacheAt = time.Now()
	e.mu.Unlock()
	return repos, nil
}

// DeleteProfile removes a model profile. If it was the default, another
// remaining profile (alphabetically first) becomes the default.
func (e *Engine) DeleteProfile(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.base.Models[name]; !ok {
		return fmt.Errorf("unknown model profile %q", name)
	}
	delete(e.base.Models, name)
	if e.base.DefaultModelName == name {
		e.base.DefaultModelName = ""
		names := make([]string, 0, len(e.base.Models))
		for n := range e.base.Models {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) > 0 {
			e.base.DefaultModelName = names[0]
		}
	}
	if err := config.Save(e.base); err != nil {
		return err
	}
	if resolved, err := e.base.Resolved(""); err == nil {
		e.cfg = resolved
	} else {
		flat := *e.base
		e.cfg = &flat
	}
	return nil
}

// TestConnection verifies a model configuration end-to-end with a tiny
// completion. Either name (an existing profile, "" = active default) or
// explicit fields (test-before-save from the setup form) are used. An empty
// apiKey falls back to the stored key of the same-named profile, then the
// global key.
func (e *Engine) TestConnection(ctx context.Context, name, provider, baseURL, apiKey, modelID string) (string, error) {
	e.mu.Lock()
	var cfg *config.Config
	if provider == "" && modelID == "" {
		// test an existing profile / the active config
		resolved, err := e.base.Resolved(name)
		if err != nil {
			e.mu.Unlock()
			return "", err
		}
		cfg = resolved
	} else {
		c := *e.cfg
		c.Provider = provider
		c.BaseURL = strings.TrimSpace(baseURL)
		c.Model = strings.TrimSpace(modelID)
		key := strings.TrimSpace(apiKey)
		if key == "" {
			if p, ok := e.base.Models[strings.TrimSpace(name)]; ok && p.APIKey != "" {
				key = p.APIKey
			} else {
				key = e.base.APIKey
			}
		}
		c.APIKey = key
		cfg = &c
	}
	e.mu.Unlock()

	if cfg.APIKey == "" {
		return "", fmt.Errorf("no API key to test — enter one first")
	}
	if cfg.Model == "" {
		return "", fmt.Errorf("model ID is required")
	}

	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, desc, err := ai.Ping(tctx, cfg)
	if err != nil {
		return "", err
	}
	return desc, nil
}

// Shutdown is the kill switch: it disables watch, cancels every queued and
// running job, and marks the engine halted so no new work (including review
// posting) is accepted. Used right before the server process exits.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	e.halted = true
	e.watch.Enabled = false
	for _, j := range e.jobs {
		if j.cancel != nil && (j.Status == StatusQueued || j.Status == StatusRunning) {
			j.cancel()
		}
	}
	e.mu.Unlock()
	select {
	case e.watchCh <- struct{}{}:
	default:
	}
}

// Purge cancels every queued job and removes queued + finished jobs from the
// list. Running and awaiting jobs are kept (use Shutdown for a hard stop).
// Purged queued watch-jobs are ignored so the next poll doesn't re-queue them.
func (e *Engine) Purge() (removed int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.jobs[:0]
	for _, j := range e.jobs {
		switch j.Status {
		case StatusRunning, StatusAwaiting:
			kept = append(kept, j)
		case StatusQueued:
			if j.cancel != nil {
				j.cancel()
			}
			if j.Source == "watch" {
				e.seen.Ignore(j.Key)
			}
			removed++
		default: // posted, failed, canceled, skipped
			removed++
		}
	}
	e.jobs = kept
	return removed
}

func (e *Engine) isHalted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.halted
}

// Submit queues a review for a PR URL. source is "manual" or "watch".
func (e *Engine) Submit(url, source string) (*Job, error) {
	if e.isHalted() {
		return nil, fmt.Errorf("engine is stopped")
	}
	if !e.Configured() {
		return nil, fmt.Errorf("prreview is not configured yet — complete the setup form first")
	}
	owner, repo, num, err := github.ParsePRURL(url)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s/%s#%d", owner, repo, num)

	e.mu.Lock()
	defer e.mu.Unlock()
	if j := e.findByKey(key); j != nil &&
		(j.Status == StatusQueued || j.Status == StatusRunning) {
		return j, nil // already in flight
	}
	e.nextID++
	j := &Job{
		ID:     e.nextID,
		Key:    key,
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, num),
		Source: source,
		Status: StatusQueued,
		Stage:  "queued",
	}
	e.jobs = append(e.jobs, j)
	go e.runJob(j)
	return j, nil
}

// Cancel aborts a queued/running job.
func (e *Engine) Cancel(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.job(id)
	if j == nil {
		return fmt.Errorf("job %d not found", id)
	}
	if j.cancel != nil && (j.Status == StatusRunning || j.Status == StatusQueued) {
		j.cancel()
	}
	return nil
}

// Remove deletes a job; watch-sourced jobs are also ignored going forward.
func (e *Engine) Remove(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.job(id)
	if j == nil {
		return fmt.Errorf("job %d not found", id)
	}
	if j.cancel != nil && (j.Status == StatusRunning || j.Status == StatusQueued) {
		j.cancel()
	}
	if j.Source == "watch" {
		e.seen.Ignore(j.Key)
	}
	out := e.jobs[:0]
	for _, x := range e.jobs {
		if x.ID != id {
			out = append(out, x)
		}
	}
	e.jobs = out
	return nil
}

// Retry re-runs a finished job.
func (e *Engine) Retry(id int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.job(id)
	if j == nil {
		return fmt.Errorf("job %d not found", id)
	}
	switch j.Status {
	case StatusQueued, StatusRunning:
		return fmt.Errorf("job is already in progress")
	}
	e.seen.Clear(j.Key)
	j.Status = StatusQueued
	j.Stage = "queued"
	j.Error = ""
	j.Posted = ""
	j.Findings = nil
	j.Summary = ""
	go e.runJob(j)
	return nil
}

// Post submits the review of an awaiting job to GitHub.
func (e *Engine) Post(id int64, state string) error {
	if e.isHalted() {
		return fmt.Errorf("engine is stopped")
	}
	switch state {
	case "APPROVE", "COMMENT", "REQUEST_CHANGES":
	default:
		return fmt.Errorf("invalid review state %q", state)
	}

	e.mu.Lock()
	j := e.job(id)
	if j == nil || j.Status != StatusAwaiting || j.pr == nil || j.result == nil {
		e.mu.Unlock()
		return fmt.Errorf("job is not awaiting a decision")
	}
	if j.OwnPR && state != "COMMENT" {
		e.mu.Unlock()
		return fmt.Errorf("you can't approve or request changes on your own PR — post a comment instead")
	}
	pr, result := j.pr, j.result
	j.Stage = "posting review…"
	e.mu.Unlock()

	body := reviewBody(result, state)
	comments := result.Comments()
	if state == "APPROVE" {
		comments = nil
	}
	fallback, err := github.PostReview(pr, state, body, comments)

	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		j.Stage = ""
		return err
	}
	j.Status = StatusPosted
	j.Posted = state
	j.Stage = ""
	if fallback {
		j.Stage = "line comments folded into body"
	}
	j.FinishedAt = time.Now()
	e.seen.Set(j.Key, pr.HeadOid)
	return nil
}

func reviewBody(result *ai.Result, state string) string {
	body := strings.TrimSpace(result.Summary)
	if body == "" && state == "APPROVE" {
		body = "LGTM."
	}
	return body + "\n\n---\n_Reviewed by AI._"
}

// runJob executes the review pipeline for a job.
func (e *Engine) runJob(j *Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	e.mu.Lock()
	j.cancel = cancel
	cfg := e.cfg
	user := e.user
	autoApprove := e.watch.AutoApprove
	e.mu.Unlock()

	e.sem <- struct{}{}
	defer func() { <-e.sem }()
	if ctx.Err() != nil {
		e.finishCanceled(j)
		return
	}

	e.setJob(j, func() {
		j.Status = StatusRunning
		j.Stage = "fetching PR…"
		j.StartedAt = time.Now()
	})

	pr, err := github.FetchPR(j.URL)
	if err != nil {
		e.fail(j, ctx, err)
		return
	}
	e.setJob(j, func() {
		j.pr = pr
		j.Title = pr.Title
		j.Author = pr.Author
		j.OwnPR = user != "" && pr.Author == user
		j.Additions, j.Deletions, j.Changed = pr.Additions, pr.Deletions, pr.ChangedFiles
	})
	if pr.State != "OPEN" {
		e.setJob(j, func() {
			j.Status = StatusSkipped
			j.Stage = "PR is " + strings.ToLower(pr.State)
			j.FinishedAt = time.Now()
		})
		return
	}
	if j.Source == "watch" && pr.HeadOid != "" && e.seen.Get(j.Key) == pr.HeadOid {
		e.setJob(j, func() {
			j.Status = StatusSkipped
			j.Stage = "already reviewed this revision"
			j.FinishedAt = time.Now()
		})
		return
	}

	result, err := ai.Analyze(ctx, cfg, pr, func(stage string) {
		e.setJob(j, func() { j.Stage = stage })
	})
	if err != nil {
		e.fail(j, ctx, err)
		return
	}

	e.setJob(j, func() {
		j.result = result
		j.Summary = result.Summary
		j.Verdict = result.Verdict
		j.Findings = result.Findings
		j.Candidates = result.Candidates
		j.Files = result.FilesFetched
	})

	if e.isHalted() {
		e.finishCanceled(j)
		return
	}

	if j.Source == "watch" {
		// Auto-post like the daemon: COMMENT, or APPROVE when clean + opted in.
		state := "COMMENT"
		if len(result.Findings) == 0 && autoApprove && user != "" && pr.Author != user {
			state = "APPROVE"
		}
		e.setJob(j, func() { j.Stage = "posting review…" })
		body := result.Summary + "\n\n---\n_Automated review by prreview watch._"
		if _, err := github.PostReview(pr, state, body, result.Comments()); err != nil {
			e.fail(j, ctx, err)
			return
		}
		e.setJob(j, func() {
			j.Status = StatusPosted
			j.Posted = state
			j.Stage = ""
			j.FinishedAt = time.Now()
		})
		e.seen.Set(j.Key, pr.HeadOid)
		return
	}

	e.setJob(j, func() {
		j.Status = StatusAwaiting
		j.Stage = ""
		j.FinishedAt = time.Now()
	})
}

func (e *Engine) setJob(j *Job, fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn()
}

func (e *Engine) fail(j *Job, ctx context.Context, err error) {
	if ctx.Err() != nil {
		e.finishCanceled(j)
		return
	}
	e.setJob(j, func() {
		j.Status = StatusFailed
		j.Error = err.Error()
		j.Stage = ""
		j.FinishedAt = time.Now()
	})
}

func (e *Engine) finishCanceled(j *Job) {
	e.setJob(j, func() {
		j.Status = StatusCanceled
		j.Stage = ""
		j.FinishedAt = time.Now()
	})
}

// pollLoop discovers PRs with review requested while watch is enabled.
func (e *Engine) pollLoop() {
	for {
		e.mu.Lock()
		enabled := e.watch.Enabled
		interval := e.watch.Interval
		e.mu.Unlock()

		if !enabled {
			<-e.watchCh
			continue
		}

		e.mu.Lock()
		includeTeams := e.watch.IncludeTeams
		repos := append([]string(nil), e.watch.Repos...)
		e.mu.Unlock()
		refs, err := github.SearchReviewRequested(includeTeams, repos)
		e.mu.Lock()
		e.watch.LastPollAt = time.Now()
		if err != nil {
			e.watch.LastError = err.Error()
		} else {
			e.watch.LastError = ""
		}
		e.mu.Unlock()

		if err == nil {
			for _, ref := range refs {
				key := ref.Key()
				e.mu.Lock()
				exists := e.findByKey(key) != nil
				ignored := e.seen.Ignored(key)
				e.mu.Unlock()
				if exists || ignored {
					continue
				}
				_, _ = e.Submit(ref.URL, "watch")
			}
		}

		select {
		case <-time.After(interval):
		case <-e.watchCh:
		}
	}
}
