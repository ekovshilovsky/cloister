// Proprietary and confidential. All rights reserved.

package lume

import (
	"reflect"
	"strings"
	"testing"

	"cloister.io/internal/vm"
)

func TestStartArgsIncludesEveryMount(t *testing.T) {
	workspace := vm.Mount{Location: "/workspace", Writable: true}
	args, err := startArgs("work", vm.StartSpec{
		WorkspaceProvider: vm.VirtiofsWorkspace,
		WorkspaceMount:    &workspace,
		SupplementalMounts: []vm.Mount{
			{Location: "/keys", Writable: false},
			{Location: "/downloads", Writable: false},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "cloister-work", "--no-display", "--shared-dir", "/workspace", "--shared-dir", "/keys:ro", "--shared-dir", "/downloads:ro"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("startArgs() = %#v, want %#v", args, want)
	}
}

func TestStartArgsRejectsRelocatedMount(t *testing.T) {
	_, err := startArgs("work", vm.StartSpec{
		WorkspaceProvider: vm.NoWorkspace,
		SupplementalMounts: []vm.Mount{
			{Location: "/host/keys", MountPoint: "/guest/keys"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "relocated mount") {
		t.Fatalf("startArgs() error = %v, want clear relocated mount rejection", err)
	}
}
