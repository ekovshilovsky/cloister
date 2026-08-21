// Proprietary and confidential. All rights reserved.

package cmd

import "testing"

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
