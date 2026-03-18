package config_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
	"gopkg.in/yaml.v3"
)

// --- Unmarshal tests ---

// TestResourcePolicyUnmarshalAuto verifies that the scalar string "auto" is
// parsed into Mode="auto" with IsSet=true and an empty Names slice.
func TestResourcePolicyUnmarshalAuto(t *testing.T) {
	input := `tunnels: auto`
	var s struct {
		Tunnels config.ResourcePolicy `yaml:"tunnels"`
	}
	if err := yaml.Unmarshal([]byte(input), &s); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	p := s.Tunnels
	if !p.IsSet {
		t.Error("IsSet must be true after unmarshal")
	}
	if p.Mode != "auto" {
		t.Errorf("Mode: got %q, want %q", p.Mode, "auto")
	}
	if len(p.Names) != 0 {
		t.Errorf("Names must be empty for scalar form, got %v", p.Names)
	}
}

// TestResourcePolicyUnmarshalNone verifies that the scalar string "none" is
// parsed into Mode="none" with IsSet=true and an empty Names slice.
func TestResourcePolicyUnmarshalNone(t *testing.T) {
	input := `tunnels: none`
	var s struct {
		Tunnels config.ResourcePolicy `yaml:"tunnels"`
	}
	if err := yaml.Unmarshal([]byte(input), &s); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	p := s.Tunnels
	if !p.IsSet {
		t.Error("IsSet must be true after unmarshal")
	}
	if p.Mode != "none" {
		t.Errorf("Mode: got %q, want %q", p.Mode, "none")
	}
	if len(p.Names) != 0 {
		t.Errorf("Names must be empty for scalar form, got %v", p.Names)
	}
}

// TestResourcePolicyUnmarshalList verifies that a YAML sequence is parsed into
// Mode="" with IsSet=true and Names populated with the sequence entries.
func TestResourcePolicyUnmarshalList(t *testing.T) {
	input := "tunnels:\n  - clipboard\n  - ollama\n"
	var s struct {
		Tunnels config.ResourcePolicy `yaml:"tunnels"`
	}
	if err := yaml.Unmarshal([]byte(input), &s); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	p := s.Tunnels
	if !p.IsSet {
		t.Error("IsSet must be true after unmarshal")
	}
	if p.Mode != "" {
		t.Errorf("Mode must be empty for list form, got %q", p.Mode)
	}
	want := []string{"clipboard", "ollama"}
	if len(p.Names) != len(want) {
		t.Fatalf("Names length: got %d, want %d", len(p.Names), len(want))
	}
	for i, v := range want {
		if p.Names[i] != v {
			t.Errorf("Names[%d]: got %q, want %q", i, p.Names[i], v)
		}
	}
}

// TestResourcePolicyUnmarshalOmitted verifies that a field absent from the
// YAML document remains a zero-value ResourcePolicy with IsSet=false.
func TestResourcePolicyUnmarshalOmitted(t *testing.T) {
	input := `memory_budget: 8`
	var s struct {
		Tunnels config.ResourcePolicy `yaml:"tunnels"`
	}
	if err := yaml.Unmarshal([]byte(input), &s); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	p := s.Tunnels
	if p.IsSet {
		t.Error("IsSet must be false when the field is absent from the document")
	}
	if p.Mode != "" {
		t.Errorf("Mode must be empty when unset, got %q", p.Mode)
	}
	if len(p.Names) != 0 {
		t.Errorf("Names must be nil/empty when unset, got %v", p.Names)
	}
}

// TestResourcePolicyUnmarshalInvalidScalar verifies that an unrecognised scalar
// value (i.e. neither "auto" nor "none") causes an unmarshal error.
func TestResourcePolicyUnmarshalInvalidScalar(t *testing.T) {
	input := `tunnels: maybe`
	var s struct {
		Tunnels config.ResourcePolicy `yaml:"tunnels"`
	}
	if err := yaml.Unmarshal([]byte(input), &s); err == nil {
		t.Error("expected an error for an invalid scalar value, got nil")
	}
}

// --- Marshal tests ---

// TestResourcePolicyMarshalAuto verifies that Mode="auto" serialises to the
// scalar string "auto".
func TestResourcePolicyMarshalAuto(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Mode: "auto"}
	out, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	got := string(out)
	// yaml.Marshal appends a trailing newline; trim for comparison.
	if got != "auto\n" {
		t.Errorf("marshaled output: got %q, want %q", got, "auto\n")
	}
}

// TestResourcePolicyMarshalNone verifies that Mode="none" serialises to the
// scalar string "none".
func TestResourcePolicyMarshalNone(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Mode: "none"}
	out, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	got := string(out)
	if got != "none\n" {
		t.Errorf("marshaled output: got %q, want %q", got, "none\n")
	}
}

// TestResourcePolicyMarshalList verifies that a Names-based policy serialises
// to a YAML sequence.
func TestResourcePolicyMarshalList(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Names: []string{"clipboard", "ollama"}}
	out, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	// Re-parse to avoid fragile string comparison.
	var names []string
	if err := yaml.Unmarshal(out, &names); err != nil {
		t.Fatalf("failed to re-parse marshaled list: %v", err)
	}
	if len(names) != 2 || names[0] != "clipboard" || names[1] != "ollama" {
		t.Errorf("re-parsed names: got %v, want [clipboard ollama]", names)
	}
}

// TestResourcePolicyMarshalUnset verifies that an unset policy (IsSet=false)
// marshals to nil so that the field is omitted from the parent document.
func TestResourcePolicyMarshalUnset(t *testing.T) {
	p := config.ResourcePolicy{}
	v, err := p.MarshalYAML()
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}
	if v != nil {
		t.Errorf("MarshalYAML for unset policy: got %v, want nil", v)
	}
}

// --- IsAllowed tests ---

// TestIsAllowedAuto verifies that Mode="auto" allows any resource name.
func TestIsAllowedAuto(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Mode: "auto"}
	for _, name := range []string{"clipboard", "ollama", "anything", ""} {
		if !p.IsAllowed(name) {
			t.Errorf("IsAllowed(%q) with auto mode: got false, want true", name)
		}
	}
}

// TestIsAllowedNone verifies that Mode="none" blocks every resource name.
func TestIsAllowedNone(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Mode: "none"}
	for _, name := range []string{"clipboard", "ollama", "anything", ""} {
		if p.IsAllowed(name) {
			t.Errorf("IsAllowed(%q) with none mode: got true, want false", name)
		}
	}
}

// TestIsAllowedList verifies that a Names-based policy allows only members of
// the explicit whitelist.
func TestIsAllowedList(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Names: []string{"clipboard", "ollama"}}
	cases := []struct {
		name string
		want bool
	}{
		{"clipboard", true},
		{"ollama", true},
		{"other", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := p.IsAllowed(tc.name); got != tc.want {
			t.Errorf("IsAllowed(%q): got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsAllowedUnset verifies that an unset policy (IsSet=false) behaves like
// "auto" and allows any resource name.
func TestIsAllowedUnset(t *testing.T) {
	p := config.ResourcePolicy{}
	for _, name := range []string{"clipboard", "ollama", "anything"} {
		if !p.IsAllowed(name) {
			t.Errorf("IsAllowed(%q) with unset policy: got false, want true", name)
		}
	}
}

// --- ResolveForTunnels tests ---

// TestResolveForTunnelsHeadless verifies that an unset policy resolves to
// Mode="none" in headless (non-interactive) environments.
func TestResolveForTunnelsHeadless(t *testing.T) {
	p := config.ResourcePolicy{}
	resolved := p.ResolveForTunnels(true)
	if !resolved.IsSet {
		t.Error("resolved policy must have IsSet=true")
	}
	if resolved.Mode != "none" {
		t.Errorf("ResolveForTunnels(headless=true): got Mode=%q, want %q", resolved.Mode, "none")
	}
}

// TestResolveForTunnelsInteractive verifies that an unset policy resolves to
// Mode="auto" in interactive environments.
func TestResolveForTunnelsInteractive(t *testing.T) {
	p := config.ResourcePolicy{}
	resolved := p.ResolveForTunnels(false)
	if !resolved.IsSet {
		t.Error("resolved policy must have IsSet=true")
	}
	if resolved.Mode != "auto" {
		t.Errorf("ResolveForTunnels(headless=false): got Mode=%q, want %q", resolved.Mode, "auto")
	}
}

// TestResolveForTunnelsExplicitUnchanged verifies that an already-set policy
// is returned unchanged regardless of the headless flag.
func TestResolveForTunnelsExplicitUnchanged(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Names: []string{"clipboard"}}
	for _, headless := range []bool{true, false} {
		resolved := p.ResolveForTunnels(headless)
		if resolved.Mode != "" || len(resolved.Names) != 1 || resolved.Names[0] != "clipboard" {
			t.Errorf("ResolveForTunnels(headless=%v): explicit policy was mutated: %+v", headless, resolved)
		}
	}
}

// --- ResolveForMounts tests ---

// TestResolveForMountsHeadless verifies that an unset policy resolves to the
// curated default list of four mount names in headless environments.
func TestResolveForMountsHeadless(t *testing.T) {
	p := config.ResourcePolicy{}
	resolved := p.ResolveForMounts(true)
	if !resolved.IsSet {
		t.Error("resolved policy must have IsSet=true")
	}
	if resolved.Mode != "" {
		t.Errorf("ResolveForMounts(headless=true): expected list form, got Mode=%q", resolved.Mode)
	}
	wantNames := []string{"code", "claude-plugins", "claude-skills", "claude-agents"}
	if len(resolved.Names) != len(wantNames) {
		t.Fatalf("Names length: got %d, want %d", len(resolved.Names), len(wantNames))
	}
	for i, v := range wantNames {
		if resolved.Names[i] != v {
			t.Errorf("Names[%d]: got %q, want %q", i, resolved.Names[i], v)
		}
	}
}

// TestResolveForMountsInteractive verifies that an unset policy resolves to
// Mode="auto" in interactive environments.
func TestResolveForMountsInteractive(t *testing.T) {
	p := config.ResourcePolicy{}
	resolved := p.ResolveForMounts(false)
	if !resolved.IsSet {
		t.Error("resolved policy must have IsSet=true")
	}
	if resolved.Mode != "auto" {
		t.Errorf("ResolveForMounts(headless=false): got Mode=%q, want %q", resolved.Mode, "auto")
	}
}

// TestResolveForMountsExplicitUnchanged verifies that an already-set policy
// is returned unchanged regardless of the headless flag.
func TestResolveForMountsExplicitUnchanged(t *testing.T) {
	p := config.ResourcePolicy{IsSet: true, Mode: "none"}
	for _, headless := range []bool{true, false} {
		resolved := p.ResolveForMounts(headless)
		if resolved.Mode != "none" {
			t.Errorf("ResolveForMounts(headless=%v): explicit policy was mutated: %+v", headless, resolved)
		}
	}
}
