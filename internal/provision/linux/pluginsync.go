package linux

import (
	"encoding/json"
	"strings"
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

// VMHome returns the expected home directory path for a profile inside a
// Colima Linux VM.
func VMHome(profile string) string {
	return "/home/" + profile + ".guest"
}
