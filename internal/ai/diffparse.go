package ai

import (
	"bufio"
	"strconv"
	"strings"
)

// diffIndex maps each changed file to the set of new-side line numbers that
// are part of the diff (added or context lines) — the only lines GitHub
// accepts for RIGHT-side review comments.
type diffIndex struct {
	files map[string]map[int]bool
	// added maps file -> new-side line numbers that were actually added,
	// which are the strongest anchors for findings.
	added map[string]map[int]bool
}

// parseDiff builds a diffIndex from a unified diff.
func parseDiff(diff string) *diffIndex {
	idx := &diffIndex{
		files: map[string]map[int]bool{},
		added: map[string]map[int]bool{},
	}
	var (
		cur     string
		newLine int
		inHunk  bool
	)
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			name := strings.TrimPrefix(line, "+++ ")
			name = strings.TrimPrefix(name, "b/")
			if name == "/dev/null" {
				name = ""
			}
			cur = name
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			// @@ -a,b +c,d @@
			parts := strings.Fields(line)
			for _, p := range parts {
				if strings.HasPrefix(p, "+") {
					numStr := strings.TrimPrefix(p, "+")
					if i := strings.Index(numStr, ","); i >= 0 {
						numStr = numStr[:i]
					}
					if n, err := strconv.Atoi(numStr); err == nil {
						newLine = n
						inHunk = true
					}
					break
				}
			}
		case inHunk && cur != "":
			if len(line) == 0 {
				// context line with empty content
				markLine(idx.files, cur, newLine)
				newLine++
				continue
			}
			switch line[0] {
			case '+':
				markLine(idx.files, cur, newLine)
				markLine(idx.added, cur, newLine)
				newLine++
			case ' ':
				markLine(idx.files, cur, newLine)
				newLine++
			case '-':
				// old-side line: does not advance new-side counter
			case '\\':
				// "\ No newline at end of file"
			default:
				inHunk = false
			}
		}
	}
	return idx
}

func markLine(m map[string]map[int]bool, file string, line int) {
	if m[file] == nil {
		m[file] = map[int]bool{}
	}
	m[file][line] = true
}

// paths returns all changed file paths.
func (d *diffIndex) paths() []string {
	out := make([]string, 0, len(d.files))
	for p := range d.files {
		out = append(out, p)
	}
	return out
}

// snap validates a (path, line) pair against the diff. It returns the exact
// line if commentable, else the nearest commentable line within ±3, else 0.
func (d *diffIndex) snap(path string, line int) int {
	lines := d.files[path]
	if lines == nil {
		return 0
	}
	if lines[line] {
		return line
	}
	for delta := 1; delta <= 3; delta++ {
		if lines[line+delta] {
			return line + delta
		}
		if line-delta > 0 && lines[line-delta] {
			return line - delta
		}
	}
	return 0
}
