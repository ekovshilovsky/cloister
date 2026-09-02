// Proprietary and confidential. All rights reserved.

package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"cloister.io/internal/config"
	"cloister.io/internal/vm"
	"github.com/spf13/cobra"
)

// fakeStatusBackend answers the two questions status asks of a backend, and
// can refuse to answer the way an uninstalled hypervisor does.
type fakeStatusBackend struct {
	prefix string
	vms    []vm.VMStatus
	err    error
}

func (f *fakeStatusBackend) List(bool) ([]vm.VMStatus, error) {
	return f.vms, f.err
}

func (f *fakeStatusBackend) ProfileFromVMName(name string) string {
	return strings.TrimPrefix(name, f.prefix)
}

func testProfiles() map[string]*config.Profile {
	return map[string]*config.Profile{
		"zulu":  {Backend: "colima", Memory: 4},
		"alpha": {Backend: "lume", Memory: 8},
		"mike":  {Backend: "", Memory: 2},
	}
}

// TestQueryVMsReportsUnknownForAnUnreachableBackend pins the distinction the
// status table has to make and used to collapse.
//
// A profile missing from the inventory means "not running" only when the
// backend was actually able to answer. When the hypervisor is absent or fails
// to enumerate, every profile on it is missing for a reason that says nothing
// about the VM, and calling that "stopped" states as fact something never
// measured.
func TestQueryVMsReportsUnknownForAnUnreachableBackend(t *testing.T) {
	inventory := queryVMs([]namedBackend{
		{name: "colima", display: "Colima", backend: &fakeStatusBackend{prefix: "cloister-"}},
		{name: "lume", display: "Lume", backend: &fakeStatusBackend{
			prefix: "cloister-",
			err:    errors.New("lume executable not found"),
		}},
	})

	if got := inventory.stateOf("alpha", "lume"); got != "unknown" {
		t.Errorf("state of a profile on an unreachable backend = %q, want %q", got, "unknown")
	}
	// The reachable backend answered, so absence there is genuine information.
	if got := inventory.stateOf("zulu", "colima"); got != "stopped" {
		t.Errorf("state of an absent profile on a reachable backend = %q, want %q", got, "stopped")
	}
}

// TestQueryVMsWarnsOncePerUnreachableBackend keeps the diagnosis attached to
// the cause. A warning per profile would repeat one backend-level fact for
// every profile configured against it.
func TestQueryVMsWarnsOncePerUnreachableBackend(t *testing.T) {
	inventory := queryVMs([]namedBackend{
		{name: "colima", display: "Colima", backend: &fakeStatusBackend{prefix: "cloister-"}},
		{name: "lume", display: "Lume", backend: &fakeStatusBackend{
			prefix: "cloister-",
			err:    errors.New("lume executable not found"),
		}},
	})

	if len(inventory.warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", inventory.warnings)
	}
	warning := inventory.warnings[0]
	if !strings.Contains(warning, "Lume") {
		t.Errorf("warning %q does not name the backend that failed", warning)
	}
	if !strings.Contains(warning, "lume executable not found") {
		t.Errorf("warning %q does not carry the cause", warning)
	}
}

// TestQueryVMsRecordsRunningVMs guards the other direction: a backend that
// answers must still populate the inventory.
func TestQueryVMsRecordsRunningVMs(t *testing.T) {
	inventory := queryVMs([]namedBackend{
		{name: "colima", display: "Colima", backend: &fakeStatusBackend{
			prefix: "cloister-",
			vms:    []vm.VMStatus{{Name: "cloister-zulu", Status: "Running"}},
		}},
	})

	if got := inventory.stateOf("zulu", "colima"); got != "running" {
		t.Errorf("state of a running profile = %q, want %q", got, "running")
	}
	if len(inventory.warnings) != 0 {
		t.Errorf("a backend that answered produced warnings: %v", inventory.warnings)
	}
}

// TestSelectProfilesSortsByName pins that two runs of status produce the same
// row order. Ranging a Go map randomizes iteration, so the table could not be
// diffed against a previous run.
func TestSelectProfilesSortsByName(t *testing.T) {
	// Repeated because map iteration order is random per range: a single pass
	// can match the wanted order by chance.
	for attempt := 0; attempt < 20; attempt++ {
		names, err := selectProfiles(testProfiles(), "")
		if err != nil {
			t.Fatalf("selectProfiles() error = %v", err)
		}
		want := []string{"alpha", "mike", "zulu"}
		if len(names) != len(want) {
			t.Fatalf("selectProfiles() = %v, want %v", names, want)
		}
		for i := range want {
			if names[i] != want[i] {
				t.Fatalf("selectProfiles() = %v, want %v", names, want)
			}
		}
	}
}

// TestSelectProfilesFiltersToOneProfile covers the argument that setup tells
// users to pass.
func TestSelectProfilesFiltersToOneProfile(t *testing.T) {
	names, err := selectProfiles(testProfiles(), "mike")
	if err != nil {
		t.Fatalf("selectProfiles() error = %v", err)
	}
	if len(names) != 1 || names[0] != "mike" {
		t.Fatalf("selectProfiles() = %v, want [mike]", names)
	}
}

// TestSelectProfilesRejectsAnUnknownProfile keeps the filter from silently
// reporting an empty table for a name the user mistyped.
func TestSelectProfilesRejectsAnUnknownProfile(t *testing.T) {
	if _, err := selectProfiles(testProfiles(), "absent"); err == nil {
		t.Fatal("selectProfiles() accepted a profile that is not configured")
	}
}

// TestTunnelSummaryDoesNotMarkUncheckedTunnelsFailed pins that status never
// shows a verdict it did not measure. The cross used to be printed for every
// configured tunnel although no health check ran at all.
func TestTunnelSummaryDoesNotMarkUncheckedTunnelsFailed(t *testing.T) {
	summary := tunnelSummary([]config.TunnelConfig{
		{Name: "api", HostPort: 8080, HealthCheck: "http://localhost:8080/health"},
		{Name: "web", HostPort: 3000},
	})

	if strings.ContainsRune(summary, '✗') {
		t.Errorf("tunnel summary %q marks an unperformed check as a failure", summary)
	}
	if strings.ContainsRune(summary, '✓') {
		t.Errorf("tunnel summary %q claims a check passed that never ran", summary)
	}
	for _, name := range []string{"api", "web"} {
		if !strings.Contains(summary, name) {
			t.Errorf("tunnel summary %q omits tunnel %q", summary, name)
		}
	}
	if !strings.Contains(summary, "not checked") {
		t.Errorf("tunnel summary %q does not say the health of these tunnels is unknown", summary)
	}
}

// TestTunnelSummaryIsEmptyWithoutTunnels keeps status from printing a tunnel
// line for a configuration that defines none.
func TestTunnelSummaryIsEmptyWithoutTunnels(t *testing.T) {
	if summary := tunnelSummary(nil); summary != "" {
		t.Errorf("tunnel summary for no tunnels = %q, want empty", summary)
	}
}

// TestPrintStatusJSONIsOrderedAndCarriesUnknownState covers the machine-readable
// path: scripts diffing successive runs need a stable order, and a consumer
// deciding whether to act on a profile must be able to tell an unreachable
// backend from a stopped VM.
func TestPrintStatusJSONIsOrderedAndCarriesUnknownState(t *testing.T) {
	inventory := queryVMs([]namedBackend{
		{name: "colima", display: "Colima", backend: &fakeStatusBackend{
			prefix: "cloister-",
			vms:    []vm.VMStatus{{Name: "cloister-zulu", Status: "Running"}},
		}},
		{name: "lume", display: "Lume", backend: &fakeStatusBackend{
			prefix: "cloister-",
			err:    errors.New("lume executable not found"),
		}},
	})
	cfg := &config.Config{Profiles: testProfiles()}
	names, err := selectProfiles(cfg.Profiles, "")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := printStatusJSON(cmd, cfg, names, inventory); err != nil {
		t.Fatalf("printStatusJSON() error = %v", err)
	}

	var decoded []profileStatus
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding status JSON: %v\n%s", err, out.String())
	}
	if len(decoded) != 3 {
		t.Fatalf("status JSON has %d entries, want 3", len(decoded))
	}
	for i, want := range []string{"alpha", "mike", "zulu"} {
		if decoded[i].Name != want {
			t.Fatalf("status JSON order = %v, want alpha, mike, zulu", decoded)
		}
	}
	if decoded[0].State != "unknown" {
		t.Errorf("state of a profile on an unreachable backend = %q, want %q", decoded[0].State, "unknown")
	}
	if decoded[2].State != "running" {
		t.Errorf("state of a running profile = %q, want %q", decoded[2].State, "running")
	}
}

// TestPrintStatusTableIsOrdered pins the human-readable path to the same
// stable order as the JSON one.
func TestPrintStatusTableIsOrdered(t *testing.T) {
	cfg := &config.Config{Profiles: testProfiles()}
	inventory := queryVMs([]namedBackend{
		{name: "colima", display: "Colima", backend: &fakeStatusBackend{prefix: "cloister-"}},
		{name: "lume", display: "Lume", backend: &fakeStatusBackend{prefix: "cloister-"}},
	})
	names, err := selectProfiles(cfg.Profiles, "")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := printStatusTable(cmd, cfg, names, inventory, 0, 16); err != nil {
		t.Fatalf("printStatusTable() error = %v", err)
	}

	body := out.String()
	alpha := strings.Index(body, "alpha")
	mike := strings.Index(body, "mike")
	zulu := strings.Index(body, "zulu")
	if alpha < 0 || mike < 0 || zulu < 0 {
		t.Fatalf("table is missing a profile row:\n%s", body)
	}
	if !(alpha < mike && mike < zulu) {
		t.Errorf("table rows are not sorted by profile name:\n%s", body)
	}
}
