// Proprietary and confidential. All rights reserved.

package colima

import (
	"reflect"
	"testing"
)

func TestSSHShellArgsPreserveCompoundScriptAsOneQuotedArgument(t *testing.T) {
	script := `target="$HOME/workspaces/urlprocessorapi-39aeb6a46515"; mkdir -p -- "$target" && test -d "$target" && test ! -L "$target"`
	want := []string{
		"ssh", "--profile", "cloister-work", "--", "bash", "-lc",
		`'target="$HOME/workspaces/urlprocessorapi-39aeb6a46515"; mkdir -p -- "$target" && test -d "$target" && test ! -L "$target"'`,
	}
	if got := sshShellArgs("cloister-work", script); !reflect.DeepEqual(got, want) {
		t.Fatalf("sshShellArgs() = %#v, want %#v", got, want)
	}
}

func TestSSHShellArgsEscapeEmbeddedSingleQuote(t *testing.T) {
	got := sshShellArgs("cloister-work", `printf '%s\n' "it's intact"`)
	wantScript := `'printf '"'"'%s\n'"'"' "it'"'"'s intact"'`
	if got[len(got)-1] != wantScript {
		t.Fatalf("quoted script = %q, want %q", got[len(got)-1], wantScript)
	}
}
