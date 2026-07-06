// Package github wraps the gh CLI for fetching PRs and posting reviews,
// mirroring the fetch_pr.py / post_review.py scripts of the
// github-pr-review-approve skill.
package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// PR holds metadata and the diff of a pull request.
type PR struct {
	Owner        string
	Repo         string
	Number       int
	Title        string
	Author       string
	State        string
	BaseRef      string
	HeadRef      string
	HeadOid      string
	URL          string
	Additions    int
	Deletions    int
	ChangedFiles int
	Diff         string
}

// ReviewComment is a line-level review comment in the GitHub API shape.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

var prURLRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// ParsePRURL extracts owner, repo and number from a GitHub PR URL.
// Trailing segments like /files, /commits, /checks are tolerated.
func ParsePRURL(raw string) (owner, repo string, number int, err error) {
	raw = strings.TrimRight(raw, "/")
	m := prURLRe.FindStringSubmatch(raw)
	if m == nil {
		return "", "", 0, fmt.Errorf("could not parse GitHub PR URL: %s", raw)
	}
	n, _ := strconv.Atoi(m[3])
	return m[1], m[2], n, nil
}

func runGH(stdin string, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// CheckGH verifies the gh CLI is installed and authenticated.
func CheckGH() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh CLI not found — install it with: brew install gh")
	}
	if _, err := runGH("", "auth", "status"); err != nil {
		return fmt.Errorf("gh is not authenticated — run: gh auth login")
	}
	return nil
}

// CurrentUser returns the authenticated GitHub login.
func CurrentUser() (string, error) {
	out, err := runGH("", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// FetchPR fetches PR metadata and its diff.
func FetchPR(rawURL string) (*PR, error) {
	owner, repo, number, err := ParsePRURL(rawURL)
	if err != nil {
		return nil, err
	}
	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)

	metaJSON, err := runGH("", "pr", "view", prURL, "--json",
		"number,title,author,state,additions,deletions,changedFiles,url,baseRefName,headRefName,headRefOid")
	if err != nil {
		return nil, err
	}
	var meta struct {
		Number       int    `json:"number"`
		Title        string `json:"title"`
		State        string `json:"state"`
		Additions    int    `json:"additions"`
		Deletions    int    `json:"deletions"`
		ChangedFiles int    `json:"changedFiles"`
		URL          string `json:"url"`
		BaseRefName  string `json:"baseRefName"`
		HeadRefName  string `json:"headRefName"`
		HeadRefOid   string `json:"headRefOid"`
		Author       struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return nil, fmt.Errorf("parse pr metadata: %w", err)
	}

	diff, err := runGH("", "pr", "diff", prURL)
	if err != nil {
		return nil, err
	}

	return &PR{
		Owner:        owner,
		Repo:         repo,
		Number:       number,
		Title:        meta.Title,
		Author:       meta.Author.Login,
		State:        meta.State,
		BaseRef:      meta.BaseRefName,
		HeadRef:      meta.HeadRefName,
		HeadOid:      meta.HeadRefOid,
		URL:          meta.URL,
		Additions:    meta.Additions,
		Deletions:    meta.Deletions,
		ChangedFiles: meta.ChangedFiles,
		Diff:         diff,
	}, nil
}

// PostReview submits a review (APPROVE / COMMENT / REQUEST_CHANGES) with
// optional line-level comments. If GitHub rejects the line comments (e.g. a
// line is not part of the diff), it retries once without them, folding the
// findings into the review body. Returns usedFallback=true in that case.
func PostReview(pr *PR, state, body string, comments []ReviewComment) (usedFallback bool, err error) {
	endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews", pr.Owner, pr.Repo, pr.Number)

	payload := map[string]any{"body": body, "event": state}
	if len(comments) > 0 {
		payload["comments"] = comments
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	_, err = runGH(string(data), "api", "--method", "POST", endpoint, "--input", "-")
	if err == nil {
		return false, nil
	}
	if len(comments) == 0 {
		return false, err
	}

	// Fallback: fold line comments into the body and retry without them.
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\n---\n**Findings** (could not be attached as line comments):\n")
	for _, c := range comments {
		fmt.Fprintf(&b, "- `%s:%d` — %s\n", c.Path, c.Line, c.Body)
	}
	data, err = json.Marshal(map[string]any{"body": b.String(), "event": state})
	if err != nil {
		return false, err
	}
	if _, err := runGH(string(data), "api", "--method", "POST", endpoint, "--input", "-"); err != nil {
		return false, err
	}
	return true, nil
}
