package colima

import (
	"slices"
	"strings"
	"testing"

	"cloister.io/internal/vm"
)

func TestStartArgsNeverActivateHostDockerContext(t *testing.T) {
	workspace := vm.Mount{Location: "/workspace", Writable: true}
	spec := vm.StartSpec{
		CPUs:              4,
		MemoryGB:          4,
		DiskGB:            40,
		WorkspaceProvider: vm.VirtiofsWorkspace,
		WorkspaceMount:    &workspace,
	}
	args, err := startArgs("work", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "--activate=false") {
		t.Errorf("start args %q must pass --activate=false so the host docker context is left alone", args)
	}
}

func TestDockerContextNameRoundTrip(t *testing.T) {
	if got := DockerContextName("work"); got != "colima-cloister-work" {
		t.Errorf("DockerContextName(work) = %q", got)
	}
	cases := map[string]string{
		"colima-cloister-work": "work",
		"colima-cloister-":     "",
		"colima-camofox":       "",
		"desktop-linux":        "",
		"cloister-work":        "",
	}
	for in, want := range cases {
		if got := ProfileFromDockerContext(in); got != want {
			t.Errorf("ProfileFromDockerContext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseDockerContexts(t *testing.T) {
	out := []byte(`{"Current":false,"Description":"colima [profile=cloister-work]","DockerEndpoint":"unix:///x","Error":"","Name":"colima-cloister-work"}
{"Current":true,"Description":"Docker Desktop","DockerEndpoint":"unix:///y","Error":"","Name":"desktop-linux"}
`)
	ctxs, err := parseDockerContexts(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctxs) != 2 || ctxs[0].Name != "colima-cloister-work" || ctxs[0].Current || ctxs[1].Name != "desktop-linux" || !ctxs[1].Current {
		t.Errorf("unexpected parse result: %+v", ctxs)
	}
	if _, err := parseDockerContexts([]byte("not json\n")); err == nil {
		t.Error("expected an error for malformed output")
	}
	if ctxs, err := parseDockerContexts([]byte("  \n")); err != nil || len(ctxs) != 0 {
		t.Errorf("empty output should yield no contexts, got %+v, %v", ctxs, err)
	}
}

func TestOrphanDockerContextsOnlyTouchesCloisterContexts(t *testing.T) {
	ctxs := []DockerContext{
		{Name: "colima-cloister-work"},
		{Name: "colima-cloister-personal"},
		{Name: "colima-camofox"},
		{Name: "colima-cloister-"},
		{Name: "desktop-linux"},
	}
	got := OrphanDockerContexts(ctxs, []string{"cloister-work", "camofox-unrelated"})
	want := []string{"colima-cloister-personal"}
	if !slices.Equal(got, want) {
		t.Errorf("OrphanDockerContexts = %q, want %q", got, want)
	}
}

func TestPreferredHostDockerContext(t *testing.T) {
	if got := PreferredHostDockerContext([]DockerContext{{Name: "default"}, {Name: "desktop-linux"}}); got != "desktop-linux" {
		t.Errorf("want desktop-linux when Docker Desktop is registered, got %q", got)
	}
	if got := PreferredHostDockerContext([]DockerContext{{Name: "default"}, {Name: "colima-cloister-work"}}); got != "default" {
		t.Errorf("want default fallback, got %q", got)
	}
}

func TestDockerContextAdvice(t *testing.T) {
	lookup := func(profile string) (exists, running bool) {
		switch profile {
		case "work":
			return true, true
		case "innolumi":
			return true, false
		}
		return false, false
	}
	cases := []struct {
		current string
		want    string
	}{
		{"desktop-linux", ""},
		{"colima-camofox", ""},
		{"colima-cloister-work", ""},
		{"colima-cloister-innolumi", "stopped"},
		{"colima-cloister-personal", "no longer exists"},
	}
	for _, c := range cases {
		got := DockerContextAdvice(c.current, "desktop-linux", lookup)
		if c.want == "" {
			if got != "" {
				t.Errorf("current=%q: expected no advice, got %q", c.current, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) || !strings.Contains(got, "docker context use desktop-linux") {
			t.Errorf("current=%q: advice %q should mention %q and the fix", c.current, got, c.want)
		}
	}
}
