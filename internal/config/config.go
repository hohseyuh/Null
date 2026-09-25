// Package config is the small, persisted choice the setup page makes: which
// directory is the vault. It is a single JSON file, written atomically and
// readable only by its owner, so a fresh install can be configured from
// the browser instead of by editing environment variables. Environment
// still wins — NULL_VAULT_PATH, when set, is an operator decision the UI
// must not override.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// File is the on-disk shape. Unknown keys are ignored on load so a newer
// file never breaks an older binary.
type File struct {
	VaultPath string `json:"vault_path"`
}

// Path returns where the config file lives: NULL_CONFIG_PATH if set, else
// null/config.json under the user's config directory, else ./null-config.json.
func Path() string {
	if p := os.Getenv("NULL_CONFIG_PATH"); p != "" {
		return p
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "null", "config.json")
	}
	return "null-config.json"
}

// Load reads the file at path. A missing file is not an error — it is a
// fresh install — and yields a zero File.
func Load(path string) (File, error) {
	var f File
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return f, nil
}

// Save writes f to path atomically (see WriteJSON).
func Save(path string, f File) error { return WriteJSON(path, f) }

// WriteJSON marshals v and writes it to path atomically (temp file in the
// same directory, then rename) with mode 0600, creating the directory 0700
// if needed. A crash mid-write leaves the previous file intact rather than
// a truncated one. Shared by the saved vault choice and nullmcp's OAuth
// state, the only two things this service ever persists of its own.
func WriteJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".null-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// VaultPath resolves the vault directory and says where the answer came
// from: "env" (NULL_VAULT_PATH, which locks the choice), "file" (the saved
// config), or "" when neither is set and the server should start in setup
// mode.
func VaultPath(configPath string) (path, source string, err error) {
	if v := os.Getenv("NULL_VAULT_PATH"); v != "" {
		return v, "env", nil
	}
	f, err := Load(configPath)
	if err != nil {
		return "", "", err
	}
	if f.VaultPath != "" {
		return f.VaultPath, "file", nil
	}
	return "", "", nil
}
