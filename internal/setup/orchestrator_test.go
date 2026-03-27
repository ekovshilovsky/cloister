package setup_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/setup"
)

func TestIsFirstRun(t *testing.T) {
	state := &setup.SetupState{}
	if !setup.IsFirstRun(state) {
		t.Error("expected first run for empty state")
	}
	state.Credentials.KeychainPassword = true
	if setup.IsFirstRun(state) {
		t.Error("expected re-run when credentials are configured")
	}
}

func TestIsFirstRunChannels(t *testing.T) {
	state := &setup.SetupState{}
	state.Channels.Telegram.Configured = true
	if setup.IsFirstRun(state) {
		t.Error("expected re-run when Telegram is configured")
	}
}

func TestIsFirstRunProviders(t *testing.T) {
	state := &setup.SetupState{}
	state.Providers.DefaultProvider = "ollama"
	if setup.IsFirstRun(state) {
		t.Error("expected re-run when default provider is set")
	}
}

func TestAllSectionsNotEmpty(t *testing.T) {
	sections := setup.AllSections()
	if len(sections) == 0 {
		t.Fatal("AllSections returned empty slice")
	}
}

func TestAllSectionsNoDuplicateNames(t *testing.T) {
	sections := setup.AllSections()
	names := make(map[string]bool)
	for _, s := range sections {
		if s.Name == "" {
			t.Error("section has empty name")
		}
		if names[s.Name] {
			t.Errorf("duplicate section name: %s", s.Name)
		}
		names[s.Name] = true
	}
}

func TestAllSectionsExpectedOrder(t *testing.T) {
	sections := setup.AllSections()
	expected := []string{"credentials", "channels", "providers", "oauth", "pairing"}
	if len(sections) != len(expected) {
		t.Fatalf("got %d sections, want %d", len(sections), len(expected))
	}
	for i, name := range expected {
		if sections[i].Name != name {
			t.Errorf("section[%d].Name = %q, want %q", i, sections[i].Name, name)
		}
	}
}

func TestAllSectionsHaveDescriptions(t *testing.T) {
	for _, s := range setup.AllSections() {
		if s.Description == "" {
			t.Errorf("section %q has empty description", s.Name)
		}
	}
}

func TestSectionIsConfiguredEmptyState(t *testing.T) {
	sections := setup.AllSections()
	empty := &setup.SetupState{}
	for _, s := range sections {
		if s.IsConfigured(empty) {
			t.Errorf("section %q should not be configured for empty state", s.Name)
		}
	}
}
