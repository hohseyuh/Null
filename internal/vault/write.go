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
	"time"

	"github.com/goccy/go-yaml"
)

// ErrNoteExists is returned by CreateNote when a file already exists at
// the given path.
var ErrNoteExists = errors.New("note already exists")

// ErrNoteNotFound is returned by WriteNote/DeleteNote/SetTier/
// ProposeTier when no file exists yet at the given path.
var ErrNoteNotFound = errors.New("note not found")

// ErrAsilLocked is returned by any write attempt against an asil note —
// immutable to the model by construction (see spec/tiers.md), not by
// convention: this is the friendly, application-layer half of the
// guarantee, backed by an actual 0444 filesystem permission the index
// maintains (see enforceAsilLock in index.go) as the belt-and-braces half.
var ErrAsilLocked = errors.New("note is asil: immutable to the model")

// ErrTierRaise is R1, as an error: the model can never raise a tier, not
// by any tool, not at creation, not as a side effect. Promotion is a
// human act, via Al-Mina. tier_set returns this rather than silently
// no-opping, because a silent failure teaches the model the call worked.
var ErrTierRaise = errors.New("cannot raise tier: promotion is a human act, via Al-Mina")

// ErrDeleteForbidden covers a delete attempt the permission matrix
// disallows: only a dakhil note — the model's own working space — may be
// deleted by the model.
var ErrDeleteForbidden = errors.New("delete is only permitted on dakhil notes")

// ErrProposalDenied is returned by ProposeTier when the note already
// carries a denial for the same target tier and hasn't been edited since
// — without this, a denied proposal would return within days and Al-Mina
// would become a screen the user stops opening.
var ErrProposalDenied = errors.New("this exact proposal was already denied and the note hasn't changed since")

// CreateNote, WriteNote, DeleteNote, SetTier, and ProposeTier are the
// only writes anywhere in this codebase — direct writes to the vault
// itself. Every one of them ends in exactly one git commit touching
// exactly that one file: this is the safety model, not a physical inbox
// staging area. A bad write is a `git revert` of one specific commit,
// never entangled with any other change, because there is never more
// than one change per commit. Callers are responsible for running rel
// through SafeRequestPath(root, rel) before calling any function here;
// none of them re-derive path safety themselves, to keep that one gate
// the single source of truth.
//
// The tier permission matrix (spec/tiers.md) is enforced here, not in
// the MCP layer: tier, proposed_*, denied_*, and tier_history are
// server-owned frontmatter — stripped from any model-supplied value and
// replaced with the server's own, silently. R1 (the model can never
// raise a tier) and R2 (editing a thabit note demotes it to amil,
// atomically with the edit) are gates that never bend, not instructions
// a caller is trusted to respect. There is deliberately no git push or
// git commit exposed to a model anywhere in this codebase — see
// spec/tiers.md's "One door": the server commits on write, and nothing
// beyond that reaches a remote from inside this process at all.
//
// One refinement on "exactly one commit": WriteNote amends its own
// immediately-preceding commit when that commit was itself an unbroken
// write_note update to the same path (see commitPathAmendable) — editing
// one note ten times in a row inside one session produces one commit,
// not ten, as long as nothing else is committed in between. CreateNote,
// DeleteNote, SetTier, and ProposeTier never amend; each is always its
// own fresh commit.

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

// GitIdentity is the default commit author name for this process — the
// binary's own name, so `git log` shows which of nullmcp or nullapi made
// a commit. Set once at startup, before any write.
var GitIdentity = "nullmcp"

// gitIdentityEnv returns GIT_AUTHOR_*/GIT_COMMITTER_* environment
// entries for the git subprocess, so commits succeed regardless of
// whether the running environment has any git identity configured on
// disk. NULL_GIT_NAME/NULL_GIT_EMAIL (or the older NULL_MCP_GIT_NAME/
// NULL_MCP_GIT_EMAIL) override the defaults.
func gitIdentityEnv() []string {
	name := firstEnv("NULL_GIT_NAME", "NULL_MCP_GIT_NAME")
	if name == "" {
		name = GitIdentity
	}
	email := firstEnv("NULL_GIT_EMAIL", "NULL_MCP_GIT_EMAIL")
	if email == "" {
		email = GitIdentity + "@localhost"
	}
	return []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// runGit runs one git command in root. gitMu serializes writers inside a
// process, but nullapi (Al-Mina) and nullmcp are separate processes
// sharing one vault: when the other one holds git's own index.lock this
// briefly retries instead of failing the write — git's lock guarantees
// they never corrupt each other, only that one has to wait its turn.
func runGit(root string, args ...string) (string, error) {
	fullArgs := append(append([]string{"-C", root}, gitSafeDirectoryArgs...), args...)
	var out []byte
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		cmd := exec.Command("git", fullArgs...)
		cmd.Env = append(os.Environ(), gitIdentityEnv()...)
		out, err = cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "index.lock") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
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

// commitPathAmendable is commitPath's WriteNote-only sibling: if HEAD is
// already a write_note "Update <rel>" commit touching nothing but rel,
// it amends that commit instead of stacking a new one on top. This is
// the fix for "edit the same note ten times in a row" producing ten
// commits — repeated revisions inside one unbroken editing session
// collapse into the single commit that represents where the note ended
// up, with the latest reason (an older one is superseded, not kept).
//
// CreateNote, DeleteNote, SetTier, and ProposeTier never call this — see
// canAmendWriteNoteHEAD for exactly why amending is scoped to
// write_note-on-write_note only. The moment anything else is committed
// in between (a different note, a create, a delete, a human's own
// commit), the chain breaks and the next write_note starts a fresh
// commit — so amending only ever collapses genuinely contiguous edits,
// never reaches back across other changes.
func commitPathAmendable(root, rel, message string) error {
	gitMu.Lock()
	defer gitMu.Unlock()
	if out, err := runGit(root, "add", "--", rel); err != nil {
		return fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(out))
	}

	args := []string{"commit", "-m", message, "--", rel}
	if canAmendWriteNoteHEAD(root, rel) {
		args = []string{"commit", "--amend", "-m", message, "--", rel}
	}
	if out, err := runGit(root, args...); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// canAmendWriteNoteHEAD reports whether HEAD is safe to amend in place
// for a new write_note commit on rel: HEAD's own message must be exactly
// a prior write_note "Update <rel>" commit (checked by header line and
// Source trailer, not just a loose substring match — a coincidental
// prefix match on a differently-named path, e.g. "foo.md" vs
// "foo.md.bak", must not pass), and HEAD must touch nothing but rel.
// Caller must hold gitMu.
func canAmendWriteNoteHEAD(root, rel string) bool {
	msg, err := runGit(root, "log", "-1", "--pretty=%B")
	if err != nil {
		return false
	}
	header, _, _ := strings.Cut(msg, "\n")
	if header != "Update "+rel || !strings.Contains(msg, "\nSource: nullmcp write_note") {
		return false
	}
	out, err := runGit(root, "show", "--name-only", "--pretty=format:", "HEAD")
	if err != nil {
		return false
	}
	changed := strings.Fields(strings.TrimSpace(out))
	return len(changed) == 1 && changed[0] == rel
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

// readCurrentNote reads and parses the note currently on disk at
// root/rel, for callers that need to inspect its existing tier or other
// server-owned frontmatter before writing. Returns ErrNoteNotFound if
// nothing exists yet, and rejects irregular files the same way the other
// write functions do.
func readCurrentNote(root, rel string) (*Note, string, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, abs, ErrNoteNotFound
		}
		return nil, abs, fmt.Errorf("stat %s: %w", rel, err)
	}
	if !st.Mode().IsRegular() {
		return nil, abs, fmt.Errorf("%s: not a regular file", rel)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, abs, fmt.Errorf("read %s: %w", rel, err)
	}
	return Parse(rel, raw, st.ModTime(), nil), abs, nil
}

// rewriteBody returns cur's body ready to hand back to serialize when
// only the frontmatter is changing. serialize puts one blank line between
// the closing fence and the body, and Parse keeps that blank line as the
// body's first byte — so a body read from a note that already has a fence
// must lose exactly one leading newline, or every metadata-only rewrite
// (a tier change, a proposal) would add another blank line under the
// fence, forever.
func rewriteBody(cur *Note) string {
	if cur.BodyLine > 1 {
		return strings.TrimPrefix(cur.Body, "\n")
	}
	return cur.Body
}

// writeFile truncates and rewrites abs's contents in place. Shared by
// every write function below, all of which have already established the
// file exists and passed their own permission checks.
func writeFile(abs, rel string, content []byte) error {
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
	return nil
}

// carryForwardServerOwned copies every server-owned key present in
// current verbatim into fm, so a wholesale model rewrite (write_note's
// contract is a full replace) can never silently erase tier history, an
// outstanding proposal, or a recorded denial just by omitting the field.
// Callers that are deliberately changing one of these keys overwrite it
// afterward.
func carryForwardServerOwned(fm, current map[string]any) {
	for _, k := range serverOwnedFrontmatterKeys {
		if v, ok := current[k]; ok {
			fm[k] = v
		}
	}
}

// appendTierHistory appends one entry to fm's tier_history array,
// creating it if absent — the "frontmatter history" spec/tiers.md
// requires tier_set's reason be written to.
func appendTierHistory(fm map[string]any, from, to Tier, reason, by string) {
	existing, _ := fm["tier_history"].([]any)
	fm["tier_history"] = append(existing, map[string]any{
		"from": string(from), "to": string(to),
		"reason": reason, "by": by, "at": timeString(time.Now()),
	})
}

// CreateNote writes a new note at root/rel and commits it — "Add <rel>",
// plus reason if given, in its own commit. Fails with ErrNoteExists if a
// file is already there — use WriteNote to overwrite one on purpose.
// Parent directories are created as needed. rel must already have passed
// SafeRequestPath(root, rel).
//
// Server-owned frontmatter (tier, proposed_*, denied_*, tier_history) is
// stripped from frontmatter unconditionally — a model has no legitimate
// reason to set any of them on a brand-new note, so it never gets to.
// The note is left with no explicit tier key at all, which parses as
// dakhil by default (ParseTier) — the same outcome as writing tier:
// dakhil explicitly, without adding a field to every note that never
// needed one.
func CreateNote(root, rel string, frontmatter map[string]any, body, reason string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Lstat(abs); err == nil {
		return ErrNoteExists
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", rel, err)
	}

	fm := stripServerOwned(frontmatter)
	content, err := serialize(fm, body)
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
// as "Update <rel>", amending straight into the prior commit instead of
// stacking a new one if that prior commit was itself an unbroken
// write_note update to this same path (see commitPathAmendable). Fails
// with ErrNoteNotFound if nothing exists yet at that path — use
// CreateNote for a new note. Fails with ErrAsilLocked if the note is
// asil — immutable to the model, no exception. rel must already have
// passed SafeRequestPath(root, rel).
//
// Server-owned frontmatter always survives a wholesale write untouched
// by the model: tier, tier_history, and any outstanding proposal or
// denial carry forward from the note's current on-disk state regardless
// of what frontmatter contains, except tier itself, which the R2 rule
// below may change as a deliberate side effect of this same call.
//
// R2: if the note is currently thabit, this edit demotes it to amil, in
// the same write — not a follow-up call, not a flag, one atomic change.
// The returned demoted is true exactly when that happened, so the caller
// can tell the model once, plainly, after the fact.
func WriteNote(root, rel string, frontmatter map[string]any, body, reason string) (demoted bool, err error) {
	current, abs, err := readCurrentNote(root, rel)
	if err != nil {
		return false, err
	}
	if current.Tier == TierAsil {
		return false, ErrAsilLocked
	}

	fm := stripServerOwned(frontmatter)
	carryForwardServerOwned(fm, current.Frontmatter)

	_, hadTier := current.Frontmatter["tier"]
	newTier := current.Tier
	if current.Tier == TierThabit {
		newTier = TierAmil
		demoted = true
	}
	if demoted || hadTier {
		fm["tier"] = string(newTier)
	}
	if demoted {
		appendTierHistory(fm, current.Tier, newTier, "edited while thabit", "write_note (R2 auto-demotion)")
	}

	content, err := serialize(fm, body)
	if err != nil {
		return demoted, err
	}
	if err := writeFile(abs, rel, content); err != nil {
		return demoted, err
	}

	msg := commitMessage("Update", rel, reason, "write_note")
	if demoted {
		msg += "\n\nAuto-demoted thabit -> amil (R2: this note was edited)."
	}
	if err := commitPathAmendable(root, rel, msg); err != nil {
		return demoted, fmt.Errorf("commit %s: %w", rel, err)
	}
	return demoted, nil
}

// DeleteNote removes an existing note at root/rel and commits the
// removal as "Delete <rel>". Fails with ErrNoteNotFound if nothing
// exists at that path, and with ErrDeleteForbidden unless the note is
// dakhil — the permission matrix allows the model to delete only its own
// working space. rel must already have passed SafeRequestPath(root,
// rel). There is no confirmation step beyond the tool call itself — git
// is the undo mechanism (a single git revert of this exact commit), by
// design.
func DeleteNote(root, rel, reason string) error {
	current, _, err := readCurrentNote(root, rel)
	if err != nil {
		return err
	}
	if current.Tier != TierDakhil {
		return fmt.Errorf("%w: %s is %s", ErrDeleteForbidden, rel, current.Tier)
	}

	if err := commitDelete(root, rel, commitMessage("Delete", rel, reason, "delete_note")); err != nil {
		return fmt.Errorf("delete %s: %w", rel, err)
	}
	return nil
}

// SetTier lowers rel's tier, and only lowers it — R1 forbids the model
// from ever raising one, so any call that would raise or hold it returns
// ErrTierRaise rather than silently no-opping. Fails with ErrAsilLocked
// if the note is currently asil: immutable, unconditionally, even to a
// call that would only lower it further. reason is required and is
// appended to the note's own tier_history in frontmatter, not just the
// commit message, so the audit trail survives even a shallow clone. rel
// must already have passed SafeRequestPath(root, rel).
func SetTier(root, rel string, target Tier, reason string) error {
	if reason == "" {
		return fmt.Errorf("reason is required")
	}
	if !target.Valid() {
		return fmt.Errorf("unknown tier %q", target)
	}
	current, abs, err := readCurrentNote(root, rel)
	if err != nil {
		return err
	}
	if current.Tier == TierAsil {
		return ErrAsilLocked
	}
	if target.Rank() >= current.Tier.Rank() {
		return ErrTierRaise
	}

	fm := make(map[string]any, len(current.Frontmatter)+1)
	for k, v := range current.Frontmatter {
		fm[k] = v
	}
	fm["tier"] = string(target)
	appendTierHistory(fm, current.Tier, target, reason, "tier_set")

	content, err := serialize(fm, rewriteBody(current))
	if err != nil {
		return err
	}
	if err := writeFile(abs, rel, content); err != nil {
		return err
	}

	if err := commitPath(root, rel, commitMessage("Set tier", rel, reason, "tier_set")); err != nil {
		return fmt.Errorf("commit %s: %w", rel, err)
	}
	return nil
}

// ProposeTier writes a proposal into rel's frontmatter — proposed_tier,
// proposed_reason, proposed_at — and changes nothing else: not the
// note's own tier, not its body. The user acts on it in Al-Mina; this
// only records the ask. Fails with ErrAsilLocked if the note is asil —
// there is no tier above it to propose. Fails with ErrProposalDenied if
// the note already carries a denial for the same target tier and hasn't
// been written to since (mtime is the "edited since" signal, the same
// one UpdatedAt exposes everywhere else in this codebase) — without
// this, a denied proposal would return within days and Al-Mina would
// become a screen the user stops opening. rel must already have passed
// SafeRequestPath(root, rel).
func ProposeTier(root, rel string, target Tier, reason string) error {
	if !target.Valid() {
		return fmt.Errorf("unknown tier %q", target)
	}
	current, abs, err := readCurrentNote(root, rel)
	if err != nil {
		return err
	}
	if current.Tier == TierAsil {
		return ErrAsilLocked
	}
	if denial := DenialFrom(current.Frontmatter); denial != nil && denial.Tier == target {
		// denied_at round-trips through RFC 3339 (whole-second precision)
		// while mtime keeps sub-second precision, so the deny-write's own
		// mtime can look nanoseconds "after" its own denied_at — truncate
		// both to the second before comparing, or a proposal could be
		// resubmitted in the same instant it was denied.
		// The two-second slack covers a deny-write whose mtime lands in the
		// next second after its own denied_at was stamped.
		if !current.UpdatedAt.Truncate(time.Second).After(denial.At.Add(2 * time.Second)) {
			return ErrProposalDenied
		}
	}

	fm := make(map[string]any, len(current.Frontmatter)+3)
	for k, v := range current.Frontmatter {
		fm[k] = v
	}
	fm["proposed_tier"] = string(target)
	fm["proposed_reason"] = reason
	fm["proposed_at"] = timeString(time.Now())

	content, err := serialize(fm, rewriteBody(current))
	if err != nil {
		return err
	}
	if err := writeFile(abs, rel, content); err != nil {
		return err
	}

	if err := commitPath(root, rel, commitMessage("Propose tier", rel, reason, "tier_propose")); err != nil {
		return fmt.Errorf("commit %s: %w", rel, err)
	}
	return nil
}
