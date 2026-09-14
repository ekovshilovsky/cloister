package processidentity

import (
	"os"
	"testing"
)

func TestIdentityMatchesExactProcessGeneration(t *testing.T) {
	identity, err := Read(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !Matches(os.Getpid(), identity) {
		t.Fatal("captured process identity did not match")
	}
	changed := identity
	changed.StartTime += "-reused"
	if Matches(os.Getpid(), changed) {
		t.Fatal("changed process start time matched")
	}
	changed = identity
	changed.Executable += ".unrelated"
	if Matches(os.Getpid(), changed) {
		t.Fatal("changed process executable matched")
	}
}
