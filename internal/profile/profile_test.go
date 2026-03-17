package profile_test

import (
	"testing"

	"github.com/ekovshilovsky/cloister/internal/profile"
)

// ---------------------------------------------------------------------------
// ValidateName
// ---------------------------------------------------------------------------

func TestValidateName_Valid(t *testing.T) {
	cases := []string{
		"work",
		"my-project",
		"dev",
		"a1",
		"abc-123-xyz",
	}
	for _, name := range cases {
		if err := profile.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) returned unexpected error: %v", name, err)
		}
	}
}

func TestValidateName_Empty(t *testing.T) {
	if err := profile.ValidateName(""); err == nil {
		t.Error("ValidateName(\"\") expected an error, got nil")
	}
}

func TestValidateName_Reserved(t *testing.T) {
	reserved := []string{
		"all", "status", "version", "help", "create",
		"stop", "delete", "update", "backup", "restore",
		"rebuild", "setup", "config", "self-update", "add-stack", "agent",
	}
	for _, name := range reserved {
		if err := profile.ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) expected error for reserved name, got nil", name)
		}
	}
}

func TestValidateName_InvalidPatterns(t *testing.T) {
	cases := []string{
		"my work", // space not permitted
		"-work",   // must begin with a letter
		"Work",    // uppercase not permitted
		"123",     // must begin with a letter
		"my_proj", // underscore not permitted
		"my.proj", // dot not permitted
	}
	for _, name := range cases {
		if err := profile.ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) expected error for invalid pattern, got nil", name)
		}
	}
}

// ---------------------------------------------------------------------------
// ValidateStacks
// ---------------------------------------------------------------------------

func TestValidateStacks_Valid(t *testing.T) {
	cases := [][]string{
		{"web", "cloud"},
		{"python", "go", "rust"},
		{"dotnet"},
		{"data"},
		{}, // empty list is valid (no stacks selected)
	}
	for _, stacks := range cases {
		if err := profile.ValidateStacks(stacks); err != nil {
			t.Errorf("ValidateStacks(%v) returned unexpected error: %v", stacks, err)
		}
	}
}

func TestValidateStacks_Invalid(t *testing.T) {
	cases := [][]string{
		{"web", "invalid"},
		{"node"}, // "node" is not a valid stack name
		{"java"},
		{""},
	}
	for _, stacks := range cases {
		if err := profile.ValidateStacks(stacks); err == nil {
			t.Errorf("ValidateStacks(%v) expected error for invalid stack, got nil", stacks)
		}
	}
}

// ---------------------------------------------------------------------------
// AutoColor
// ---------------------------------------------------------------------------

func TestAutoColor_DifferentIndices(t *testing.T) {
	c0 := profile.AutoColor(0)
	c1 := profile.AutoColor(1)
	if c0 == c1 {
		t.Errorf("AutoColor(0) and AutoColor(1) returned the same color %q; expected distinct values", c0)
	}
}

func TestAutoColor_Length(t *testing.T) {
	for i := 0; i < 8; i++ {
		c := profile.AutoColor(i)
		if len(c) != 6 {
			t.Errorf("AutoColor(%d) = %q, expected a 6-character hex string", i, c)
		}
	}
}

func TestAutoColor_Wraps(t *testing.T) {
	// Palette has 8 entries; index 8 must equal index 0.
	if profile.AutoColor(0) != profile.AutoColor(8) {
		t.Errorf("AutoColor(0) = %q, AutoColor(8) = %q; expected equal after wrap-around",
			profile.AutoColor(0), profile.AutoColor(8))
	}
}
