package cmd

import (
	"reflect"
	"testing"

	"github.com/Cassandras-Edge/cass/internal/cassconfig"
)

func TestResolvePersonaConsumesConfiguredFirstArg(t *testing.T) {
	resolved := &cassconfig.Resolved{
		Personas: map[string]cassconfig.PersonaConfig{
			"finance": {
				Args: []string{"--profile", "finance"},
				Env:  map[string]string{"CASS_PERSONA": "finance"},
			},
		},
	}

	persona, remaining := resolvePersona(resolved, []string{"finance", "scan", "today"})

	if !reflect.DeepEqual(persona.Args, []string{"--profile", "finance"}) {
		t.Fatalf("persona args = %#v", persona.Args)
	}
	if got, want := persona.Env["CASS_PERSONA"], "finance"; got != want {
		t.Fatalf("persona env = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(remaining, []string{"scan", "today"}) {
		t.Fatalf("remaining args = %#v", remaining)
	}
}

func TestResolvePersonaLeavesUnknownFirstArgUntouched(t *testing.T) {
	resolved := &cassconfig.Resolved{
		Personas: map[string]cassconfig.PersonaConfig{
			"finance": {Args: []string{"--profile", "finance"}},
		},
	}
	userArgs := []string{"fix", "this"}

	persona, remaining := resolvePersona(resolved, userArgs)

	if len(persona.Args) != 0 || len(persona.Env) != 0 {
		t.Fatalf("unexpected persona: %#v", persona)
	}
	if !reflect.DeepEqual(remaining, userArgs) {
		t.Fatalf("remaining args = %#v, want %#v", remaining, userArgs)
	}
}
