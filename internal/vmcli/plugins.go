package vmcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CachePlugin describes a plugin found in the shared cache directory.
type CachePlugin struct {
	Name        string
	Marketplace string
	Version     string
	Path        string
}

// PluginKey returns the canonical key used in installed_plugins.json.
func (p CachePlugin) PluginKey() string {
	return p.Name + "@" + p.Marketplace
}

// PluginStatus describes the registration state of a cached plugin.
type PluginStatus struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace"`
	Version     string `json:"version"`
	Status      string `json:"status"`
}

// DiscoverCachePlugins walks the shared plugin cache directory and returns
// all plugins found. Cache structure: cache/<marketplace>/<plugin>/<version>/
func DiscoverCachePlugins(cacheDir string) []CachePlugin {
	var plugins []CachePlugin

	marketplaces, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil
	}

	for _, mp := range marketplaces {
		if !mp.IsDir() || strings.HasPrefix(mp.Name(), ".") {
			continue
		}
		pluginDirs, err := os.ReadDir(filepath.Join(cacheDir, mp.Name()))
		if err != nil {
			continue
		}
		for _, pd := range pluginDirs {
			if !pd.IsDir() || strings.HasPrefix(pd.Name(), ".") {
				continue
			}
			versionDirs, err := os.ReadDir(filepath.Join(cacheDir, mp.Name(), pd.Name()))
			if err != nil {
				continue
			}
			for _, vd := range versionDirs {
				if !vd.IsDir() {
					continue
				}
				plugins = append(plugins, CachePlugin{
					Name:        pd.Name(),
					Marketplace: mp.Name(),
					Version:     vd.Name(),
					Path:        filepath.Join(cacheDir, mp.Name(), pd.Name(), vd.Name()),
				})
				break
			}
		}
	}

	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].Name < plugins[j].Name
	})
	return plugins
}

// installedPluginsFile is the JSON structure of installed_plugins.json.
type installedPluginsFile struct {
	Version int                                  `json:"version"`
	Plugins map[string][]map[string]interface{} `json:"plugins"`
}

// LoadRegisteredPlugins reads a local installed_plugins.json and returns the
// set of registered plugin keys.
func LoadRegisteredPlugins(path string) map[string]bool {
	registered := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return registered
	}
	var doc installedPluginsFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return registered
	}
	for key := range doc.Plugins {
		registered[key] = true
	}
	return registered
}

// PluginStatuses compares cached plugins against the registered set.
func PluginStatuses(cached []CachePlugin, registered map[string]bool) []PluginStatus {
	var statuses []PluginStatus
	for _, p := range cached {
		status := "available"
		if registered[p.PluginKey()] {
			status = "registered"
		}
		statuses = append(statuses, PluginStatus{
			Name:        p.Name,
			Marketplace: p.Marketplace,
			Version:     p.Version,
			Status:      status,
		})
	}
	return statuses
}

// FormatPluginList renders a human-readable table of plugin statuses.
func FormatPluginList(statuses []PluginStatus) string {
	if len(statuses) == 0 {
		return "No plugins found in shared cache.\n"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%-35s %-12s %s\n", "PLUGIN", "VERSION", "STATUS"))
	for _, s := range statuses {
		b.WriteString(fmt.Sprintf("%-35s %-12s %s\n", s.Name+"@"+s.Marketplace, s.Version, s.Status))
	}
	return b.String()
}

// ImportPlugins registers cached plugins into the local installed_plugins.json.
// Idempotent: existing entries are updated, new ones are added.
func ImportPlugins(cached []CachePlugin, indexPath string) error {
	var doc installedPluginsFile
	if data, err := os.ReadFile(indexPath); err == nil {
		json.Unmarshal(data, &doc)
	}
	if doc.Version == 0 {
		doc.Version = 2
	}
	if doc.Plugins == nil {
		doc.Plugins = map[string][]map[string]interface{}{}
	}
	for _, p := range cached {
		key := p.PluginKey()
		entry := map[string]interface{}{
			"scope":       "user",
			"installPath": p.Path,
			"version":     p.Version,
		}
		if existing, ok := doc.Plugins[key]; ok && len(existing) > 0 {
			existing[0]["installPath"] = p.Path
			existing[0]["version"] = p.Version
		} else {
			doc.Plugins[key] = []map[string]interface{}{entry}
		}
	}
	os.MkdirAll(filepath.Dir(indexPath), 0o755)
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(indexPath, data, 0o644)
}
