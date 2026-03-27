package setup_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/setup"
)

func TestLocalStoreSetAndGet(t *testing.T) {
	store := setup.NewLocalStore(t.TempDir())
	if err := store.Set("myprofile", "telegram_token", "123:ABC"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, err := store.Get("myprofile", "telegram_token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "123:ABC" {
		t.Errorf("Get = %q, want 123:ABC", val)
	}
}

func TestLocalStoreHas(t *testing.T) {
	store := setup.NewLocalStore(t.TempDir())
	if store.Has("myprofile", "missing_key") {
		t.Error("Has should return false for non-existent key")
	}
	if err := store.Set("myprofile", "exists", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !store.Has("myprofile", "exists") {
		t.Error("Has should return true for existing key")
	}
}

func TestLocalStoreGetMissingKey(t *testing.T) {
	store := setup.NewLocalStore(t.TempDir())
	_, err := store.Get("myprofile", "nonexistent")
	if err == nil {
		t.Error("Get should return error for missing key")
	}
}

func TestLocalStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	store := setup.NewLocalStore(dir)
	if err := store.Set("myprofile", "secret", "sensitive-data"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "myprofile", "secret"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestLocalStoreOverwrite(t *testing.T) {
	store := setup.NewLocalStore(t.TempDir())
	store.Set("p", "key", "old")
	store.Set("p", "key", "new")
	val, _ := store.Get("p", "key")
	if val != "new" {
		t.Errorf("Get = %q, want new", val)
	}
}

func TestLocalStoreTrimWhitespace(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "p"), 0o700)
	os.WriteFile(filepath.Join(dir, "p", "key"), []byte("value\n"), 0o600)
	store := setup.NewLocalStore(dir)
	val, _ := store.Get("p", "key")
	if val != "value" {
		t.Errorf("Get = %q, want value", val)
	}
}

func TestIsOpAvailable(t *testing.T) {
	_ = setup.IsOpAvailable() // smoke test — result depends on host
}
