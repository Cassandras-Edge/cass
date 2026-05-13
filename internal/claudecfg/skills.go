package claudecfg

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// SkillsDir returns the user-scoped skills directory (always
// ~/.claude/skills/ — Claude Code does not currently support
// project-scoped skills via settings.json).
func SkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// WriteSkill drops `body` into ~/.claude/skills/<name>/SKILL.md. Creates
// dirs as needed. If the file already has identical contents, the write
// is skipped (preserves mtime — handy for change detection).
//
// Returns true if the file was created or modified.
func WriteSkill(name, body string) (bool, error) {
	dir, err := SkillsDir()
	if err != nil {
		return false, err
	}
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return false, err
	}
	path := filepath.Join(skillDir, "SKILL.md")
	if existing, err := os.ReadFile(path); err == nil {
		if sha256.Sum256(existing) == sha256.Sum256([]byte(body)) {
			return false, nil
		}
	}
	return true, os.WriteFile(path, []byte(body), 0o644)
}

// RemoveSkill deletes ~/.claude/skills/<name>/ entirely. Used when a
// service is removed from the registry.
func RemoveSkill(name string) (bool, error) {
	dir, err := SkillsDir()
	if err != nil {
		return false, err
	}
	skillDir := filepath.Join(dir, name)
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		return false, nil
	}
	return true, os.RemoveAll(skillDir)
}

// ListSkills returns the names of skills in ~/.claude/skills/. Filters to
// directories containing a SKILL.md.
func ListSkills() ([]string, error) {
	dir, err := SkillsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

var _ = fmt.Sprintf // keep import while package grows
