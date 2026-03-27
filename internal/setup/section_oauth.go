package setup

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/pflag"
)

// OAuth flag values are read from ctx.Flags, populated by the cmd layer.

// oauthFlags registers CLI flags for non-interactive OAuth setup.
func oauthFlags(fs *pflag.FlagSet) {
	// Flags are registered in cmd/setup_openclaw.go.
}

// runOAuth handles the Google OAuth wizard section: copies client credentials
// to the VM, sets up an SSH tunnel for the OAuth callback, and authenticates
// Google services via gog.
func runOAuth(ctx *SetupContext) error {
	if len(ctx.State.OAuth.GoogleServices) > 0 {
		fmt.Printf("  ✓ Google OAuth already configured (%s)\n",
			strings.Join(ctx.State.OAuth.GoogleServices, ", "))
		return nil
	}

	if !ctx.Interactive && ctx.Flags.SkipGoogleOAuth {
		fmt.Println("  Skipping Google OAuth (--skip-google-oauth)")
		return nil
	}

	fmt.Println("  Google OAuth Setup")
	fmt.Println("  ──────────────────")
	fmt.Println()
	fmt.Println("  OpenClaw uses Google OAuth for Gmail, Calendar, Drive,")
	fmt.Println("  Contacts, Docs, and Sheets access.")

	// Step 1: Copy client_secret.json to the VM and register credentials.
	if err := setupClientCredentials(ctx); err != nil {
		ctx.Progress.MarkFailed("oauth", "client_credentials", err.Error())
		SaveProgress(ctx.ProgressPath, ctx.Progress)
		return err
	}

	// Step 2: Establish an SSH tunnel so the OAuth redirect URI resolves back
	// to the VM's gog listener from the user's Mac browser.
	port, cleanup, err := setupOAuthTunnel(ctx)
	if err != nil {
		ctx.Progress.MarkFailed("oauth", "ssh_tunnel", err.Error())
		SaveProgress(ctx.ProgressPath, ctx.Progress)
		return err
	}
	defer cleanup()

	// Step 3: Authenticate Google services via the gog CLI inside the VM.
	if err := authenticateGoogleServices(ctx, port); err != nil {
		ctx.Progress.MarkFailed("oauth", "google_auth", err.Error())
		SaveProgress(ctx.ProgressPath, ctx.Progress)
		return err
	}

	return nil
}

// setupClientCredentials prompts for or reads the Google OAuth client_secret.json,
// persists it to the credential store, SCPs it to the VM, and registers it with gog.
func setupClientCredentials(ctx *SetupContext) error {
	fmt.Println()

	var clientSecretPath string

	if ctx.Interactive {
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("  You need a Google Cloud OAuth client_secret.json.")
		fmt.Println("  See: https://openclaw.ai/docs/google-setup")
		fmt.Println()

		if ctx.State.CredentialStore == "op" {
			fmt.Println("  [1] Retrieve client_secret.json from 1Password")
			fmt.Println("  [2] Provide path to client_secret.json on this Mac")
			fmt.Println()
			fmt.Print("  > ")
			line, _ := reader.ReadString('\n')
			choice := strings.TrimSpace(line)

			if choice == "1" {
				val, err := ctx.Creds.Get(ctx.Profile, "google_client_secret")
				if err == nil && val != "" {
					// Write the retrieved JSON to a temp file so it can be
					// transferred to the VM via scp.
					tmpFile, err := os.CreateTemp("", "client_secret_*.json")
					if err != nil {
						return fmt.Errorf("creating temp file for client secret: %w", err)
					}
					defer os.Remove(tmpFile.Name())
					if _, err := tmpFile.WriteString(val); err != nil {
						return fmt.Errorf("writing client secret to temp file: %w", err)
					}
					tmpFile.Close()
					clientSecretPath = tmpFile.Name()
					fmt.Println("  ✓ Retrieved from 1Password")
				} else {
					fmt.Println("  ⚠ Not found in 1Password — provide path instead")
					choice = "2"
				}
			}
			if choice == "2" {
				fmt.Print("  Path: ")
				pathLine, _ := reader.ReadString('\n')
				clientSecretPath = strings.TrimSpace(pathLine)
			}
		} else {
			fmt.Print("  Path to client_secret.json: ")
			pathLine, _ := reader.ReadString('\n')
			clientSecretPath = strings.TrimSpace(pathLine)
		}
	} else {
		clientSecretPath = ctx.Flags.GoogleClientSecret
		if clientSecretPath == "" {
			fmt.Println("  Skipping Google OAuth (no --google-client-secret provided)")
			return nil
		}
	}

	if clientSecretPath == "" {
		return fmt.Errorf("client_secret.json path is required")
	}

	// Expand a leading ~ to the user's home directory.
	if strings.HasPrefix(clientSecretPath, "~/") {
		home, _ := os.UserHomeDir()
		clientSecretPath = home + clientSecretPath[1:]
	}

	// Read the file and cache it in the credential store so future re-runs can
	// retrieve it without prompting.
	data, err := os.ReadFile(clientSecretPath)
	if err != nil {
		return fmt.Errorf("reading client_secret.json: %w", err)
	}
	if ctx.Creds != nil {
		ctx.Creds.Set(ctx.Profile, "google_client_secret", string(data))
	}

	// Resolve SSH access parameters for the target VM.
	sshAccess := ctx.Backend.SSHConfig(ctx.Profile)
	var scpArgs []string
	if sshAccess.ConfigFile != "" {
		scpArgs = append(scpArgs, "-F", sshAccess.ConfigFile)
	}
	if sshAccess.KeyFile != "" {
		scpArgs = append(scpArgs, "-i", sshAccess.KeyFile)
	}

	destUser := sshAccess.User
	if destUser == "" {
		destUser = "lume"
	}
	destHost := sshAccess.Host
	if sshAccess.HostAlias != "" {
		destHost = sshAccess.HostAlias
	}

	scpArgs = append(scpArgs,
		"-o", "StrictHostKeyChecking=no",
		clientSecretPath,
		fmt.Sprintf("%s@%s:~/client_secret.json", destUser, destHost),
	)

	fmt.Println("  Copying to VM...")
	scpCmd := exec.Command("scp", scpArgs...)
	if out, err := scpCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copying client_secret.json to VM: %w\n%s", err, string(out))
	}

	// Register the credentials file with the gog CLI inside the VM.
	// GOG_KEYRING_PASSWORD is needed so the file-backed keyring can store
	// the credentials without prompting for a password.
	var credKRPassword string
	if ctx.Creds != nil && ctx.Creds.Has(ctx.Profile, "keychain_password") {
		credKRPassword, _ = ctx.Creds.Get(ctx.Profile, "keychain_password")
	}
	credSetCmd := fmt.Sprintf("GOG_KEYRING=file GOG_KEYRING_PASSWORD=%q gog auth credentials set ~/client_secret.json", credKRPassword)
	if _, err := ctx.Backend.SSHCommand(ctx.Profile, credSetCmd); err != nil {
		return fmt.Errorf("registering gog credentials: %w", err)
	}

	fmt.Println("  ✓ Client credentials configured")
	ctx.Progress.MarkComplete("oauth", "client_credentials")
	SaveProgress(ctx.ProgressPath, ctx.Progress)
	return nil
}

// findAvailablePort selects a random port in the ephemeral range 49152–60999 and
// verifies availability by attempting a transient TCP listen on the host.
// The restricted upper bound avoids collisions with commonly allocated high ports.
func findAvailablePort() (int, error) {
	const (
		portRangeMin = 49152
		portRangeMax = 60999
	)
	for attempts := 0; attempts < 20; attempts++ {
		n, err := rand.Int(rand.Reader, big.NewInt(portRangeMax-portRangeMin+1))
		if err != nil {
			return 0, fmt.Errorf("generating random port: %w", err)
		}
		port := int(n.Int64()) + portRangeMin

		// Confirm no existing process holds the port before returning it.
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // Port already in use; try again.
		}
		ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("could not find an available port after 20 attempts")
}

// setupOAuthTunnel creates an SSH port-forward from the host Mac to the VM so
// that the Google OAuth redirect URI (http://localhost:<port>/...) is delivered
// to the gog listener running inside the VM. Returns the forwarded port and a
// cleanup function that tears down the tunnel process on exit.
func setupOAuthTunnel(ctx *SetupContext) (int, func(), error) {
	port, err := findAvailablePort()
	if err != nil {
		return 0, nil, err
	}

	sshAccess := ctx.Backend.SSHConfig(ctx.Profile)

	var sshArgs []string
	if sshAccess.ConfigFile != "" {
		sshArgs = append(sshArgs, "-F", sshAccess.ConfigFile)
	}
	if sshAccess.KeyFile != "" {
		sshArgs = append(sshArgs, "-i", sshAccess.KeyFile)
	}

	destHost := sshAccess.Host
	if sshAccess.HostAlias != "" {
		destHost = sshAccess.HostAlias
	}

	// -N: do not execute a remote command (tunnel only).
	// -f: background the ssh process after authentication.
	// -L: forward the chosen port from host to the same port on the VM.
	sshArgs = append(sshArgs,
		"-o", "StrictHostKeyChecking=no",
		"-N", "-f",
		"-L", fmt.Sprintf("%d:localhost:%d", port, port),
		fmt.Sprintf("%s@%s", sshAccess.User, destHost),
	)

	tunnelCmd := exec.Command("ssh", sshArgs...)
	if out, err := tunnelCmd.CombinedOutput(); err != nil {
		return 0, nil, fmt.Errorf("setting up SSH tunnel on port %d: %w\n%s", port, err, string(out))
	}

	fmt.Printf("  Port %d forwarded (VM → host) for OAuth redirect\n", port)
	fmt.Println()
	fmt.Println("  The OAuth flow redirects to localhost. This tunnel forwards")
	fmt.Println("  the callback from your Mac's browser back to the VM where")
	fmt.Println("  gog is listening.")

	cleanup := func() {
		// Tear down the background tunnel by matching its argument signature.
		exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L.*%d:localhost:%d", port, port)).Run()
	}

	return port, cleanup, nil
}

// capitalize returns s with its first byte uppercased. Used in place of the
// deprecated strings.Title for single-word service name display.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// authenticateGoogleServices collects the user's Google email, unlocks the VM
// keychain, and invokes gog auth add with the correct services and listen
// address for the SSH-tunneled OAuth callback.
func authenticateGoogleServices(ctx *SetupContext, port int) error {
	fmt.Println()
	fmt.Println("  Authenticating Google services...")

	// Collect the Google email address for the OAuth flow.
	var email string
	if ctx.Interactive {
		reader := bufio.NewReader(os.Stdin)
		fmt.Println()
		fmt.Print("  Google account email: ")
		line, _ := reader.ReadString('\n')
		email = strings.TrimSpace(line)
		if email == "" {
			return fmt.Errorf("Google email is required for OAuth")
		}
	} else {
		email = ctx.Flags.GoogleEmail
		if email == "" {
			return fmt.Errorf("--google-email is required for Google OAuth setup")
		}
	}

	// Store the email for future re-runs.
	if ctx.Creds != nil {
		ctx.Creds.Set(ctx.Profile, "google_email", email)
	}

	// Unlock the VM login keychain so gog can store OAuth tokens without
	// prompting for the user's password mid-flow.
	if ctx.Creds != nil && ctx.Creds.Has(ctx.Profile, "keychain_password") {
		kcPassword, err := ctx.Creds.Get(ctx.Profile, "keychain_password")
		if err == nil {
			unlockCmd := fmt.Sprintf("security unlock-keychain -p %q ~/Library/Keychains/login.keychain-db", kcPassword)
			ctx.Backend.SSHCommand(ctx.Profile, unlockCmd)
		}
	}

	// Build the gog auth add command with the email, requested services, and
	// a listen address that routes through the SSH tunnel. GOG_KEYRING_PASSWORD
	// is set so the file-backed keyring can encrypt tokens without a TTY prompt.
	services := "gmail,calendar,drive,contacts,docs,sheets"
	var keyringPassword string
	if ctx.Creds != nil && ctx.Creds.Has(ctx.Profile, "keychain_password") {
		keyringPassword, _ = ctx.Creds.Get(ctx.Profile, "keychain_password")
	}
	authCmd := fmt.Sprintf(
		"GOG_KEYRING=file GOG_KEYRING_PASSWORD=%q gog auth add %q --services %s --listen-addr 0.0.0.0:%d --force-consent",
		keyringPassword, email, services, port,
	)

	// Always use SSHInteractive for OAuth — gog prints the authorization URL
	// to stdout and the user must see it in real time to open it in their
	// browser before the flow times out.
	fmt.Println()
	fmt.Println("  Starting Google OAuth flow...")
	fmt.Println("  Copy the URL below into your browser to authorize:")
	fmt.Println()
	if err := ctx.Backend.SSHInteractive(ctx.Profile, authCmd); err != nil {
		return fmt.Errorf("gog auth failed: %w", err)
	}

	// Record the authorized services in setup state.
	serviceList := strings.Split(services, ",")
	fmt.Println()
	fmt.Println("  Registering services:")
	for _, svc := range serviceList {
		fmt.Printf("    ✓ %s\n", capitalize(svc))
	}

	ctx.State.OAuth.GoogleServices = serviceList
	ctx.State.Credentials.GoogleOAuth = true
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("oauth", "google_auth")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	return nil
}
