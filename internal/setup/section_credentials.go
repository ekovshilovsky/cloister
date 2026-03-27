package setup

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/pflag"
)

// credentialFlags registers CLI flags for non-interactive credential setup.
func credentialFlags(fs *pflag.FlagSet) {
	// Credential store type is auto-detected; no section-specific flags needed.
}

// runCredentials handles the credentials wizard section: detects or sets up the
// credential store, generates a keychain password, and stores VM user credentials.
func runCredentials(ctx *SetupContext) error {
	// Step 1: Detect or set up credential store if not already resolved.
	if ctx.Creds == nil {
		store, storeType, err := setupCredentialStore(ctx.Interactive)
		if err != nil {
			return fmt.Errorf("credential store setup: %w", err)
		}
		ctx.Creds = store
		ctx.State.CredentialStore = storeType
		if err := SaveState(ctx.StatePath, ctx.State); err != nil {
			return fmt.Errorf("persisting credential store choice: %w", err)
		}
		ctx.Progress.MarkComplete("credentials", "store_detect")
		SaveProgress(ctx.ProgressPath, ctx.Progress)
	}

	// Step 2: Generate and store keychain password.
	if !ctx.State.Credentials.KeychainPassword {
		if err := setupKeychainPassword(ctx); err != nil {
			ctx.Progress.MarkFailed("credentials", "keychain_password", err.Error())
			SaveProgress(ctx.ProgressPath, ctx.Progress)
			return err
		}
	} else {
		fmt.Println("  ✓ Keychain password already configured")
	}

	// Step 3: Store VM user credentials.
	if !ctx.State.Credentials.VMLumeUser {
		if err := storeVMCredentials(ctx); err != nil {
			ctx.Progress.MarkFailed("credentials", "vm_users", err.Error())
			SaveProgress(ctx.ProgressPath, ctx.Progress)
			return err
		}
	} else {
		fmt.Println("  ✓ VM user credentials already stored")
	}

	return nil
}

// setupCredentialStore detects 1Password CLI or prompts the user to choose
// between installing it and using local file storage.
func setupCredentialStore(interactive bool) (CredentialStore, string, error) {
	fmt.Println("  Checking for 1Password CLI...")

	if IsOpAvailable() {
		fmt.Println("  ✓ 1Password CLI found")
		return NewOpStore(), "op", nil
	}

	if !interactive {
		fmt.Println("  ⚠ 1Password CLI not found — using local credential storage")
		store, err := NewDefaultLocalStore()
		return store, "local", err
	}

	fmt.Println("  ⚠ 1Password CLI (op) not found.")
	fmt.Println()
	fmt.Println("  1Password stores all credentials securely with Touch ID.")
	fmt.Println("  Without it, credentials are stored locally at ~/.cloister/keys/")
	fmt.Println()
	fmt.Println("  [1] Install 1Password CLI (recommended)")
	fmt.Println("  [2] Store credentials locally")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("  > ")
	line, _ := reader.ReadString('\n')
	choice := strings.TrimSpace(line)

	if choice == "1" {
		fmt.Println()
		fmt.Println("  Running: brew install 1password-cli")
		cmd := exec.Command("brew", "install", "1password-cli")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, "", fmt.Errorf("installing 1password-cli: %w", err)
		}
		if IsOpAvailable() {
			fmt.Println("  ✓ 1Password CLI installed")
			return NewOpStore(), "op", nil
		}
		return nil, "", fmt.Errorf("1password-cli installed but op not found on PATH")
	}

	store, err := NewDefaultLocalStore()
	return store, "local", err
}

// setupKeychainPassword generates a random password, resets the VM's login
// keychain to use it, and stores the password in the credential store.
func setupKeychainPassword(ctx *SetupContext) error {
	fmt.Println("  Generating keychain password...")

	// Generate random password inside the VM.
	out, err := ctx.Backend.SSHCommand(ctx.Profile, "openssl rand -base64 32")
	if err != nil {
		return fmt.Errorf("generating keychain password: %w", err)
	}
	password := strings.TrimSpace(out)

	// Write to credential store before applying to the VM. If the store write
	// fails, abort before changing the VM password to avoid an unrecoverable
	// state where the password is changed but not recorded.
	if err := ctx.Creds.Set(ctx.Profile, "keychain_password", password); err != nil {
		return fmt.Errorf("storing keychain password: %w", err)
	}

	// Attempt to change the keychain password. Try the default 'lume' password
	// first. If it has drifted (common after headless setup), delete and recreate
	// the login keychain with the new password.
	kcPath := "~/Library/Keychains/login.keychain-db"
	resetCmd := fmt.Sprintf("security set-keychain-password -o lume -p %q %s", password, kcPath)
	if _, err := ctx.Backend.SSHCommand(ctx.Profile, resetCmd); err != nil {
		fmt.Println("  ⚠ Current keychain password unknown — recreating login keychain")
		recreateCmd := fmt.Sprintf(
			"security delete-keychain %s 2>/dev/null; security create-keychain -p %q %s && security default-keychain -s %s",
			kcPath, password, kcPath, kcPath,
		)
		if _, err := ctx.Backend.SSHCommand(ctx.Profile, recreateCmd); err != nil {
			return fmt.Errorf("recreating login keychain: %w", err)
		}
	}

	ctx.State.Credentials.KeychainPassword = true
	if err := SaveState(ctx.StatePath, ctx.State); err != nil {
		return fmt.Errorf("persisting keychain password state: %w", err)
	}
	ctx.Progress.MarkComplete("credentials", "keychain_password")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	fmt.Println("  ✓ Keychain password generated and stored")
	return nil
}

// storeVMCredentials stores the lume admin and openclaw user passwords in the
// credential store. For the openclaw user, a new random password is generated
// and applied because the original password from BaseUserSteps may be unknown.
func storeVMCredentials(ctx *SetupContext) error {
	fmt.Println("  Storing VM credentials...")

	// Store lume user credentials (default password from base image).
	if err := ctx.Creds.Set(ctx.Profile, "vm_lume_password", "lume"); err != nil {
		return fmt.Errorf("storing lume user credentials: %w", err)
	}
	ctx.State.Credentials.VMLumeUser = true
	SaveState(ctx.StatePath, ctx.State)

	// Check if the openclaw user exists, then generate and set a known password.
	out, err := ctx.Backend.SSHCommand(ctx.Profile, "id openclaw >/dev/null 2>&1 && echo exists || echo missing")
	if err == nil && strings.TrimSpace(out) == "exists" {
		pwOut, err := ctx.Backend.SSHCommand(ctx.Profile, "openssl rand -base64 32")
		if err != nil {
			return fmt.Errorf("generating openclaw user password: %w", err)
		}
		openclawPassword := strings.TrimSpace(pwOut)

		// Store before applying — same credential sync invariant as keychain.
		if err := ctx.Creds.Set(ctx.Profile, "vm_openclaw_password", openclawPassword); err != nil {
			return fmt.Errorf("storing openclaw user credentials: %w", err)
		}

		chpassCmd := fmt.Sprintf("sudo -n sysadminctl -resetPasswordFor openclaw -newPassword %q", openclawPassword)
		if _, err := ctx.Backend.SSHCommand(ctx.Profile, chpassCmd); err != nil {
			return fmt.Errorf("resetting openclaw user password: %w", err)
		}
	} else {
		// User doesn't exist yet — store a placeholder.
		if err := ctx.Creds.Set(ctx.Profile, "vm_openclaw_password", "not-created"); err != nil {
			return fmt.Errorf("storing openclaw user placeholder: %w", err)
		}
	}

	ctx.State.Credentials.VMOpenClawUser = true
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("credentials", "vm_users")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	fmt.Println("  ✓ lume admin credentials stored")
	fmt.Println("  ✓ openclaw user credentials stored")
	return nil
}
