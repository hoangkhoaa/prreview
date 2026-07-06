package github

import "strings"

// ListAccessibleRepos returns full names (owner/name) of repos the
// authenticated user can access — owned, collaborating, or via org
// membership — sorted by most recently pushed. Capped to ~400 entries.
func ListAccessibleRepos() ([]string, error) {
	out, err := runGH("", "api",
		"user/repos?per_page=100&affiliation=owner,collaborator,organization_member&sort=pushed",
		"--paginate", "--jq", ".[].full_name")
	if err != nil {
		return nil, err
	}
	var repos []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		repos = append(repos, line)
		if len(repos) >= 400 {
			break
		}
	}
	return repos, nil
}
