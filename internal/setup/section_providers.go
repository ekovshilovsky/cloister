package setup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// Provider flag values are read from ctx.Flags, populated by the cmd layer.

// providerFlags registers CLI flags for non-interactive provider setup.
func providerFlags(fs *pflag.FlagSet) {
	// Flags are registered in cmd/setup_openclaw.go.
}

// runProviders handles the providers wizard section: detects Ollama, selects
// the default provider (Ollama vs Anthropic), and collects optional API keys.
func runProviders(ctx *SetupContext) error {
	// Step 1: Ollama auto-detection.
	if !ctx.State.Providers.Ollama.Configured {
		if err := detectOllama(ctx); err != nil {
			// Ollama detection failure is non-fatal; user may not have Ollama.
			fmt.Fprintf(os.Stderr, "  ⚠ Ollama detection: %v\n", err)
		}
	} else {
		fmt.Printf("  ✓ Ollama already configured (%s, model: %s)\n",
			ctx.State.Providers.Ollama.Host, ctx.State.Providers.Ollama.PrimaryModel)
	}

	// Step 2: Default provider selection. Re-run if a flag override is provided
	// (e.g. switching from ollama to anthropic on a re-run).
	flagOverride := ctx.Flags.DefaultProvider != "" && ctx.Flags.DefaultProvider != ctx.State.Providers.DefaultProvider
	if ctx.State.Providers.DefaultProvider == "" || flagOverride {
		if err := selectDefaultProvider(ctx); err != nil {
			ctx.Progress.MarkFailed("providers", "default_provider", err.Error())
			SaveProgress(ctx.ProgressPath, ctx.Progress)
			return err
		}
	} else {
		fmt.Printf("  ✓ Default provider: %s\n", ctx.State.Providers.DefaultProvider)
	}

	// Step 3: Optional additional API keys (interactive only).
	if ctx.Interactive {
		collectOptionalAPIKeys(ctx)
	}

	return nil
}

// detectOllama checks if Ollama is reachable on the VM's bridge gateway IP
// and registers it as a provider in OpenClaw's config.
func detectOllama(ctx *SetupContext) error {
	fmt.Println("  Checking for Ollama on host...")

	// Detect the host IP from the VM's default gateway (bridge IP).
	out, err := ctx.Backend.SSHCommand(ctx.Profile, "route -n get default 2>/dev/null | awk '/gateway:/{print $2}'")
	if err != nil {
		return fmt.Errorf("detecting host IP: %w", err)
	}
	hostIP := strings.TrimSpace(out)
	if hostIP == "" {
		return fmt.Errorf("could not determine host IP from VM gateway")
	}

	ollamaURL := fmt.Sprintf("http://%s:11434", hostIP)

	// Check if Ollama is reachable.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(ollamaURL + "/api/tags")
	if err != nil {
		fmt.Printf("  ⚠ Ollama not reachable at %s\n", ollamaURL)
		return nil
	}
	defer resp.Body.Close()

	fmt.Printf("  ✓ Ollama reachable at %s:11434\n", hostIP)

	// Parse available models.
	var tagsResp struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		return fmt.Errorf("parsing Ollama model list: %w", err)
	}

	if len(tagsResp.Models) > 0 {
		fmt.Println()
		fmt.Println("  Available models:")
		for i, m := range tagsResp.Models {
			sizeGB := float64(m.Size) / (1024 * 1024 * 1024)
			fmt.Printf("    [%d] %s (%.1f GB)\n", i+1, m.Name, sizeGB)
		}
	}

	// Select or pull a model.
	var selectedModel string
	if ctx.Interactive {
		reader := bufio.NewReader(os.Stdin)

		if len(tagsResp.Models) == 0 {
			fmt.Println()
			fmt.Print("  ⚠ No models loaded. Pull qwen3:32b (recommended)? [Y/n] ")
			line, _ := reader.ReadString('\n')
			if strings.TrimSpace(line) == "" || strings.EqualFold(strings.TrimSpace(line), "y") {
				fmt.Println("  Pulling qwen3:32b on host...")
				pullCmd := exec.Command("ollama", "pull", "qwen3:32b")
				pullCmd.Stdout = os.Stdout
				pullCmd.Stderr = os.Stderr
				if err := pullCmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr, "  ⚠ Failed to pull model: %v\n", err)
				}
				selectedModel = "qwen3:32b"
			}
		} else {
			fmt.Println()
			fmt.Printf("  Select primary model [1-%d] (Enter for %s): ", len(tagsResp.Models), tagsResp.Models[0].Name)
			line, _ := reader.ReadString('\n')
			choice := strings.TrimSpace(line)
			if choice == "" {
				selectedModel = tagsResp.Models[0].Name
			} else {
				var idx int
				fmt.Sscan(choice, &idx)
				if idx >= 1 && idx <= len(tagsResp.Models) {
					selectedModel = tagsResp.Models[idx-1].Name
				} else {
					selectedModel = tagsResp.Models[0].Name
				}
			}
		}
	} else {
		if ctx.Flags.OllamaModel != "" {
			selectedModel = ctx.Flags.OllamaModel
		} else if len(tagsResp.Models) > 0 {
			selectedModel = tagsResp.Models[0].Name
		} else {
			selectedModel = "qwen3:32b"
		}
	}

	if selectedModel == "" {
		return nil
	}

	// Register Ollama provider in OpenClaw config via SSH.
	registerCmd := fmt.Sprintf(`python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('models', {}).setdefault('providers', {})['ollama'] = {
    'baseUrl': 'http://%s:11434',
    'apiKey': 'ollama-local',
    'api': 'ollama',
    'models': []
}
cfg.setdefault('auth', {}).setdefault('profiles', {})['ollama:default'] = {
    'provider': 'ollama',
    'mode': 'api_key'
}
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
"`, hostIP)

	if _, err := ctx.Backend.SSHCommand(ctx.Profile, registerCmd); err != nil {
		return fmt.Errorf("registering Ollama provider in VM: %w", err)
	}

	ctx.State.Providers.Ollama.Configured = true
	ctx.State.Providers.Ollama.Host = fmt.Sprintf("%s:11434", hostIP)
	ctx.State.Providers.Ollama.PrimaryModel = selectedModel
	SaveState(ctx.StatePath, ctx.State)

	ctx.Progress.MarkComplete("providers", "ollama_detect")
	SaveProgress(ctx.ProgressPath, ctx.Progress)

	fmt.Printf("  ✓ Ollama registered (model: %s)\n", selectedModel)
	return nil
}

// selectDefaultProvider lets the user choose between Ollama (local) and
// Anthropic (cloud) as the default AI provider.
func selectDefaultProvider(ctx *SetupContext) error {
	fmt.Println()
	fmt.Println("  Choose your default AI provider:")
	fmt.Println()

	var choice string

	if ctx.Interactive {
		reader := bufio.NewReader(os.Stdin)

		ollamaLabel := "Ollama (local, no cost, fan noise under load)"
		if ctx.State.Providers.Ollama.Configured {
			ollamaLabel = fmt.Sprintf("Ollama — %s (local, no cost, fan noise under load)", ctx.State.Providers.Ollama.PrimaryModel)
		}
		fmt.Printf("  [1] %s\n", ollamaLabel)
		fmt.Println("  [2] Anthropic — Claude (cloud, API key required, quiet)")
		fmt.Println()
		fmt.Print("  > ")
		line, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(line)
	} else {
		if ctx.Flags.DefaultProvider != "" {
			choice = ctx.Flags.DefaultProvider
		} else if ctx.State.Providers.Ollama.Configured {
			choice = "1"
		} else {
			choice = "2"
		}
	}

	switch choice {
	case "1", "ollama":
		ctx.State.Providers.DefaultProvider = "ollama"
		// Set the default model in OpenClaw config.
		if ctx.State.Providers.Ollama.PrimaryModel != "" {
			modelCmd := fmt.Sprintf(`python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('agents', {}).setdefault('defaults', {}).setdefault('model', {})['primary'] = 'ollama/%s'
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
"`, ctx.State.Providers.Ollama.PrimaryModel)
			ctx.Backend.SSHCommand(ctx.Profile, modelCmd)
		}
		fmt.Println("  ✓ Default provider set to: ollama")

	case "2", "anthropic":
		if err := setupAnthropicKey(ctx); err != nil {
			return err
		}
		ctx.State.Providers.DefaultProvider = "anthropic"
		// Set the default model in OpenClaw config.
		modelCmd := `python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('agents', {}).setdefault('defaults', {}).setdefault('model', {})['primary'] = 'anthropic/claude-sonnet-4-6'
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
"`
		ctx.Backend.SSHCommand(ctx.Profile, modelCmd)
		fmt.Println("  ✓ Default provider set to: anthropic/claude-sonnet-4-6")

	default:
		return fmt.Errorf("invalid provider selection: %q", choice)
	}

	SaveState(ctx.StatePath, ctx.State)
	ctx.Progress.MarkComplete("providers", "default_provider")
	SaveProgress(ctx.ProgressPath, ctx.Progress)
	return nil
}

// setupAnthropicKey collects the Anthropic API key from the user or 1Password
// and stores it in the credential store and OpenClaw config.
func setupAnthropicKey(ctx *SetupContext) error {
	fmt.Println()
	var apiKey string

	if ctx.Interactive {
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("  Anthropic API key setup:")
		fmt.Println()
		fmt.Println("  [1] Enter API key manually")
		fmt.Println("  [2] Retrieve from 1Password")
		fmt.Println()
		fmt.Print("  > ")
		line, _ := reader.ReadString('\n')
		choice := strings.TrimSpace(line)

		if choice == "2" && ctx.State.CredentialStore == "op" {
			val, err := ctx.Creds.Get(ctx.Profile, "anthropic_api_key")
			if err == nil && val != "" {
				apiKey = val
				fmt.Println("  ✓ Retrieved from 1Password")
			} else {
				fmt.Println("  ⚠ Not found in 1Password — enter manually")
				choice = "1"
			}
		}

		if choice == "1" || apiKey == "" {
			fmt.Print("  API key: ")
			keyLine, _ := reader.ReadString('\n')
			apiKey = strings.TrimSpace(keyLine)
		}
	} else {
		apiKey = ctx.Flags.AnthropicAPIKey
		if apiKey == "" {
			return fmt.Errorf("--anthropic-api-key is required when selecting Anthropic as default provider")
		}
	}

	if apiKey == "" {
		return fmt.Errorf("Anthropic API key is required")
	}

	// Store immediately.
	if err := ctx.Creds.Set(ctx.Profile, "anthropic_api_key", apiKey); err != nil {
		return fmt.Errorf("storing Anthropic API key: %w", err)
	}
	ctx.State.Credentials.AnthropicAPIKey = true
	SaveState(ctx.StatePath, ctx.State)

	// Write Anthropic provider config to OpenClaw.
	registerCmd := fmt.Sprintf(`python3 -c "
import json, os
cfg_path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(cfg_path) as f:
    cfg = json.load(f)
cfg.setdefault('models', {}).setdefault('providers', {})['anthropic'] = {
    'apiKey': '%s',
    'api': 'anthropic',
    'models': []
}
with open(cfg_path, 'w') as f:
    json.dump(cfg, f, indent=2)
"`, apiKey)

	if _, err := ctx.Backend.SSHCommand(ctx.Profile, registerCmd); err != nil {
		return fmt.Errorf("registering Anthropic provider in VM: %w", err)
	}

	ctx.State.Providers.Anthropic.Configured = true
	SaveState(ctx.StatePath, ctx.State)

	return nil
}

// collectOptionalAPIKeys prompts for optional API keys (OpenAI, Google Places)
// that enhance OpenClaw skills but are not required.
func collectOptionalAPIKeys(ctx *SetupContext) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println()
	fmt.Println("  Optional API keys (skip any with Enter):")

	// OpenAI.
	if !ctx.State.Credentials.OpenAIAPIKey {
		fmt.Print("  OpenAI API key: ")
		line, _ := reader.ReadString('\n')
		key := strings.TrimSpace(line)
		if key != "" {
			ctx.Creds.Set(ctx.Profile, "openai_api_key", key)
			ctx.State.Credentials.OpenAIAPIKey = true
			SaveState(ctx.StatePath, ctx.State)
			fmt.Println("  ✓ OpenAI API key stored")
		}
	}

	// Google Places.
	if !ctx.State.Credentials.GooglePlacesAPIKey {
		fmt.Print("  Google Places API key: ")
		line, _ := reader.ReadString('\n')
		key := strings.TrimSpace(line)
		if key != "" {
			ctx.Creds.Set(ctx.Profile, "google_places_api_key", key)
			ctx.State.Credentials.GooglePlacesAPIKey = true
			SaveState(ctx.StatePath, ctx.State)
			fmt.Println("  ✓ Google Places API key stored")
		}
	}
}
