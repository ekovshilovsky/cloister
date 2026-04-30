package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/gpgforward"
)

// pinentryProgramRE matches a non-comment pinentry-program line at the start
// of a line, capturing the existing value so callers can compare.
var pinentryProgramRE = regexp.MustCompile(`(?m)^pinentry-program\s+(\S+)\s*$`)

// pinentryConflictError is returned by ensurePinentryProgram when gpg-agent.conf
// already has a pinentry-program directive with a different value than cloister
// wants to set. The Existing field carries the current value so callers can
// surface it in a confirmation prompt without re-reading the file.
type pinentryConflictError struct {
	Existing string
}

func (e *pinentryConflictError) Error() string {
	return fmt.Sprintf("different pinentry-program already configured: %s", e.Existing)
}

// setupGPGForward runs the host preflight for cloister's gpg-agent forwarding
// feature. It is idempotent and safe to re-run:
//
//  1. Locate or install pinentry-mac (Homebrew).
//  2. Ensure ~/.gnupg/gpg-agent.conf has a matching pinentry-program line,
//     prompting before overwriting an existing different value.
//  3. Reload gpg-agent.
//  4. Resolve and persist the extra-socket path for later tunnel start.
func setupGPGForward() error {
	pinentryPath, err := locatePinentryMac()
	if err != nil {
		return err
	}
	fmt.Printf("✓ pinentry-mac found at %s\n", pinentryPath)

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locating home directory: %w", err)
	}
	confPath := filepath.Join(home, ".gnupg", "gpg-agent.conf")

	if err := os.MkdirAll(filepath.Dir(confPath), 0o700); err != nil {
		return fmt.Errorf("creating ~/.gnupg: %w", err)
	}

	confirmOverwrite := false
	var conflict *pinentryConflictError
	for {
		changed, err := ensurePinentryProgram(confPath, pinentryPath, confirmOverwrite)
		if err == nil {
			if changed {
				fmt.Printf("✓ wrote pinentry-program to %s\n", confPath)
			} else {
				fmt.Printf("✓ %s already configured\n", confPath)
			}
			break
		}
		if !errors.As(err, &conflict) {
			return err
		}
		fmt.Printf("\n%s already sets pinentry-program to:\n  %s\n", confPath, conflict.Existing)
		fmt.Printf("Cloister wants to set it to:\n  %s\n", pinentryPath)
		if !promptYesNo("Overwrite? [y/N] ") {
			return fmt.Errorf("aborted by user — leaving %s unchanged", confPath)
		}
		confirmOverwrite = true
	}

	if err := exec.Command("gpgconf", "--reload", "gpg-agent").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ gpgconf --reload failed (%v); the next gpg invocation will pick up the conf change automatically\n", err)
	}

	socketPath, err := resolveExtraSocket()
	if err != nil {
		return err
	}
	if err := gpgforward.PersistHostSocketPath(home, socketPath); err != nil {
		return fmt.Errorf("persisting host socket path: %w", err)
	}
	fmt.Printf("✓ host extra-socket at %s\n", socketPath)
	fmt.Println("\ngpg-forward host preflight complete.")
	fmt.Println("Run `cloister update <profile>` for any profile with GPGSigning=true.")
	return nil
}

// locatePinentryMac returns the absolute path to pinentry-mac on the host,
// installing it via Homebrew if absent and the user consents.
func locatePinentryMac() (string, error) {
	if path, err := exec.LookPath("pinentry-mac"); err == nil {
		return path, nil
	}
	fmt.Println("pinentry-mac is not installed.")
	if !promptYesNo("Install via `brew install pinentry-mac`? [Y/n] ") {
		return "", fmt.Errorf("pinentry-mac is required and was not installed")
	}
	if err := runCommandInteractive("brew", "install", "pinentry-mac"); err != nil {
		return "", fmt.Errorf("brew install pinentry-mac: %w", err)
	}
	path, err := exec.LookPath("pinentry-mac")
	if err != nil {
		return "", fmt.Errorf("pinentry-mac still not on PATH after install: %w", err)
	}
	return path, nil
}

// ensurePinentryProgram makes ~/.gnupg/gpg-agent.conf contain exactly one
// pinentry-program line set to want. Returns (changed, error). When the file
// already has a pinentry-program line with a different value and
// confirmOverwrite is false, it returns a *pinentryConflictError carrying the
// existing value so callers can prompt the user without re-reading the file.
func ensurePinentryProgram(confPath, want string, confirmOverwrite bool) (bool, error) {
	contents, err := os.ReadFile(confPath)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("reading %s: %w", confPath, err)
	}

	text := string(contents)
	matches := pinentryProgramRE.FindStringSubmatch(text)

	if matches == nil {
		appended := text
		if appended != "" && !strings.HasSuffix(appended, "\n") {
			appended += "\n"
		}
		appended += fmt.Sprintf("pinentry-program %s\n", want)
		if err := os.WriteFile(confPath, []byte(appended), 0o600); err != nil {
			return false, fmt.Errorf("writing %s: %w", confPath, err)
		}
		return true, nil
	}

	if matches[1] == want {
		return false, nil
	}

	if !confirmOverwrite {
		return false, &pinentryConflictError{Existing: matches[1]}
	}

	replaced := pinentryProgramRE.ReplaceAllString(text, fmt.Sprintf("pinentry-program %s", want))
	if err := os.WriteFile(confPath, []byte(replaced), 0o600); err != nil {
		return false, fmt.Errorf("writing %s: %w", confPath, err)
	}
	return true, nil
}

// resolveExtraSocket returns the absolute path of the host gpg-agent's
// restricted (extra) socket, as reported by gpgconf.
func resolveExtraSocket() (string, error) {
	out, err := exec.Command("gpgconf", "--list-dirs", "agent-extra-socket").Output()
	if err != nil {
		return "", fmt.Errorf("gpgconf --list-dirs agent-extra-socket: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("gpgconf returned empty path for agent-extra-socket")
	}
	return path, nil
}
