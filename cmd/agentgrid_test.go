package cmd

import (
	"strings"
	"testing"

	"cloister.io/internal/agentgrid"
	"cloister.io/internal/vm"
)

func TestAgentgridCommandContract(t *testing.T) {
	for _, name := range []string{"list", "share", "unshare"} {
		if command, _, err := agentgridCmd.Find([]string{name}); err != nil || command.Name() != name {
			t.Fatalf("agentgrid %s is not registered: %v", name, err)
		}
	}
	if command, _, err := rootCmd.Find([]string{"agentgrid"}); err != nil || command.Name() != "agentgrid" {
		t.Fatalf("agentgrid not registered on root: %v", err)
	}
}

func TestAgentgridGuestPathExpandsWorkspacesRoot(t *testing.T) {
	got, err := agentgridGuestPath("/home/dev.guest", "~/workspaces/awscrossreference-main-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/dev.guest/workspaces/awscrossreference-main-abc123" {
		t.Fatalf("guest path = %q", got)
	}
}

func TestAgentgridGuestPathRejectsUnexpectedRoots(t *testing.T) {
	for _, root := range []string{"/etc/passwd", "~/other/x", "~/workspaces/../escape"} {
		if _, err := agentgridGuestPath("/home/dev.guest", root); err == nil {
			t.Fatalf("agentgridGuestPath accepted unsafe root %q", root)
		}
	}
}

func TestAgentgridSelectorIsWorkspaceRelative(t *testing.T) {
	got, err := agentgridSelector("/work/root", "/work/root/apps/api")
	if err != nil {
		t.Fatal(err)
	}
	if got != "apps/api" {
		t.Fatalf("selector = %q", got)
	}
	if _, err := agentgridSelector("/work/root", "/elsewhere/api"); err == nil {
		t.Fatal("selector accepted a path outside the root")
	}
}

func sampleShareables() []shareable {
	return []shareable{
		{Selector: "apps/api", GuestPath: "/home/d/workspaces/api-1", Name: "apps/api"},
		{Selector: "apps/web", GuestPath: "/home/d/workspaces/web-2", Name: "apps/web"},
		{Selector: workspaceSelectorToken, GuestPath: "/home/d/workspaces", Name: "p workspace", Workspace: true},
	}
}

func TestSelectShareablesByName(t *testing.T) {
	chosen, err := selectShareables(sampleShareables(), []string{"apps/web"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chosen) != 1 || chosen[0].Selector != "apps/web" {
		t.Fatalf("chosen = %#v", chosen)
	}
}

func TestSelectShareablesAllExcludesWorkspaceEntry(t *testing.T) {
	chosen, err := selectShareables(sampleShareables(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chosen) != 2 {
		t.Fatalf("--all chose %d, want 2 project entries", len(chosen))
	}
	for _, s := range chosen {
		if s.Workspace {
			t.Fatalf("--all included the whole-workspace entry: %#v", s)
		}
	}
}

func TestSelectShareablesWorkspaceFlagAndToken(t *testing.T) {
	viaFlag, err := selectShareables(sampleShareables(), nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	viaToken, err := selectShareables(sampleShareables(), []string{workspaceSelectorToken}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, chosen := range [][]shareable{viaFlag, viaToken} {
		if len(chosen) != 1 || !chosen[0].Workspace {
			t.Fatalf("workspace selection = %#v", chosen)
		}
	}
}

func TestSelectShareablesDedupesWorkspace(t *testing.T) {
	chosen, err := selectShareables(sampleShareables(), []string{workspaceSelectorToken}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(chosen) != 1 {
		t.Fatalf("workspace selected twice was not deduped: %#v", chosen)
	}
}

func TestSelectShareablesUnknownSelectorFailsClosed(t *testing.T) {
	if _, err := selectShareables(sampleShareables(), []string{"apps/missing"}, false, false); err == nil {
		t.Fatal("unknown selector was accepted")
	}
}

func TestSelectShareablesRequiresSomething(t *testing.T) {
	if _, err := selectShareables(sampleShareables(), nil, false, false); err == nil {
		t.Fatal("empty selection was accepted")
	}
}

func TestAgentgridReadEntriesParsesSentinelWrappedFile(t *testing.T) {
	backend := &vm.MockBackend{
		SSHScriptOut: "banner noise\n__CLH[" +
			`[{"path":"/home/d/workspaces/api-1","name":"apps/api","addedAt":"t"}]` +
			"]CLH__",
	}
	entries, err := agentgridReadEntries(backend, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/home/d/workspaces/api-1" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestAgentgridReadEntriesTreatsMissingFileAsEmpty(t *testing.T) {
	backend := &vm.MockBackend{SSHScriptOut: "__CLH[]CLH__"}
	entries, err := agentgridReadEntries(backend, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %#v, want empty", entries)
	}
}

func TestAgentgridWriteEntriesWritesAtomicallyWithQuotedHeredoc(t *testing.T) {
	backend := &vm.MockBackend{}
	entries := []agentgrid.Entry{{Path: "/home/d/workspaces/api-1", Name: "apps/api", AddedAt: "t"}}
	if err := agentgridWriteEntries(backend, "work", entries); err != nil {
		t.Fatal(err)
	}
	if len(backend.SSHScriptCalls) != 1 {
		t.Fatalf("expected one script call, got %d", len(backend.SSHScriptCalls))
	}
	script := backend.SSHScriptCalls[0].Script
	for _, want := range []string{
		agentgrid.SharedListRelPath,
		"<<'CLOISTER_AGENTGRID_EOF'",
		"mv -f",
		"chmod 600",
		`"/home/d/workspaces/api-1"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("write script missing %q:\n%s", want, script)
		}
	}
	// The JSON payload is delivered inside a single-quoted heredoc so the guest
	// shell performs no expansion on it.
	if strings.Contains(script, "<<\"CLOISTER_AGENTGRID_EOF\"") {
		t.Fatal("heredoc delimiter is not single-quoted")
	}
}
