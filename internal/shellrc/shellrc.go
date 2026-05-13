// Package shellrc manages a sentinel-delimited block in the user's shell
// rc file. cass writes aliases (`claude` → `cass claude`, `codex` → `cass codex`)
// inside the block so the wrapper subcommands intercept every invocation.
//
// Idempotency is the whole point: re-running setup rewrites the block in
// place — never appends a duplicate. Teardown strips the block cleanly.
// Anything outside the markers is preserved byte-for-byte.
package shellrc

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	beginMarker = "# >>> cass managed (do not edit) >>>"
	endMarker   = "# <<< cass managed <<<"
)

// AliasBlock is the canonical content of the managed block. Two aliases
// + comments. Kept short on purpose — the rc file is sensitive territory.
const AliasBlock = beginMarker + `
# Rebinds claude/codex to go through cass so per-project .cass.toml
# and MCP key auto-refresh kick in. ` + "`cass setup`" + ` rewrites this block;
# ` + "`cass teardown`" + ` removes it.
alias claude='cass claude'
alias codex='cass codex'
` + endMarker + "\n"

// rcCandidates returns the rc files cass knows about, in preference
// order. Returns empty slice when neither exists — we don't create rc
// files that the user hasn't set up themselves.
func rcCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range []string{".zshrc", ".bashrc"} {
		p := filepath.Join(home, name)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// Targets reports which rc files Install/Remove will touch. Useful for
// the interactive setup form so we can tell the user up front.
func Targets() []string {
	return rcCandidates()
}

// Install writes the managed alias block to every available rc file.
// Returns the list of files actually updated (excludes ones already in
// the desired state). No-op if no rc file exists.
func Install() ([]string, error) {
	var touched []string
	for _, path := range rcCandidates() {
		changed, err := writeManagedBlock(path, AliasBlock)
		if err != nil {
			return touched, err
		}
		if changed {
			touched = append(touched, path)
		}
	}
	return touched, nil
}

// Remove strips the managed block from every rc file. Returns the list
// of files actually modified.
func Remove() ([]string, error) {
	var touched []string
	for _, path := range rcCandidates() {
		changed, err := stripManagedBlock(path)
		if err != nil {
			return touched, err
		}
		if changed {
			touched = append(touched, path)
		}
	}
	return touched, nil
}

// FindExistingClaudeFunction scans the rc files for a `claude()` or
// `function claude` definition outside the managed block. Returns
// (file, line) of the first match, or ("", 0) if nothing is shadowed.
// Used to warn the user during setup that their hand-written wrapper
// will be superseded by the alias.
func FindExistingClaudeFunction() (string, int) {
	re := regexp.MustCompile(`^\s*(?:function\s+)?claude\s*\(\)\s*\{`)
	for _, path := range rcCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		inManaged := false
		scanner := bufio.NewScanner(bytes.NewReader(data))
		line := 0
		for scanner.Scan() {
			line++
			t := scanner.Text()
			if strings.Contains(t, beginMarker) {
				inManaged = true
				continue
			}
			if strings.Contains(t, endMarker) {
				inManaged = false
				continue
			}
			if inManaged {
				continue
			}
			if re.MatchString(t) {
				return path, line
			}
		}
	}
	return "", 0
}

// writeManagedBlock idempotently sets the managed block in `path` to
// `block`. Returns whether the file content actually changed.
//
// Behavior:
//   - If markers already exist: replace whatever's between them with
//     `block` (keeps surrounding content intact).
//   - If markers don't exist: append `block` to end of file (with a
//     leading newline if the file doesn't end in one).
//   - If the desired block already matches what's there: no-op.
func writeManagedBlock(path, block string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}

	newContent, changed := upsertBlock(string(existing), block)
	if !changed {
		return false, nil
	}
	// Preserve original file mode (default 0644 if stat fails for some
	// reason — but the rc file already exists since rcCandidates filters).
	info, _ := os.Stat(path)
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	return true, os.WriteFile(path, []byte(newContent), mode)
}

// upsertBlock returns (newContent, changed). Pure function — tested
// without touching disk.
func upsertBlock(content, block string) (string, bool) {
	beginIdx := strings.Index(content, beginMarker)
	if beginIdx == -1 {
		// Append. Ensure exactly one blank line separator.
		trimmed := strings.TrimRight(content, "\n")
		var b strings.Builder
		b.WriteString(trimmed)
		if trimmed != "" {
			b.WriteString("\n\n")
		}
		b.WriteString(block)
		return b.String(), true
	}
	// Find the end marker on a line at or after begin.
	endRegion := content[beginIdx:]
	endIdx := strings.Index(endRegion, endMarker)
	if endIdx == -1 {
		// Begin marker present but no end — corrupted block. Replace
		// everything from begin to EOF with the canonical block.
		newContent := content[:beginIdx] + block
		return newContent, newContent != content
	}
	// endIdx is relative to beginIdx; advance past the end marker line.
	absEnd := beginIdx + endIdx + len(endMarker)
	// Include the trailing newline if there is one so we don't leave a
	// dangling `<<< cass managed <<<` glued to the next line.
	if absEnd < len(content) && content[absEnd] == '\n' {
		absEnd++
	}
	newContent := content[:beginIdx] + block + content[absEnd:]
	return newContent, newContent != content
}

// stripManagedBlock removes the managed block from `path`, if present.
// Returns whether anything changed.
func stripManagedBlock(path string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	content := string(existing)
	beginIdx := strings.Index(content, beginMarker)
	if beginIdx == -1 {
		return false, nil
	}
	endRegion := content[beginIdx:]
	endIdx := strings.Index(endRegion, endMarker)
	if endIdx == -1 {
		return false, fmt.Errorf("managed block in %s missing end marker (refusing to strip a half-block)", path)
	}
	absEnd := beginIdx + endIdx + len(endMarker)
	if absEnd < len(content) && content[absEnd] == '\n' {
		absEnd++
	}
	// Also drop the blank line we left after the block when appending.
	preEnd := beginIdx
	for preEnd > 0 && content[preEnd-1] == '\n' {
		preEnd--
	}
	// Keep at most one newline before the block start (preserves
	// formatting if the user has unrelated content directly above).
	keep := beginIdx
	if beginIdx-preEnd >= 1 {
		keep = preEnd + 1
	}
	newContent := content[:keep] + content[absEnd:]
	if newContent == content {
		return false, nil
	}
	info, _ := os.Stat(path)
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	return true, os.WriteFile(path, []byte(newContent), mode)
}
