package linux

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/vm"
)

// macOSPathMap maps macOS-specific PATH entries to their Linux equivalents.
var macOSPathMap = map[string]string{
	"/opt/homebrew/bin":  "/home/linuxbrew/.linuxbrew/bin",
	"/opt/homebrew/sbin": "/home/linuxbrew/.linuxbrew/sbin",
}

// macOSDropPrefixes lists path prefixes that should be removed from env.PATH
// because they have no equivalent on Linux.
var macOSDropPrefixes = []string{
	"/Library/",
	"/System/",
	"/Applications/",
}

// macOSDropEnvKeys lists environment variable keys that are macOS-specific
// and should be removed from the translated settings.
var macOSDropEnvKeys = map[string]bool{
	"HOMEBREW_PREFIX":     true,
	"HOMEBREW_CELLAR":     true,
	"HOMEBREW_REPOSITORY": true,
}

// translatePath replaces the host home prefix with the VM home prefix in an
// absolute path. Paths that do not start with hostHome are returned unchanged.
func translatePath(hostHome, vmHome, path string) string {
	if strings.HasPrefix(path, hostHome) {
		return vmHome + path[len(hostHome):]
	}
	return path
}

// translatePATH splits a colon-delimited PATH string and translates each entry.
func translatePATH(hostHome, vmHome, pathStr string) string {
	entries := strings.Split(pathStr, ":")
	var result []string
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		if replacement, ok := macOSPathMap[entry]; ok {
			result = append(result, replacement)
			continue
		}
		drop := false
		for _, prefix := range macOSDropPrefixes {
			if strings.HasPrefix(entry, prefix) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		result = append(result, translatePath(hostHome, vmHome, entry))
	}
	return strings.Join(result, ":")
}

// TranslateInstalledPlugins reads a host installed_plugins.json and returns a
// version with all installPath and projectPath values translated to VM paths.
func TranslateInstalledPlugins(data []byte, hostHome, vmHome string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var plugins map[string][]map[string]interface{}
	if err := json.Unmarshal(doc["plugins"], &plugins); err != nil {
		return nil, err
	}
	for _, entries := range plugins {
		for _, entry := range entries {
			if path, ok := entry["installPath"].(string); ok {
				entry["installPath"] = translatePath(hostHome, vmHome, path)
			}
			if path, ok := entry["projectPath"].(string); ok {
				entry["projectPath"] = translatePath(hostHome, vmHome, path)
			}
		}
	}
	pluginsJSON, err := json.Marshal(plugins)
	if err != nil {
		return nil, err
	}
	doc["plugins"] = pluginsJSON
	return json.MarshalIndent(doc, "", "  ")
}

// TranslateKnownMarketplaces reads a host known_marketplaces.json and returns
// a version with all installLocation values translated to VM paths.
func TranslateKnownMarketplaces(data []byte, hostHome, vmHome string) ([]byte, error) {
	var doc map[string]map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	for _, marketplace := range doc {
		if loc, ok := marketplace["installLocation"].(string); ok {
			marketplace["installLocation"] = translatePath(hostHome, vmHome, loc)
		}
	}
	return json.MarshalIndent(doc, "", "  ")
}

// TranslateSettings reads a host settings.json and returns a version with
// env.PATH entries translated for Linux and macOS-only env vars removed.
func TranslateSettings(data []byte, hostHome, vmHome string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if envRaw, ok := doc["env"]; ok {
		var env map[string]string
		if err := json.Unmarshal(envRaw, &env); err != nil {
			return nil, err
		}
		if pathStr, ok := env["PATH"]; ok {
			env["PATH"] = translatePATH(hostHome, vmHome, pathStr)
		}
		for key := range macOSDropEnvKeys {
			delete(env, key)
		}
		for key, val := range env {
			env[key] = translatePath(hostHome, vmHome, val)
		}
		envJSON, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		doc["env"] = envJSON
	}
	delete(doc, "$schema")
	delete(doc, "feedbackSurveyState")
	return json.MarshalIndent(doc, "", "  ")
}

// SyncPlugins reads the host's plugin index files and settings, translates
// paths for the target VM, and writes the translated versions into the VM.
// This ensures the VM starts with a working plugin configuration that
// references the correct paths for its filesystem layout.
func SyncPlugins(profile string, hostHome string, backend vm.Backend) error {
	vmHome := vm.VMHome(hostHome)

	// Ensure the plugins directory exists for index files. The cache/ and
	// marketplaces/ subdirectories are virtiofs mounts at the host path
	// managed by Colima — they do not need to be created here.
	mkdirScript := "mkdir -p ~/.claude/plugins"
	if _, err := backend.SSHScript(profile, mkdirScript); err != nil {
		return fmt.Errorf("creating plugins directory: %w", err)
	}

	// Translate and deploy installed_plugins.json.
	installedPath := filepath.Join(hostHome, ".claude", "plugins", "installed_plugins.json")
	if data, err := os.ReadFile(installedPath); err == nil {
		translated, err := TranslateInstalledPlugins(data, hostHome, vmHome)
		if err != nil {
			return fmt.Errorf("translating installed_plugins.json: %w", err)
		}
		script := fmt.Sprintf("cat > ~/.claude/plugins/installed_plugins.json << 'CLOISTER_EOF'\n%s\nCLOISTER_EOF", string(translated))
		if _, err := backend.SSHScript(profile, script); err != nil {
			return fmt.Errorf("writing installed_plugins.json: %w", err)
		}
	}

	// Translate and deploy known_marketplaces.json.
	marketplacesPath := filepath.Join(hostHome, ".claude", "plugins", "known_marketplaces.json")
	if data, err := os.ReadFile(marketplacesPath); err == nil {
		translated, err := TranslateKnownMarketplaces(data, hostHome, vmHome)
		if err != nil {
			return fmt.Errorf("translating known_marketplaces.json: %w", err)
		}
		script := fmt.Sprintf("cat > ~/.claude/plugins/known_marketplaces.json << 'CLOISTER_EOF'\n%s\nCLOISTER_EOF", string(translated))
		if _, err := backend.SSHScript(profile, script); err != nil {
			return fmt.Errorf("writing known_marketplaces.json: %w", err)
		}
	}

	// Translate and deploy settings.json.
	settingsPath := filepath.Join(hostHome, ".claude", "settings.json")
	if data, err := os.ReadFile(settingsPath); err == nil {
		translated, err := TranslateSettings(data, hostHome, vmHome)
		if err != nil {
			return fmt.Errorf("translating settings.json: %w", err)
		}
		script := fmt.Sprintf("cat > ~/.claude/settings.json << 'CLOISTER_EOF'\n%s\nCLOISTER_EOF", string(translated))
		if _, err := backend.SSHScript(profile, script); err != nil {
			return fmt.Errorf("writing settings.json: %w", err)
		}
	}

	// Deploy .claude.json with onboarding marked as complete so that Claude
	// Code skips the first-run setup wizard (theme picker + login prompt).
	// If the host has a .claude.json, copy its theme and relevant settings.
	// Otherwise create a minimal config that bypasses onboarding.
	claudeConfigPath := filepath.Join(hostHome, ".claude", ".claude.json")
	if data, err := os.ReadFile(claudeConfigPath); err == nil {
		// Host has a .claude.json — deploy it with onboarding flags set.
		var cfg map[string]interface{}
		if err := json.Unmarshal(data, &cfg); err == nil {
			cfg["hasCompletedOnboarding"] = true
			cfg["numStartups"] = 1
			translated, err := json.MarshalIndent(cfg, "", "  ")
			if err == nil {
				script := fmt.Sprintf("cat > ~/.claude/.claude.json << 'CLOISTER_EOF'\n%s\nCLOISTER_EOF", string(translated))
				backend.SSHScript(profile, script)
			}
		}
	} else {
		// No host config — create a minimal one to skip onboarding.
		script := `cat > ~/.claude/.claude.json << 'CLOISTER_EOF'
{
  "hasCompletedOnboarding": true,
  "theme": "dark",
  "numStartups": 1
}
CLOISTER_EOF`
		backend.SSHScript(profile, script)
	}

	// Add GitHub's SSH host key to known_hosts so git operations over SSH
	// don't prompt for host authenticity verification.
	backend.SSHScript(profile, `mkdir -p ~/.ssh && ssh-keyscan -t ed25519,rsa github.com >> ~/.ssh/known_hosts 2>/dev/null`)

	return nil
}
