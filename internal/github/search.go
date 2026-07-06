package github

import (
	"encoding/json"
	"fmt"
)

// PRRef is a lightweight reference to a PR found by search.
type PRRef struct {
	Repo   string // owner/name
	Number int
	Title  string
	URL    string
}

// Key uniquely identifies a PR.
func (r PRRef) Key() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// SearchReviewRequested returns open PRs where the authenticated user's
// review is requested. By default only direct (user) review requests are
// matched; includeTeams also matches requests to teams the user belongs to.
// A non-empty repos list ("owner/name") limits the search to those repos.
func SearchReviewRequested(includeTeams bool, repos []string) ([]PRRef, error) {
	qualifier := "user-review-requested:@me"
	if includeTeams {
		qualifier = "review-requested:@me"
	}
	args := []string{"search", "prs", qualifier}
	for _, r := range repos {
		if r != "" {
			args = append(args, "repo:"+r)
		}
	}
	args = append(args, "--state=open", "--limit", "50",
		"--json", "url,title,number,repository")
	out, err := runGH("", args...)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		Number     int    `json:"number"`
		Repository struct {
			Name          string `json:"name"`
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}
	refs := make([]PRRef, 0, len(rows))
	for _, r := range rows {
		repo := r.Repository.NameWithOwner
		if repo == "" {
			repo = r.Repository.Name
		}
		refs = append(refs, PRRef{Repo: repo, Number: r.Number, Title: r.Title, URL: r.URL})
	}
	return refs, nil
}
