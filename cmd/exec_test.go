package cmd

import (
	"errors"
	"testing"

	"cloister.io/internal/vm"
)

func TestErrorRenderingPolicy(t *testing.T) {
	if ShouldPrintError(errSilentExit) {
		t.Error("the transparent exec sentinel would be printed")
	}
	if !ShouldPrintError(errors.New("provision failed")) {
		t.Error("an ordinary command error would be hidden")
	}
}

func TestShellJoinArgsPreservesGuestCommandArgv(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "compound guest script",
			argv: []string{"bash", "-lc", `printf "%s\n" "$HOME"; test -d "$HOME/workspaces"`},
			want: `'bash' '-lc' 'printf "%s\n" "$HOME"; test -d "$HOME/workspaces"'`,
		},
		{
			name: "git commit message",
			argv: []string{"git", "commit", "-m", "two words and it's quoted"},
			want: `'git' 'commit' '-m' 'two words and it'"'"'s quoted'`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shellJoinArgs(test.argv); got != test.want {
				t.Fatalf("shellJoinArgs() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExecCommandPreservesArgv(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "guest shell script retains redirects and variables",
			argv: []string{"bash", "-lc", `printf "home=%s\n" "$HOME"; printf "%s\n" "$USER" > "$HOME/result file"; printf "%s\n" "$(uname -s)"`},
			want: `'bash' '-lc' 'printf "home=%s\n" "$HOME"; printf "%s\n" "$USER" > "$HOME/result file"; printf "%s\n" "$(uname -s)"'`,
		},
		{
			name: "spaces and quotes remain in their original arguments",
			argv: []string{"some binary", "two words", `it's "quoted"`, ""},
			want: `'some binary' 'two words' 'it'"'"'s "quoted"' ''`,
		},
		{
			name: "plain argv remains separate",
			argv: []string{"some-binary", "arg1", "arg2"},
			want: `'some-binary' 'arg1' 'arg2'`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &vm.MockBackend{}
			command := shellJoinArgs(tt.argv)
			if _, err := backend.SSHCommand("work", command); err != nil {
				t.Fatal(err)
			}

			if len(backend.SSHCommandCalls) != 1 {
				t.Fatalf("SSHCommand calls = %d, want 1", len(backend.SSHCommandCalls))
			}
			got := backend.SSHCommandCalls[0]
			if got.Profile != "work" {
				t.Errorf("profile = %q, want %q", got.Profile, "work")
			}
			if got.Command != tt.want {
				t.Errorf("remote command = %q, want %q", got.Command, tt.want)
			}
		})
	}
}
