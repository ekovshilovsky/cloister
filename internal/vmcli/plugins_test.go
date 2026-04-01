package vmcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverCachePlugins(t *testing.T) {
	tmp := t.TempDir()
	cache := filepath.Join(tmp, "cache", "test-marketplace")
	os.MkdirAll(filepath.Join(cache, "plugin-a", "1.0.0"), 0o755)
	os.MkdirAll(filepath.Join(cache, "plugin-b", "2.0.0"), 0o755)

	plugins := DiscoverCachePlugins(filepath.Join(tmp, "cache"))

	if len(plugins) != 2 {
		t.Fatalf("DiscoverCachePlugins returned %d plugins, want 2", len(plugins))
	}

	names := map[string]bool{}
	for _, p := range plugins {
		names[p.Name] = true
	}
	if !names["plugin-a"] || !names["plugin-b"] {
		t.Errorf("expected plugin-a and plugin-b, got: %v", names)
	}
}

func TestPluginListStatus(t *testing.T) {
	tmp := t.TempDir()
	cache := filepath.Join(tmp, "cache", "test-marketplace")
	os.MkdirAll(filepath.Join(cache, "my-plugin", "1.0.0"), 0o755)

	indexDir := filepath.Join(tmp, "plugins")
	os.MkdirAll(indexDir, 0o755)
	indexData := []byte(`{"version":2,"plugins":{"my-plugin@test-marketplace":[{"scope":"user","installPath":"/home/test/.claude/plugins/cache/test-marketplace/my-plugin/1.0.0","version":"1.0.0"}]}}`)
	os.WriteFile(filepath.Join(indexDir, "installed_plugins.json"), indexData, 0o644)

	cached := DiscoverCachePlugins(filepath.Join(tmp, "cache"))
	registered := LoadRegisteredPlugins(filepath.Join(indexDir, "installed_plugins.json"))
	statuses := PluginStatuses(cached, registered)

	if len(statuses) != 1 {
		t.Fatalf("PluginStatuses returned %d entries, want 1", len(statuses))
	}
	if statuses[0].Status != "registered" {
		t.Errorf("expected status 'registered', got %q", statuses[0].Status)
	}
}

func TestImportPlugins(t *testing.T) {
	tmp := t.TempDir()
	cache := filepath.Join(tmp, "cache", "test-marketplace")
	os.MkdirAll(filepath.Join(cache, "plugin-a", "1.0.0"), 0o755)
	os.MkdirAll(filepath.Join(cache, "plugin-b", "2.0.0"), 0o755)

	indexPath := filepath.Join(tmp, "installed_plugins.json")
	cached := DiscoverCachePlugins(filepath.Join(tmp, "cache"))

	err := ImportPlugins(cached, indexPath)
	if err != nil {
		t.Fatalf("ImportPlugins: %v", err)
	}

	registered := LoadRegisteredPlugins(indexPath)
	if !registered["plugin-a@test-marketplace"] {
		t.Error("expected plugin-a@test-marketplace to be registered")
	}
	if !registered["plugin-b@test-marketplace"] {
		t.Error("expected plugin-b@test-marketplace to be registered")
	}

	// Verify idempotency.
	err = ImportPlugins(cached, indexPath)
	if err != nil {
		t.Fatalf("second ImportPlugins: %v", err)
	}

	data, _ := os.ReadFile(indexPath)
	var doc installedPluginsFile
	json.Unmarshal(data, &doc)
	if len(doc.Plugins["plugin-a@test-marketplace"]) != 1 {
		t.Errorf("expected 1 entry for plugin-a, got %d", len(doc.Plugins["plugin-a@test-marketplace"]))
	}
}
