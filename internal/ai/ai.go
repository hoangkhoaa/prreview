// Package ai analyzes a PR with a multi-stage pipeline modeled on commercial
// review bots: full-file context -> parallel specialized finders
// (coverage-first) -> dedupe -> adversarial verification (precision) ->
// line-snapping against the diff -> summary.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"prreview/internal/config"
	"prreview/internal/github"
)

// Finding is one issue found in the diff.
type Finding struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Severity   string `json:"severity"` // critical | major | minor | nit
	Confidence int    `json:"confidence"`
	Comment    string `json:"comment"`
}

// Result is the structured output of a review.
type Result struct {
	Summary  string    `json:"summary"`
	Verdict  string    `json:"verdict"` // approve | request_changes
	Findings []Finding `json:"findings"`
	// Stats for display.
	Candidates   int `json:"-"`
	FilesFetched int `json:"-"`
}

// lens is one specialized finder perspective.
type lens struct {
	name   string
	prompt string
}

var allLenses = []lens{
	{"correctness", `Focus exclusively on CORRECTNESS: logic errors, off-by-one, inverted conditions, wrong operators, nil/null dereferences, unhandled error paths, incorrect state updates, broken control flow, wrong variable used, copy-paste mistakes.`},
	{"security", `Focus exclusively on SECURITY: hardcoded secrets/credentials/tokens, injection (SQL, command, path traversal), missing authn/authz checks, unsafe deserialization, sensitive data in logs, weak crypto, SSRF, unvalidated redirects.`},
	{"resilience", `Focus exclusively on API MISUSE & RESILIENCE: misused library/framework APIs, race conditions and concurrency bugs, resource leaks (unclosed files/connections/goroutines), missing timeouts, edge cases (empty input, unicode, overflow, boundary values), error swallowing.`},
	{"hygiene", `Focus exclusively on CHANGE HYGIENE: leftover debug code (print/console.log/debugger), TODO/FIXME that must be resolved, missing tests for new functionality, breaking API/schema changes without migration, dead code, misleading names/comments introduced by this change.`},
}

var combinedLens = lens{"combined", `Look for: logic bugs, security issues (secrets, injection, authz), API misuse, concurrency bugs, resource leaks, leftover debug code, missing tests, breaking changes.`}

// depthProfile controls how much work the pipeline does.
type depthProfile struct {
	lenses  []lens
	verify  bool
	effort  string
	limits  github.FileFetchLimits
	findTok int
}

func profileFor(cfg *config.Config) depthProfile {
	switch cfg.Depth {
	case "fast":
		return depthProfile{
			lenses: []lens{combinedLens},
			verify: false,
			effort: "medium",
			limits: github.FileFetchLimits{MaxFiles: 10, PerFileBytes: 20_000, TotalBytes: 80_000},
			findTok: 6000,
		}
	case "standard":
		return depthProfile{
			lenses: []lens{allLenses[0], {"security+hygiene", allLenses[1].prompt + "\nAlso: " + allLenses[3].prompt}},
			verify: true,
			effort: "high",
			limits: github.FileFetchLimits{MaxFiles: 20, PerFileBytes: 30_000, TotalBytes: 120_000},
			findTok: 8000,
		}
	default: // "max"
		return depthProfile{
			lenses: allLenses,
			verify: true,
			effort: "xhigh",
			limits: github.FileFetchLimits{MaxFiles: 30, PerFileBytes: 48_000, TotalBytes: 200_000},
			findTok: 8000,
		}
	}
}

const maxDiffChars = 180_000

const finderSystem = `You are one specialized reviewer in a multi-agent code review pipeline for GitHub pull requests. Other reviewers cover other concern areas; a downstream verification step filters false positives.

Your goal at this stage is COVERAGE, not filtering: report every issue you find in your assigned focus area, including ones you are uncertain about. It is better to surface a finding that later gets filtered than to silently drop a real bug. Do NOT report:
- issues in code that this PR does not touch (pre-existing problems),
- pure style/formatting preferences,
- speculative "might be nice" suggestions.

Rules for findings:
- "path" must be a file path exactly as it appears in the diff.
- "line" must be a line number in the NEW version of the file, inside the diff hunks (added or context line). Prefer added lines.
- "severity": "critical" (bug/secret that must not merge), "major" (should fix before merge), "minor" (worth fixing), "nit" (trivial).
- "confidence": 0-100, your honest probability the issue is real.
- Each comment: concise, actionable, one issue per finding. Reference concrete identifiers from the code.

Respond with ONLY a JSON object, no markdown fences:
{"findings": [{"path": "...", "line": 123, "severity": "...", "confidence": 85, "comment": "..."}]}
If nothing is found, respond {"findings": []}.`

const verifierSystem = `You are an adversarial verification reviewer in a code review pipeline. You receive candidate findings produced by earlier reviewers, plus the full PR context. Your job is to KILL false positives: for each candidate, actively try to refute it using the code.

Reject a candidate if:
- the "bug" is actually handled elsewhere in the visible code,
- it misreads the diff (e.g. refers to deleted code, or code the PR doesn't change),
- it's speculative and the code gives no concrete evidence,
- it's a duplicate of another candidate (keep the better one, reject the rest),
- it's a pure style preference.

Confirm only findings a strong human reviewer would stand behind in a real PR review. You may adjust severity and rewrite the comment to be sharper and reference concrete evidence.

Respond with ONLY a JSON object, no markdown fences:
{"verdicts": [{"id": 0, "verdict": "confirmed", "severity": "major", "comment": "improved comment"}, {"id": 1, "verdict": "rejected"}]}
Every candidate id you were given MUST appear exactly once.`

const summarySystem = `You write the summary body of a GitHub pull request review. Given PR context and the final list of confirmed findings, write 2-5 sentences of markdown: what the PR does, overall assessment, and the key risks (reference the most severe findings briefly). No headings, no lists, no preamble. Respond with ONLY the summary text.`

// Analyze runs the full review pipeline. progress (optional) receives short
// human-readable stage updates.
func Analyze(ctx context.Context, cfg *config.Config, pr *github.PR, progress func(string)) (*Result, error) {
	if progress == nil {
		progress = func(string) {}
	}
	prof := profileFor(cfg)
	effCfg := *cfg
	if cfg.Effort == "" || cfg.Effort == config.DefaultEffort {
		// Depth profile picks the effort unless the user set one explicitly.
		effCfg.Effort = prof.effort
	}
	client, err := newLLM(&effCfg)
	if err != nil {
		return nil, err
	}

	// Stage 1: context.
	idx := parseDiff(pr.Diff)
	progress(fmt.Sprintf("fetching %d changed files for context…", len(idx.paths())))
	files := github.FetchFiles(pr, idx.paths(), prof.limits)
	shared := buildContext(pr, files)

	// Stage 2: parallel finders (coverage-first).
	progress(fmt.Sprintf("scanning with %d specialized reviewers…", len(prof.lenses)))
	candidates, err := runFinders(ctx, client, prof, shared)
	if err != nil {
		return nil, err
	}

	// Stage 3: dedupe + line snapping.
	candidates = dedupe(idx, candidates)
	numCandidates := len(candidates)

	// Stage 4: adversarial verification.
	if prof.verify && len(candidates) > 0 {
		progress(fmt.Sprintf("verifying %d candidate findings…", len(candidates)))
		candidates, err = runVerifiers(ctx, client, shared, candidates)
		if err != nil {
			return nil, err
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return severityRank(candidates[i].Severity) < severityRank(candidates[j].Severity)
	})

	// Stage 5: summary + verdict.
	progress("writing summary…")
	summary := summarize(ctx, client, shared, pr, candidates)

	verdict := "approve"
	for _, f := range candidates {
		if f.Severity == "critical" || f.Severity == "major" {
			verdict = "request_changes"
			break
		}
	}

	return &Result{
		Summary:      summary,
		Verdict:      verdict,
		Findings:     candidates,
		Candidates:   numCandidates,
		FilesFetched: len(files),
	}, nil
}

func buildContext(pr *github.PR, files map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Pull request context\n\n")
	fmt.Fprintf(&b, "PR: %s (#%d)\nRepository: %s/%s\nAuthor: %s | Branch: %s -> %s\n",
		pr.Title, pr.Number, pr.Owner, pr.Repo, pr.Author, pr.HeadRef, pr.BaseRef)
	fmt.Fprintf(&b, "Stats: +%d -%d across %d files\n\n", pr.Additions, pr.Deletions, pr.ChangedFiles)

	if len(files) > 0 {
		b.WriteString("## Full content of changed files (head revision)\n\n")
		paths := make([]string, 0, len(files))
		for p := range files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&b, "### %s\n```\n%s\n```\n\n", p, files[p])
		}
	}

	diff := pr.Diff
	if len(diff) > maxDiffChars {
		diff = diff[:maxDiffChars] + "\n... (diff truncated)\n"
	}
	b.WriteString("## Diff\n\n")
	b.WriteString(diff)
	return b.String()
}

func runFinders(ctx context.Context, client llm, prof depthProfile, shared string) ([]Finding, error) {
	type out struct {
		findings []Finding
		err      error
	}
	results := make([]out, len(prof.lenses))
	var wg sync.WaitGroup
	for i, l := range prof.lenses {
		wg.Add(1)
		go func(i int, l lens) {
			defer wg.Done()
			text, err := client.complete(ctx, llmRequest{
				system:        finderSystem,
				sharedContext: shared,
				instruction:   "Your assigned focus area:\n" + l.prompt + "\n\nReview the pull request now and return the JSON.",
				maxTokens:     prof.findTok,
			})
			if err != nil {
				results[i] = out{err: fmt.Errorf("%s reviewer: %w", l.name, err)}
				return
			}
			var parsed struct {
				Findings []Finding `json:"findings"`
			}
			if err := parseJSON(text, &parsed); err != nil {
				results[i] = out{err: fmt.Errorf("%s reviewer: %w", l.name, err)}
				return
			}
			results[i] = out{findings: parsed.Findings}
		}(i, l)
	}
	wg.Wait()

	var all []Finding
	var firstErr error
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		all = append(all, r.findings...)
	}
	// Tolerate partial lens failures, but not total failure.
	if failed == len(results) && firstErr != nil {
		return nil, firstErr
	}
	return all, nil
}

// dedupe snaps lines to the diff, drops findings that can't be anchored
// confidently, and merges near-duplicates (same file within 2 lines),
// keeping the most severe / most confident one.
func dedupe(idx *diffIndex, findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		f.Severity = normalizeSeverity(f.Severity)
		f.Line = idx.snap(f.Path, f.Line)
		if f.Path == "" || idx.files[f.Path] == nil {
			continue
		}
		dup := false
		for i := range out {
			if out[i].Path == f.Path && abs(out[i].Line-f.Line) <= 2 && f.Line > 0 {
				dup = true
				if severityRank(f.Severity) < severityRank(out[i].Severity) ||
					(severityRank(f.Severity) == severityRank(out[i].Severity) && f.Confidence > out[i].Confidence) {
					out[i] = f
				}
				break
			}
		}
		if !dup {
			out = append(out, f)
		}
	}
	return out
}

const verifyBatchSize = 8

func runVerifiers(ctx context.Context, client llm, shared string, candidates []Finding) ([]Finding, error) {
	type batchResult struct {
		kept []Finding
		err  error
	}
	var batches [][]Finding
	for i := 0; i < len(candidates); i += verifyBatchSize {
		end := min(i+verifyBatchSize, len(candidates))
		batches = append(batches, candidates[i:end])
	}

	results := make([]batchResult, len(batches))
	var wg sync.WaitGroup
	for bi, batch := range batches {
		wg.Add(1)
		go func(bi int, batch []Finding) {
			defer wg.Done()
			var b strings.Builder
			b.WriteString("Candidate findings to verify:\n\n")
			for i, f := range batch {
				fmt.Fprintf(&b, "id=%d  %s:%d  [%s, confidence %d]\n  %s\n\n",
					i, f.Path, f.Line, f.Severity, f.Confidence, f.Comment)
			}
			b.WriteString("Verify each candidate against the PR context and return the JSON verdicts.")

			text, err := client.complete(ctx, llmRequest{
				system:        verifierSystem,
				sharedContext: shared,
				instruction:   b.String(),
				maxTokens:     6000,
			})
			if err != nil {
				results[bi] = batchResult{err: err}
				return
			}
			var parsed struct {
				Verdicts []struct {
					ID       int    `json:"id"`
					Verdict  string `json:"verdict"`
					Severity string `json:"severity"`
					Comment  string `json:"comment"`
				} `json:"verdicts"`
			}
			if err := parseJSON(text, &parsed); err != nil {
				results[bi] = batchResult{err: err}
				return
			}
			var kept []Finding
			for _, v := range parsed.Verdicts {
				if v.Verdict != "confirmed" || v.ID < 0 || v.ID >= len(batch) {
					continue
				}
				f := batch[v.ID]
				if v.Severity != "" {
					f.Severity = normalizeSeverity(v.Severity)
				}
				if v.Comment != "" {
					f.Comment = v.Comment
				}
				kept = append(kept, f)
			}
			results[bi] = batchResult{kept: kept}
		}(bi, batch)
	}
	wg.Wait()

	var out []Finding
	for bi, r := range results {
		if r.err != nil {
			// Verification failed for this batch: keep high-confidence
			// candidates rather than dropping possible real bugs.
			for _, f := range batches[bi] {
				if f.Confidence >= 70 {
					out = append(out, f)
				}
			}
			continue
		}
		out = append(out, r.kept...)
	}
	return out, nil
}

func summarize(ctx context.Context, client llm, shared string, pr *github.PR, findings []Finding) string {
	var b strings.Builder
	b.WriteString("Confirmed findings:\n")
	if len(findings) == 0 {
		b.WriteString("(none — the diff looks clean)\n")
	}
	for _, f := range findings {
		fmt.Fprintf(&b, "- %s:%d %s **[%s]** %s\n", f.Path, f.Line, severityEmoji(f.Severity), f.Severity, f.Comment)
	}
	b.WriteString("\nWrite the review summary now.")

	text, err := client.complete(ctx, llmRequest{
		system:        summarySystem,
		sharedContext: shared,
		instruction:   b.String(),
		maxTokens:     1500,
	})
	if err != nil || strings.TrimSpace(text) == "" {
		if len(findings) == 0 {
			return fmt.Sprintf("Reviewed #%d — no issues found in the diff.", pr.Number)
		}
		return fmt.Sprintf("Reviewed #%d — %d finding(s); see line comments.", pr.Number, len(findings))
	}
	return strings.TrimSpace(text)
}

// --- helpers ---

// parseJSON extracts a JSON object from model output, tolerating prose or
// markdown fences around it.
func parseJSON(s string, v any) error {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return fmt.Errorf("model did not return JSON: %.200s", s)
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), v); err != nil {
		return fmt.Errorf("parse model JSON: %w", err)
	}
	return nil
}

// severityEmoji maps severity names to colored, fun emojis for display.
// critical = red, major = orange, minor = yellow/blue, nit = gray/green.
func severityEmoji(s string) string {
	switch s {
	case "critical":
		return "🔴"
	case "major":
		return "🟠"
	case "minor":
		return "🟡"
	default:
		return "🟢"
	}
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return "critical"
	case "major", "high":
		return "major"
	case "minor", "medium", "low":
		return "minor"
	default:
		return "nit"
	}
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "major":
		return 1
	case "minor":
		return 2
	default:
		return 3
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// HasBlockingFindings reports whether any finding is critical or major.
func (r *Result) HasBlockingFindings() bool {
	for _, f := range r.Findings {
		if f.Severity == "critical" || f.Severity == "major" {
			return true
		}
	}
	return false
}

// Comments converts findings into GitHub line-level review comments.
// Findings that could not be anchored to a diff line (Line == 0) are skipped
// here; they still appear in the review body via the summary fallback.
func (r *Result) Comments() []github.ReviewComment {
	out := make([]github.ReviewComment, 0, len(r.Findings))
	for _, f := range r.Findings {
		if f.Path == "" || f.Line <= 0 {
			continue
		}
		out = append(out, github.ReviewComment{
			Path: f.Path,
			Line: f.Line,
			Side: "RIGHT",
			Body: fmt.Sprintf("%s **[%s]** %s", severityEmoji(f.Severity), f.Severity, f.Comment),
		})
	}
	return out
}
