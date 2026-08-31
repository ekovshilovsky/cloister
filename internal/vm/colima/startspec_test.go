package colima

import (
	"slices"
	"testing"

	"cloister.io/internal/vm"
)

func TestStartArgsWiresStartSpecMounts(t *testing.T) {
	workspace := vm.Mount{Location: "/workspace", Writable: true}
	spec := vm.StartSpec{
		CPUs:              6,
		MemoryGB:          12,
		DiskGB:            80,
		RootDiskGB:        30,
		MountInotify:      false,
		WorkspaceProvider: vm.VirtiofsWorkspace,
		WorkspaceMount:    &workspace,
		SupplementalMounts: []vm.Mount{
			{Location: "/keys", Writable: false},
		},
	}
	args, err := startArgs("work", spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--cpu", "6", "--memory", "12", "--disk", "80", "--root-disk", "30", "--mount-inotify=false", "/workspace:w", "/keys"} {
		if !slices.Contains(args, want) {
			t.Errorf("start args %q do not contain %q", args, want)
		}
	}
}
