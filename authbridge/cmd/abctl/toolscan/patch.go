package toolscan

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	toolPruneEntry = regexp.MustCompile(`^(\s*)-\s+name:\s*tool-prune\s*$`)
	listItem       = regexp.MustCompile(`^\s*-\s`)
	removeKey      = regexp.MustCompile(`^(\s*)remove:\s*.*$`)
)

// PatchConfig rewrites the remove: list of the tool-prune entry in the YAML at
// path, in place, and reports whether the file changed.
//
// Line-based on purpose. Round-tripping through a YAML library would reformat
// the whole document — dropping the comments that explain each plugin and
// reflowing entries the operator hand-tuned. The edit here touches exactly one
// line, so everything else in the file survives byte-for-byte, and re-running
// with the same candidates is a no-op.
func PatchConfig(path string, candidates []string) (changed bool, err error) {
	orig, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(orig), "\n")

	start := -1
	var entryIndent string
	for i, l := range lines {
		if m := toolPruneEntry.FindStringSubmatch(l); m != nil {
			start, entryIndent = i, m[1]
			break
		}
	}
	if start < 0 {
		return false, fmt.Errorf("no `- name: tool-prune` entry in %s — add the plugin to a pipeline first", path)
	}

	// The entry ends at the next list item indented no deeper than this one.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if listItem.MatchString(lines[i]) && leadingSpaces(lines[i]) <= len(entryIndent) {
			end = i
			break
		}
	}

	want := "remove: []"
	if len(candidates) > 0 {
		want = fmt.Sprintf("remove: [%s]", strings.Join(candidates, ", "))
	}
	for i := start + 1; i < end; i++ {
		m := removeKey.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		replacement := m[1] + want
		if lines[i] == replacement {
			return false, nil // already current — idempotent
		}
		lines[i] = replacement
		out := strings.Join(lines, "\n")
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("tool-prune entry in %s has no `remove:` key under config: — add `remove: []` and re-run", path)
}

func leadingSpaces(s string) int {
	for i, r := range s {
		if r != ' ' && r != '\t' {
			return i
		}
	}
	return len(s)
}
