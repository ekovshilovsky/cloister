package terminal

import "testing"

// TestDetect exercises the environment-variable detection table. Each case
// represents one terminal emulator's typical environment shape; the test
// scrubs every signal-bearing variable so that detection cannot accidentally
// pick up a leftover value from the actual host environment running the
// suite.
func TestDetect(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Terminal
	}{
		{
			name: "iTerm2",
			env:  map[string]string{"TERM_PROGRAM": "iTerm.app"},
			want: Terminal{Name: "iterm2", Capabilities: Capabilities{UserVars: true}},
		},
		{
			name: "Ghostty",
			env:  map[string]string{"TERM_PROGRAM": "ghostty"},
			want: Terminal{Name: "ghostty", Capabilities: Capabilities{UserVars: true}},
		},
		{
			name: "WezTerm via TERM_PROGRAM",
			env:  map[string]string{"TERM_PROGRAM": "WezTerm"},
			want: Terminal{Name: "wezterm", Capabilities: Capabilities{UserVars: true}},
		},
		{
			name: "WezTerm via WEZTERM_PANE only",
			env:  map[string]string{"WEZTERM_PANE": "0"},
			want: Terminal{Name: "wezterm", Capabilities: Capabilities{UserVars: true}},
		},
		{
			name: "Warp",
			env:  map[string]string{"TERM_PROGRAM": "WarpTerminal"},
			want: Terminal{Name: "warp"},
		},
		{
			name: "Apple Terminal",
			env:  map[string]string{"TERM_PROGRAM": "Apple_Terminal"},
			want: Terminal{Name: "terminal-app"},
		},
		{
			name: "VS Code integrated terminal",
			env:  map[string]string{"TERM_PROGRAM": "vscode"},
			want: Terminal{Name: "vscode"},
		},
		{
			name: "Kitty via KITTY_WINDOW_ID",
			env:  map[string]string{"KITTY_WINDOW_ID": "1"},
			want: Terminal{Name: "kitty"},
		},
		{
			name: "Kitty via TERM only",
			env:  map[string]string{"TERM": "xterm-kitty"},
			want: Terminal{Name: "kitty"},
		},
		{
			name: "Alacritty via ALACRITTY_LOG",
			env:  map[string]string{"ALACRITTY_LOG": "/tmp/alacritty.log"},
			want: Terminal{Name: "alacritty"},
		},
		{
			name: "unknown terminal",
			env:  nil,
			want: Terminal{},
		},
		{
			name: "TERM_PROGRAM wins over TERM heuristic",
			env: map[string]string{
				"TERM_PROGRAM": "iTerm.app",
				"TERM":         "xterm-kitty",
			},
			want: Terminal{Name: "iterm2", Capabilities: Capabilities{UserVars: true}},
		},
	}

	// Variables that any single test case may set; clear all of them on
	// every iteration so leakage from the host environment or a previous
	// case cannot influence the result.
	signals := []string{
		"TERM_PROGRAM",
		"TERM",
		"KITTY_WINDOW_ID",
		"WEZTERM_PANE",
		"ALACRITTY_LOG",
		"ALACRITTY_WINDOW_ID",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range signals {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := Detect()
			if got != tc.want {
				t.Errorf("Detect() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
