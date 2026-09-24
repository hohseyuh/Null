package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingIsZero(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || f.VaultPath != "" {
		t.Fatalf("Load missing = %+v, %v; want zero, nil", f, err)
	}
}

func TestSaveLoadRoundTripAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	if err := Save(path, File{VaultPath: "/srv/vault"}); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil || f.VaultPath != "/srv/vault" {
		t.Fatalf("Load = %+v, %v", f, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", st.Mode().Perm())
	}
	// overwrite is atomic and leaves no temp files behind
	if err := Save(path, File{VaultPath: "/other"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("expected a parse error, not a silent empty config")
	}
}

func TestVaultPathEnvWinsOverFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	Save(path, File{VaultPath: "/from-file"})

	t.Setenv("NULL_VAULT_PATH", "")
	if p, src, _ := VaultPath(path); p != "/from-file" || src != "file" {
		t.Fatalf("got %q %q", p, src)
	}
	t.Setenv("NULL_VAULT_PATH", "/from-env")
	if p, src, _ := VaultPath(path); p != "/from-env" || src != "env" {
		t.Fatalf("got %q %q", p, src)
	}
	t.Setenv("NULL_VAULT_PATH", "")
	if p, src, _ := VaultPath(filepath.Join(t.TempDir(), "none.json")); p != "" || src != "" {
		t.Fatalf("fresh install: got %q %q, want setup mode", p, src)
	}
}
