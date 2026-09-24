package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// This file holds the ONLY code paths in the codebase that raise a note's
// tier: a human opening a note in the renderer (MarkOpened) and a human
// approving a proposal in Al-Mina (ApproveProposal). They exist for
// nullapi's renderer and are deliberately unreachable from nullmcp — the
// internal/mcp package never references anything in this file, which
// TestMCPCannotReachHumanTierActions enforces by scanning its source.
// The model never raises a tier (R1); a human does, here, and nowhere
// else. Like every other write, each function ends in exactly one git
// commit touching exactly one file, and callers must have run rel through
// SafeRequestPath first.

// ErrNoProposal is returned by the Al-Mina actions when the note carries
// no outstanding proposal to act on.
var ErrNoProposal = errors.New("note has no outstanding proposal")

// updateFrontmatter rewrites rel's frontmatter through mutate — handed a
// copy of the current map — keeping the body byte-for-byte, and commits
// it as a fresh commit (never amended) with the given action verb, tool
// tag and reason. It refuses asil notes: nothing edits those, human
// Al-Mina actions included, until someone changes the file by hand.
func updateFrontmatter(root, rel, action, reason, tool string, mutate func(cur *Note, fm map[string]any) error) error {
	cur, abs, err := readCurrentNote(root, rel)
	if err != nil {
		return err
	}
	if cur.Tier == TierAsil {
		return ErrAsilLocked
	}
	fm := make(map[string]any, len(cur.Frontmatter)+4)
	for k, v := range cur.Frontmatter {
		fm[k] = v
	}
	if err := mutate(cur, fm); err != nil {
		return err
	}
	content, err := serialize(fm, rewriteBody(cur))
	if err != nil {
		return err
	}
	if err := writeFile(abs, rel, content); err != nil {
		return err
	}
	if err := commitPath(root, rel, commitMessage(action, rel, reason, tool)); err != nil {
		return fmt.Errorf("commit %s: %w", rel, err)
	}
	return nil
}

func clearProposal(fm map[string]any) {
	delete(fm, "proposed_tier")
	delete(fm, "proposed_reason")
	delete(fm, "proposed_at")
}

// MarkOpened promotes a dakhil note to amil because a human just opened
// it — the read is exactly the evidence tier 2 claims ("the user is aware
// of it"). It reports whether it promoted; a note at any other tier is a
// no-op. Callers must count only human reads (the renderer's cookie
// session), never a model's, or a note a model keeps re-reading would
// promote itself.
func MarkOpened(root, rel string) (bool, error) {
	cur, _, err := readCurrentNote(root, rel)
	if err != nil {
		return false, err
	}
	if cur.Tier != TierDakhil {
		return false, nil
	}
	err = updateFrontmatter(root, rel, "Mark amil", "first opened by the user", "renderer", func(_ *Note, fm map[string]any) error {
		fm["tier"] = string(TierAmil)
		appendTierHistory(fm, TierDakhil, TierAmil, "first opened by the user", "renderer")
		return nil
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// ApproveProposal applies rel's outstanding proposal: the note's tier
// becomes the proposed tier, the proposal is cleared, and the change is
// recorded in tier_history. This is the one place R1 is bypassed, because
// the actor is the user. A note that reaches asil is write-locked
// (0444) immediately, not just at the next index pass.
func ApproveProposal(root, rel string) error {
	var target Tier
	err := updateFrontmatter(root, rel, "Approve tier", "approved in Al-Mina", "al_mina", func(cur *Note, fm map[string]any) error {
		p := ProposalFrom(cur.Frontmatter)
		if p == nil {
			return ErrNoProposal
		}
		target = p.Tier
		fm["tier"] = string(p.Tier)
		clearProposal(fm)
		appendTierHistory(fm, cur.Tier, p.Tier, "approved in Al-Mina: "+p.Reason, "al_mina")
		return nil
	})
	if err != nil {
		return err
	}
	if target == TierAsil {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Chmod(abs, 0o444); err != nil {
			return fmt.Errorf("write-lock %s: %w", rel, err)
		}
	}
	return nil
}

// DenyProposal records that the user rejected rel's outstanding
// proposal: denied_tier/denied_at/denied_reason are written and the
// proposal is cleared. Denial is data — it is what stops tier_propose
// resubmitting the same ask until the note has changed.
func DenyProposal(root, rel, reason string) error {
	if reason == "" {
		reason = "denied in Al-Mina"
	}
	return updateFrontmatter(root, rel, "Deny tier", reason, "al_mina", func(cur *Note, fm map[string]any) error {
		p := ProposalFrom(cur.Frontmatter)
		if p == nil {
			return ErrNoProposal
		}
		fm["denied_tier"] = string(p.Tier)
		fm["denied_reason"] = reason
		fm["denied_at"] = timeString(time.Now())
		clearProposal(fm)
		return nil
	})
}

// DeferProposal clears rel's outstanding proposal without acting on it:
// the tier is untouched and no denial is written, so the model may
// propose again.
func DeferProposal(root, rel string) error {
	return updateFrontmatter(root, rel, "Defer tier proposal", "deferred in Al-Mina", "al_mina", func(cur *Note, fm map[string]any) error {
		if ProposalFrom(cur.Frontmatter) == nil {
			return ErrNoProposal
		}
		clearProposal(fm)
		return nil
	})
}
