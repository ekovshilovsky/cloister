package provision

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// embeddedScripts enumerates all scripts that must be present in the embedded
// filesystem. This list is the authoritative record of what provisioning
// delivers and should be kept in sync with the scripts/ directory.
var embeddedScripts = []string{
	"scripts/base.sh",
	"scripts/stack-web.sh",
	"scripts/stack-cloud.sh",
	"scripts/stack-python.sh",
	"scripts/stack-dotnet.sh",
	"scripts/stack-go.sh",
	"scripts/stack-rust.sh",
	"scripts/stack-data.sh",
	"scripts/stack-ollama.sh",
	"scripts/read-only-mounts.sh",
	"scripts/gpg-setup.sh",
}

// embeddedTemplates enumerates all templates that must be present in the
// embedded filesystem.
var embeddedTemplates = []string{
	"templates/bashrc.tmpl",
	"templates/gitconfig.tmpl",
}

// TestEmbeddedScriptsExist verifies that every required provisioning script
// was embedded at compile time and can be read without error.
func TestEmbeddedScriptsExist(t *testing.T) {
	t.Parallel()
	for _, path := range embeddedScripts {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			data, err := Scripts.ReadFile(path)
			if err != nil {
				t.Fatalf("Scripts.ReadFile(%q): %v", path, err)
			}
			if len(data) == 0 {
				t.Fatalf("Scripts.ReadFile(%q): file is empty", path)
			}
		})
	}
}

// TestEmbeddedTemplatesExist verifies that every required configuration
// template was embedded at compile time and can be read without error.
func TestEmbeddedTemplatesExist(t *testing.T) {
	t.Parallel()
	for _, path := range embeddedTemplates {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			data, err := Templates.ReadFile(path)
			if err != nil {
				t.Fatalf("Templates.ReadFile(%q): %v", path, err)
			}
			if len(data) == 0 {
				t.Fatalf("Templates.ReadFile(%q): file is empty", path)
			}
		})
	}
}

// TestBashrcTemplateParses verifies that bashrc.tmpl is syntactically valid
// Go template syntax and renders without error for both GPG-signing and
// non-GPG-signing profiles.
func TestBashrcTemplateParses(t *testing.T) {
	t.Parallel()

	raw, err := Templates.ReadFile("templates/bashrc.tmpl")
	if err != nil {
		t.Fatalf("reading bashrc.tmpl: %v", err)
	}

	tmpl, err := template.New("bashrc").Parse(string(raw))
	if err != nil {
		t.Fatalf("parsing bashrc.tmpl: %v", err)
	}

	cases := []struct {
		name string
		data bashrcTemplateData
	}{
		{
			name: "gpg_signing_enabled",
			data: bashrcTemplateData{
				Profile:    "dev",
				StartDir:   "~/code/myproject",
				GPGSigning: true,
			},
		},
		{
			name: "gpg_signing_disabled",
			data: bashrcTemplateData{
				Profile:    "work",
				StartDir:   "~/code",
				GPGSigning: false,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, tc.data); err != nil {
				t.Fatalf("executing bashrc.tmpl with data %+v: %v", tc.data, err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.data.Profile) {
				t.Errorf("rendered bashrc missing profile name %q", tc.data.Profile)
			}
			if !strings.Contains(out, tc.data.StartDir) {
				t.Errorf("rendered bashrc missing start dir %q", tc.data.StartDir)
			}
			if tc.data.GPGSigning && !strings.Contains(out, "GNUPGHOME") {
				t.Errorf("rendered bashrc missing GNUPGHOME when GPGSigning=true")
			}
			if !tc.data.GPGSigning && strings.Contains(out, "GNUPGHOME") {
				t.Errorf("rendered bashrc contains GNUPGHOME when GPGSigning=false")
			}
		})
	}
}

// TestBashrcTemplateGPGTTYCorrect ensures the bashrc template uses the correct
// GPG_TTY variable name (not the former typo GPP_TTY).
func TestBashrcTemplateGPGTTYCorrect(t *testing.T) {
	t.Parallel()

	raw, err := Templates.ReadFile("templates/bashrc.tmpl")
	if err != nil {
		t.Fatalf("reading bashrc.tmpl: %v", err)
	}

	content := string(raw)
	if strings.Contains(content, "GPP_TTY") {
		t.Error("bashrc.tmpl contains typo GPP_TTY; should be GPG_TTY")
	}
	if !strings.Contains(content, "GPG_TTY") {
		t.Error("bashrc.tmpl is missing GPG_TTY export")
	}
}

// TestGitconfigTemplateParses verifies that gitconfig.tmpl is syntactically
// valid Go template syntax and renders correctly for both GPG-signing and
// non-GPG-signing configurations.
func TestGitconfigTemplateParses(t *testing.T) {
	t.Parallel()

	raw, err := Templates.ReadFile("templates/gitconfig.tmpl")
	if err != nil {
		t.Fatalf("reading gitconfig.tmpl: %v", err)
	}

	tmpl, err := template.New("gitconfig").Parse(string(raw))
	if err != nil {
		t.Fatalf("parsing gitconfig.tmpl: %v", err)
	}

	type gitconfigData struct {
		GitName    string
		GitEmail   string
		GPGSigning bool
		GPGKeyID   string
	}

	cases := []struct {
		name string
		data gitconfigData
	}{
		{
			name: "with_gpg_signing",
			data: gitconfigData{
				GitName:    "Alice Example",
				GitEmail:   "alice@example.com",
				GPGSigning: true,
				GPGKeyID:   "DEADBEEF12345678",
			},
		},
		{
			name: "without_gpg_signing",
			data: gitconfigData{
				GitName:    "Bob Example",
				GitEmail:   "bob@example.com",
				GPGSigning: false,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, tc.data); err != nil {
				t.Fatalf("executing gitconfig.tmpl with data %+v: %v", tc.data, err)
			}
			out := buf.String()
			if !strings.Contains(out, tc.data.GitName) {
				t.Errorf("rendered gitconfig missing name %q", tc.data.GitName)
			}
			if !strings.Contains(out, tc.data.GitEmail) {
				t.Errorf("rendered gitconfig missing email %q", tc.data.GitEmail)
			}
			if tc.data.GPGSigning {
				if !strings.Contains(out, "gpgsign = true") {
					t.Errorf("rendered gitconfig missing gpgsign=true when GPGSigning=true")
				}
				if !strings.Contains(out, tc.data.GPGKeyID) {
					t.Errorf("rendered gitconfig missing GPG key ID %q", tc.data.GPGKeyID)
				}
			} else {
				if strings.Contains(out, "gpgsign") {
					t.Errorf("rendered gitconfig contains gpgsign when GPGSigning=false")
				}
			}
		})
	}
}

// TestBashrcDataDefaults verifies that bashrcData substitutes the fallback
// start directory when the profile configuration leaves StartDir empty.
func TestBashrcDataDefaults(t *testing.T) {
	t.Parallel()

	p := &config.Profile{
		GPGSigning: false,
		// StartDir intentionally left empty to exercise the default path.
	}
	data := bashrcData("myprofile", p)

	if data.Profile != "myprofile" {
		t.Errorf("Profile = %q; want %q", data.Profile, "myprofile")
	}
	if data.StartDir != "~/code" {
		t.Errorf("StartDir = %q; want %q", data.StartDir, "~/code")
	}
	if data.GPGSigning {
		t.Errorf("GPGSigning = true; want false")
	}
}

// TestBashrcDataCustomStartDir verifies that a non-empty StartDir in the
// profile is preserved verbatim in the template data.
func TestBashrcDataCustomStartDir(t *testing.T) {
	t.Parallel()

	p := &config.Profile{
		StartDir:   "~/code/myproject",
		GPGSigning: true,
	}
	data := bashrcData("work", p)

	if data.StartDir != "~/code/myproject" {
		t.Errorf("StartDir = %q; want %q", data.StartDir, "~/code/myproject")
	}
	if !data.GPGSigning {
		t.Errorf("GPGSigning = false; want true")
	}
}

// TestScriptShebangAndPipefail verifies that each embedded shell script begins
// with a bash shebang and, where applicable, enables strict error handling.
// Scripts that explicitly relax error handling (read-only-mounts.sh) are
// excluded from the pipefail check since they are intentionally lenient.
func TestScriptShebangAndPipefail(t *testing.T) {
	t.Parallel()

	// Scripts that deliberately omit set -euo pipefail because their logic
	// relies on best-effort commands (mount, etc.).
	pipefailExempt := map[string]bool{
		"scripts/read-only-mounts.sh": true,
	}

	for _, path := range embeddedScripts {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			data, err := Scripts.ReadFile(path)
			if err != nil {
				t.Fatalf("Scripts.ReadFile(%q): %v", path, err)
			}
			content := string(data)
			if !strings.HasPrefix(content, "#!/bin/bash") {
				t.Errorf("%s: missing #!/bin/bash shebang", path)
			}
			if !pipefailExempt[path] && !strings.Contains(content, "set -euo pipefail") {
				t.Errorf("%s: missing 'set -euo pipefail' strict mode", path)
			}
		})
	}
}

// TestCheckHostAvailable verifies that checkHost returns true when a TCP
// listener is accepting connections on the target port.
func TestCheckHostAvailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test listener: %v", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	if !checkHost("127.0.0.1", port, 500*time.Millisecond) {
		t.Error("checkHost should return true for a listening port")
	}
}

// TestCheckHostUnavailable verifies that checkHost returns false when no
// process is listening on the target port within the given timeout.
func TestCheckHostUnavailable(t *testing.T) {
	// Port 59999 is above the registered range and is almost certainly unused
	// in a CI or developer environment.
	if checkHost("127.0.0.1", 59999, 100*time.Millisecond) {
		t.Error("checkHost should return false for a non-listening port")
	}
}

// TestAssembleScriptWithEnv verifies that assembleScriptWithEnv prepends the
// export line to the embedded script content and that the resulting string
// contains the expected script body.
func TestAssembleScriptWithEnv(t *testing.T) {
	script, err := assembleScriptWithEnv("scripts/read-only-mounts.sh", "CLOISTER_HEADLESS=1")
	if err != nil {
		t.Fatalf("assembleScriptWithEnv: %v", err)
	}
	if !strings.HasPrefix(script, "export CLOISTER_HEADLESS=1\n") {
		t.Error("assembled script should start with the export line")
	}
	if !strings.Contains(script, "READONLY_DIRS=") {
		t.Error("assembled script should contain read-only-mounts.sh body content")
	}
}
