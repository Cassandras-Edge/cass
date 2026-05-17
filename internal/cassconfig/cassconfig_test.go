package cassconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadForCwdLoadsClientPersonas(t *testing.T) {
	dir := t.TempDir()
	config := `
[codex]
args = ["--search"]

[codex.personas.finance]
args = ["--profile", "finance"]

[codex.personas.finance.env]
CASS_PERSONA = "finance"
`
	if err := os.WriteFile(filepath.Join(dir, ".cass.toml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := LoadForCwd(dir, "codex")
	if err != nil {
		t.Fatal(err)
	}

	if got, want := len(resolved.Args), 1; got != want {
		t.Fatalf("len(Args) = %d, want %d", got, want)
	}
	if resolved.Args[0] != "--search" {
		t.Fatalf("Args[0] = %q, want --search", resolved.Args[0])
	}

	persona, ok := resolved.Personas["finance"]
	if !ok {
		t.Fatal("finance persona not loaded")
	}
	if got, want := persona.Args, []string{"--profile", "finance"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("persona args = %#v, want %#v", got, want)
	}
	if got, want := persona.Env["CASS_PERSONA"], "finance"; got != want {
		t.Fatalf("persona env CASS_PERSONA = %q, want %q", got, want)
	}
}
