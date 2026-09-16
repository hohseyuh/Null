package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"
)

// ErrNoteExists is returned by CreateNote when a file already exists at
// the given path.
var ErrNoteExists = errors.New("note already exists")

// ErrNoteNotFound is returned by WriteNote/DeleteNote when no file
// exists yet at the given path.
var ErrNoteNotFound = errors.New("note not found")

// CreateNote, WriteNote, and DeleteNote are the only writes anywhere in
// this codebase — direct writes to the vault itself. Every one of them
// ends in exactly one git commit touching exactly that one file: this is
// the safety model, not a physical inbox staging area. A bad write is a
// `git revert` of one specific commit, never entangled with any other
// change, because there is never more than one change per commit.
// Callers are responsible for running rel through SafeRequestPath(root,
// rel) before calling any function here; none of them re-derive path
// safety themselves, to keep that one gate the single source of truth.

// gitMu serializes every stage-then-commit sequence in this process, so
// two concurrent tool calls can never interleave their git operations
// into a single commit — "one note, one commit" is a guarantee this
// package enforces itself, not a convention callers have to honor.
var gitMu sync.Mutex

// gitSafeDirectoryArgs disables git's ownership check (safe.directory) for
// the single call it's passed on. Without this, a git process running as
// a container user against a bind-mounted volume owned by a different
// host UID — the normal shape of this deployment — refuses to operate at
// all ("detected dubious ownership"). The check exists to stop a git
// command from trusting a repo config planted by a different user at a
// path you merely `cd` into; it doesn't apply here, since NULL_VAULT_PATH
// is one hardcoded, operator-chosen path this process is built to write
// to, not an arbitrary directory it wanders into.
var gitSafeDirectoryArgs = []string{"-c", "safe.directory=*"}

// EnsureGitRepo verifies root is inside a git working tree. Call once at
// boot: every write commits, so a vault that isn't a git repository must
// fail loudly before the server ever starts, never on the first write
// attempt at runtime.
func EnsureGitRepo(root string) error {
	args := append(append([]string{"-C", root}, gitSafeDirectoryArgs...), "rev-parse", "--is-inside-work-tree")
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s is not a git repository (required — every write becomes a commit): %s",
			root, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("%s is not inside a git working tree", root)
	}
	return nil
}

// gitIdentityEnv returns GIT_AUTHOR_*/GIT_COMMITTER_* environment
// entries for the git subprocess, so commits succeed regardless of
// whether the running environment has any git identity configured on
// disk. NULL_MCP_GIT_NAME/NULL_MCP_GIT_EMAIL override the defaults.
func gitIdentityEnv() []string {
	name := os.Getenv("NULL_MCP_GIT_NAME")
	if name == "" {
		name = "nullmcp"
	}
	email := os.Getenv("NULL_MCP_GIT_EMAIL")
	if email == "" {
		email = "nullmcp@localhost"
	}
	return []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
	}
}

func runGit(root string, args ...string) (string, error) {
	fullArgs := append(append([]string{"-C", root}, gitSafeDirectoryArgs...), args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Env = append(os.Environ(), gitIdentityEnv()...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commitPath stages exactly rel (an add or a modify) and commits it with
// message, holding gitMu for the whole stage-then-commit sequence.
func commitPath(root, rel, message string) error {
	gitMu.Lock()
	defer gitMu.Unlock()
	if out, err := runGit(root, "add", "--", rel); err != nil {
		return fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := runGit(root, "commit", "-m", message, "--", rel); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// commitDelete stages rel's removal (git rm both deletes the file and
// stages the deletion) and commits it, same serialization as commitPath.
func commitDelete(root, rel, message string) error {
	gitMu.Lock()
	defer gitMu.Unlock()
	if out, err := runGit(root, "rm", "--", rel); err != nil {
		return fmt.Errorf("git rm: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := runGit(root, "commit", "-m", message, "--", rel); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// commitMessage builds a concise, imperative-mood message — this
// codebase's own commit convention, applied to commits it makes itself —
// with an optional caller-supplied reason appended as the body, and a
// Source trailer identifying which tool made the commit.
func commitMessage(action, rel, reason, tool string) string {
	msg := action + " " + rel
	if reason != "" {
		msg += "\n\n" + reason
	}
	msg += "\n\nSource: nullmcp " + tool
	return msg
}

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

// CreateNote writes a new note at root/rel and commits it — "Add <rel>",
// plus reason if given, in its own commit. Fails with ErrNoteExists if a
// file is already there — use WriteNote to overwrite deliberately.
// Parent directories are created as needed. rel must already have
// passed SafeRequestPath(root, rel).
func CreateNote(root, rel string, frontmatter map[string]any, body, reason string) error {
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
	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", rel, err)
	}

	if err := commitPath(root, rel, commitMessage("Add", rel, reason, "create_note")); err != nil {
		return fmt.Errorf("commit %s: %w", rel, err)
	}
	return nil
}

// WriteNote overwrites an existing note at root/rel wholesale — the full
// new frontmatter and body replace whatever was there — and commits it
// as "Update <rel>". Fails with ErrNoteNotFound if nothing exists yet at
// that path — use CreateNote for a new note. rel must already have
// passed SafeRequestPath(root, rel).
func WriteNote(root, rel string, frontmatter map[string]any, body, reason string) error {
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
	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", rel, err)
	}

	if err := commitPath(root, rel, commitMessage("Update", rel, reason, "write_note")); err != nil {
		return fmt.Errorf("commit %s: %w", rel, err)
	}
	return nil
}

// DeleteNote removes an existing note at root/rel and commits the
// removal as "Delete <rel>". Fails with ErrNoteNotFound if nothing
// exists at that path. rel must already have passed
// SafeRequestPath(root, rel). There is no confirmation step beyond the
// tool call itself — git is the undo mechanism (a single git revert of
// this exact commit), by design.
func DeleteNote(root, rel, reason string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); err != nil {
		if os.IsNotExist(err) {
			return ErrNoteNotFound
		}
		return fmt.Errorf("stat %s: %w", rel, err)
	}

	if err := commitDelete(root, rel, commitMessage("Delete", rel, reason, "delete_note")); err != nil {
		return fmt.Errorf("delete %s: %w", rel, err)
	}
	return nil
}

// PushVault pushes root's current branch to its configured upstream.
// Fails with git's own error text (e.g. a non-fast-forward rejection)
// rather than attempting to resolve anything itself — force-pushing or
// merging automatically is not this function's call to make; a human
// looking at the actual conflicting history is.
func PushVault(root string) (string, error) {
	gitMu.Lock()
	defer gitMu.Unlock()
	out, err := runGit(root, "push")
	if err != nil {
		return out, fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(out))
	}
	return out, nil
}
