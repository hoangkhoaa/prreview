package github

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
)

// FileFetchLimits caps how much full-file context is fetched.
type FileFetchLimits struct {
	MaxFiles     int
	PerFileBytes int
	TotalBytes   int
}

// generated/vendored files that add noise, not context.
var skipPatterns = []string{
	"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum",
	"Cargo.lock", "poetry.lock", "composer.lock", "Gemfile.lock",
}

func skippable(p string) bool {
	base := path.Base(p)
	for _, s := range skipPatterns {
		if base == s {
			return true
		}
	}
	if strings.Contains(p, "vendor/") || strings.Contains(p, "node_modules/") {
		return true
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf",
		".woff", ".woff2", ".ttf", ".eot", ".zip", ".gz", ".jar",
		".min.js", ".map", ".svg", ".lock":
		return true
	}
	return false
}

// FetchFiles downloads the full head-revision content of the given changed
// files (for review context beyond the diff hunks). Files that are binary,
// generated, missing (deleted in the PR), or over the limits are skipped.
// Fetch errors are non-fatal: the review degrades to diff-only context.
func FetchFiles(pr *PR, paths []string, lim FileFetchLimits) map[string]string {
	sort.Strings(paths)

	var candidates []string
	for _, p := range paths {
		if p == "" || skippable(p) {
			continue
		}
		candidates = append(candidates, p)
		if len(candidates) >= lim.MaxFiles {
			break
		}
	}

	ref := pr.HeadOid
	if ref == "" {
		ref = pr.HeadRef
	}

	type result struct {
		path    string
		content string
	}
	sem := make(chan struct{}, 6)
	results := make(chan result, len(candidates))
	var wg sync.WaitGroup

	for _, p := range candidates {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			endpoint := fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", pr.Owner, pr.Repo, p, ref)
			out, err := runGH("", "api", "-H", "Accept: application/vnd.github.raw", endpoint)
			if err != nil || out == "" {
				return
			}
			if len(out) > lim.PerFileBytes {
				out = out[:lim.PerFileBytes] + "\n... (truncated)\n"
			}
			// crude binary guard
			if strings.ContainsRune(out[:min(len(out), 1024)], '\x00') {
				return
			}
			results <- result{path: p, content: out}
		}(p)
	}
	wg.Wait()
	close(results)

	files := map[string]string{}
	total := 0
	for r := range results {
		if total+len(r.content) > lim.TotalBytes {
			continue
		}
		files[r.path] = r.content
		total += len(r.content)
	}
	return files
}
