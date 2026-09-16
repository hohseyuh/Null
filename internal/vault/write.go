package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

// ErrNoteExists is returned by CreateNote when a file already exists at
// the given path.
var ErrNoteExists = errors.New("note already exists")

// ErrNoteNotFound is returned by WriteNote when no file exists yet at
// the given path.
var ErrNoteNotFound = errors.New("note not found")

// This file is the only place in the codebase that opens a note file for
// writing. It is used solely against the inbox root — never the vault
// root, which every other read path in this codebase opens strictly
// O_RDONLY (see readNoteFile in index.go). Callers are responsible for
// running rel through SafeRequestPath(root, rel) before calling either
// function here; neither function re-derives path safety itself, to
// keep that one gate the single source of truth.

// serialize renders frontmatter and body back into a note file: a YAML
// front-matter fence (omitted entirely if frontmatter is empty) followed
// by the body verbatim. It assumes body does not itself begin with a
// "---" fence, which would be ambiguous on the next parse.
func serialize(frontmatter map[string]any, body string) ([]byte, error) {
	var buf bytes.Buffer
	if len(frontmatter) > 0 {
		y, err := yaml.Marshal(frontmatter)
		if err != nil {
			return nil, fmt.Errorf("marshal frontmatter: %w", err)
		}
		buf.WriteString("---\n")
		buf.Write(y)
		buf.WriteString("---\n\n")
	}
	buf.WriteString(body)
	return buf.Bytes(), nil
}

// CreateNote writes a new note at root/rel. It fails with ErrNoteExists
// if a file is already there — use WriteNote to overwrite deliberately.
// Parent directories are created as needed. rel must already have passed
// SafeRequestPath(root, rel).
func CreateNote(root, rel string, frontmatter map[string]any, body string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); err == nil {
		return ErrNoteExists
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", rel, err)
	}

	content, err := serialize(frontmatter, body)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", rel, err)
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrNoteExists
		}
		return fmt.Errorf("create %s: %w", rel, err)
	}
	defer f.Close()
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}

// WriteNote overwrites an existing note at root/rel wholesale — the full
// new frontmatter and body replace whatever was there. It fails with
// ErrNoteNotFound if nothing exists yet at that path — use CreateNote
// for a new note. rel must already have passed SafeRequestPath(root, rel).
func WriteNote(root, rel string, frontmatter map[string]any, body string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if st, err := os.Lstat(abs); err != nil {
		if os.IsNotExist(err) {
			return ErrNoteNotFound
		}
		return fmt.Errorf("stat %s: %w", rel, err)
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", rel)
	}

	content, err := serialize(frontmatter, body)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", rel, err)
	}
	defer f.Close()
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}
