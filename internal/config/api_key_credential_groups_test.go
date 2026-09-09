package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSaveConfigPreserveCommentsCredentialGroupDeletion(t *testing.T) {
	tests := []struct {
		name     string
		bindings string
	}{
		{
			name:     "delete one binding and retain another",
			bindings: "  owner-key:\n    - shared\n  guest-key:\n    - shared\n",
		},
		{
			name:     "delete the last binding",
			bindings: "  owner-key:\n    - shared\n",
		},
		{
			name:     "delete one binding and retain explicit deny-all",
			bindings: "  owner-key:\n    - shared\n  guest-key: []\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			initial := "api-keys:\n  - owner-key\n  - guest-key\napi-key-credential-groups:\n" + tt.bindings
			if errWrite := os.WriteFile(path, []byte(initial), 0o600); errWrite != nil {
				t.Fatalf("write initial config: %v", errWrite)
			}
			cfg, errLoad := LoadConfig(path)
			if errLoad != nil {
				t.Fatalf("load initial config: %v", errLoad)
			}
			guestGroups, guestRestricted := cfg.APIKeyCredentialGroups["guest-key"]
			guestGroups = slices.Clone(guestGroups)
			delete(cfg.APIKeyCredentialGroups, "owner-key")

			// Repeated saves and reloads must not resurrect a removed restriction.
			for round := 0; round < 2; round++ {
				if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
					t.Fatalf("save config, round %d: %v", round, errSave)
				}
				cfg, errLoad = LoadConfig(path)
				if errLoad != nil {
					t.Fatalf("reload config, round %d: %v", round, errLoad)
				}
				if _, restricted := cfg.APIKeyCredentialGroups["owner-key"]; restricted {
					t.Fatalf("deleted owner binding returned after save/reload, round %d", round)
				}
				gotGuest, gotRestricted := cfg.APIKeyCredentialGroups["guest-key"]
				if gotRestricted != guestRestricted || !slices.Equal(gotGuest, guestGroups) {
					t.Fatalf("guest restriction changed, round %d: got (%v, %v), want (%v, %v)",
						round, gotGuest, gotRestricted, guestGroups, guestRestricted)
				}
				if !slices.Equal(cfg.APIKeys, []string{"owner-key", "guest-key"}) {
					t.Fatalf("API keys changed, round %d", round)
				}
			}
		})
	}
}
