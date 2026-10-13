package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const interactiveSessionStateVersion = 1

type interactiveRestoreStatus string

const (
	interactiveRestoreExact              interactiveRestoreStatus = "exact"
	interactiveRestoreReplyAnchor        interactiveRestoreStatus = "reply_anchor"
	interactiveRestoreCompanion          interactiveRestoreStatus = "turn_companion"
	interactiveRestoreStateMissing       interactiveRestoreStatus = "state_missing"
	interactiveRestoreDirty              interactiveRestoreStatus = "dirty"
	interactiveRestoreContextMismatch    interactiveRestoreStatus = "context_mismatch"
	interactiveRestoreCheckpointNotFound interactiveRestoreStatus = "checkpoint_not_found"
	interactiveRestoreSuffixRejected     interactiveRestoreStatus = "suffix_rejected"
)

type conversationSuffixStatus uint8

const (
	conversationSuffixOK conversationSuffixStatus = iota
	conversationSuffixCheckpointNotFound
	conversationSuffixRejected
)

type interactiveSessionEntry struct {
	SessionID  string    `json:"session_id"`
	Model      string    `json:"model,omitempty"`
	ThreadKey  string    `json:"thread_key,omitempty"`
	ConvKey    string    `json:"conv_key"`
	ReplyKey   string    `json:"reply_key,omitempty"`
	ContextKey string    `json:"context_key"`
	Dirty      bool      `json:"dirty,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type interactiveSessionState struct {
	Version  int                                `json:"version"`
	Sessions map[string]interactiveSessionEntry `json:"sessions"`
}

func interactiveSessionStatePath() string {
	return filepath.Join(mustStateDir(), "claude-interactive-sessions.json")
}

func interactiveSessionStateKey(owner, outerSession string) string {
	sum := sha256.Sum256([]byte(owner + "\x00" + outerSession))
	return hex.EncodeToString(sum[:])
}

func interactiveSessionWorkDir(owner, outerSession string) (string, error) {
	// Keep the cwd stable for this outer conversation, but outside the home:
	// Claude Code includes git status for a repository containing its cwd in
	// the system prompt, so a cache directory under a dotfiles-managed home can
	// both leak unrelated paths and invalidate the prompt-cache prefix. This is
	// the same concern addressed by #626 for headless subscription runs; the
	// interactive backend needs a per-conversation directory because --resume
	// resolves sessions by project/cwd.
	dir := filepath.Join(os.TempDir(), "magpie-claude-interactive-"+interactiveSessionStateKey(owner, outerSession))
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a folder", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s can be written by others (%v)", dir, fi.Mode().Perm())
	}
	// On a shared /tmp, a different user may have pre-created a 0700 path.
	// Creating a file inside proves this process can actually own/use the cwd
	// without platform-specific uid code; remove the probe immediately.
	probe, err := os.CreateTemp(dir, ".magpie-owner-check-*")
	if err != nil {
		return "", fmt.Errorf("%s is not writable by this user: %w", dir, err)
	}
	probeName := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probeName)
		return "", err
	}
	_ = os.Remove(probeName)
	return dir, nil
}

func readInteractiveSessionState() interactiveSessionState {
	out := interactiveSessionState{Version: interactiveSessionStateVersion, Sessions: map[string]interactiveSessionEntry{}}
	b, err := os.ReadFile(interactiveSessionStatePath())
	if err != nil {
		return out
	}
	if json.Unmarshal(b, &out) != nil || out.Version != interactiveSessionStateVersion || out.Sessions == nil {
		return interactiveSessionState{Version: interactiveSessionStateVersion, Sessions: map[string]interactiveSessionEntry{}}
	}
	return out
}

func writeInteractiveSessionState(state interactiveSessionState) error {
	dir := filepath.Dir(interactiveSessionStatePath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A bounded state file is enough for continuity while avoiding an
	// unbounded collection of conversations that can no longer share a live
	// Anthropic prompt cache anyway.
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for k, v := range state.Sessions {
		if !v.UpdatedAt.IsZero() && v.UpdatedAt.Before(cutoff) {
			delete(state.Sessions, k)
		}
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claude-interactive-sessions-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, interactiveSessionStatePath()); err != nil {
		// Windows cannot replace an existing file with Rename. The experimental
		// backend is primarily Unix/PTY based, but keep the state helper portable.
		_ = os.Remove(interactiveSessionStatePath())
		if err2 := os.Rename(name, interactiveSessionStatePath()); err2 != nil {
			return err
		}
	}
	return nil
}

func (b *subscriptionBridge) interactiveSession(owner, outerSession string) (interactiveSessionEntry, bool) {
	if outerSession == "" {
		return interactiveSessionEntry{}, false
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	state := readInteractiveSessionState()
	e, ok := state.Sessions[interactiveSessionStateKey(owner, outerSession)]
	return e, ok
}

func (b *subscriptionBridge) saveInteractiveSession(owner, outerSession string, e interactiveSessionEntry) error {
	if outerSession == "" || e.SessionID == "" || e.ConvKey == "" || e.ContextKey == "" {
		return errors.New("incomplete interactive checkpoint entry")
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	state := readInteractiveSessionState()
	e.UpdatedAt = time.Now().UTC()
	state.Sessions[interactiveSessionStateKey(owner, outerSession)] = e
	if err := writeInteractiveSessionState(state); err != nil {
		log.Printf("Claude interactive session state: %v", err)
		return err
	}
	return nil
}

func (b *subscriptionBridge) markInteractiveSessionDirty(owner, outerSession string) {
	if outerSession == "" {
		return
	}
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	state := readInteractiveSessionState()
	key := interactiveSessionStateKey(owner, outerSession)
	e, ok := state.Sessions[key]
	if !ok || e.Dirty {
		return
	}
	e.Dirty = true
	e.UpdatedAt = time.Now().UTC()
	state.Sessions[key] = e
	if err := writeInteractiveSessionState(state); err != nil {
		log.Printf("Claude interactive session state: %v", err)
	}
}

// claimInteractiveOuterTurn gives one in-flight interactive turn per
// (owner, outerSession). Desktop retries can deliver two requests for the
// same outer conversation concurrently; only the first may start a run,
// whichever kind of turn it is (a fresh first turn, a persisted resume, or
// an opt-in read-only recovery). The claim is released by the caller when
// the HTTP reply finishes — never held across turns and never persisted —
// so a restart drops it, and a replay then costs at most one extra
// tool-free turn, which is the documented bound.
func (b *subscriptionBridge) claimInteractiveOuterTurn(owner, outerSession string) bool {
	if outerSession == "" {
		return false
	}
	key := interactiveSessionStateKey(owner, outerSession)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.outerTurns[key] {
		return false
	}
	b.outerTurns[key] = true
	return true
}

func (b *subscriptionBridge) releaseInteractiveOuterTurn(owner, outerSession string) {
	if outerSession == "" {
		return
	}
	key := interactiveSessionStateKey(owner, outerSession)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.outerTurns, key)
}

// errInteractiveOuterLive refuses a canonical start on an outer
// conversation that a live canonical run still owns; serveMessagesInner
// maps it to 409, like the per-request outerTurns claim.
var errInteractiveOuterLive = errors.New("Claude interactive conversation has a live run; refusing a second PTY")

// interactiveOuterLive reports whether a canonical interactive run owns
// (owner, outerSession): the run was published against it and its child
// has not been reaped and removed yet. Callers on the HTTP path hold the
// per-request outerTurns claim, so the check serializes with every
// canonical start: a run registers liveOuter while its starting request
// still holds that claim, which means any later start attempt sees it.
func (b *subscriptionBridge) interactiveOuterLive(owner, outerSession string) bool {
	if outerSession == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.liveOuter[interactiveSessionStateKey(owner, outerSession)] != nil
}

// interactiveOuterOwner returns the canonical run that currently owns
// (owner, outerSession), or nil when it is free.
func (b *subscriptionBridge) interactiveOuterOwner(owner, outerSession string) *subscriptionRun {
	if outerSession == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.liveOuter[interactiveSessionStateKey(owner, outerSession)]
}

// interactiveTerminalCleanupWait bounds how long a next-turn request
// waits for a proven terminal-delivered owner to finish reaping — a
// wrapper's tmux EXIT_HOLD teardown is ~2s; the bound is short so a run
// that is merely slow, stuck or actually still working can never be
// mistaken for a finished one by a caller that waits longer.
var interactiveTerminalCleanupWait = 6 * time.Second

// errInteractiveTerminalBusy refuses a canonical start while the outer's
// terminal-delivered owner did not finish reaping inside the wait bound:
// fail closed like errInteractiveOuterLive, never a second PTY.
var errInteractiveTerminalBusy = errors.New("Claude interactive conversation's last run is still cleaning up; refusing a second PTY")

// interactiveTerminalDelivered reports whether ownerRun is the proven
// terminal-delivered canonical run for (owner, outerSession): it is the
// run still registered in liveOuter, and ended() marked it
// terminalDelivered — which is only ever set after a complete non-tool
// reply was delivered AND the clean canonical checkpoint for this same
// (owner, outerSession) was written. The persisted entry is re-checked
// to be clean and to belong to this run's inner session, so a stale or
// dirty checkpoint, a helper run, a parked tool turn, a failed/cancelled
// turn and a read-only-signaled turn are all refused, not waited on.
func (b *subscriptionBridge) interactiveTerminalDelivered(ownerRun *subscriptionRun, saved interactiveSessionEntry, savedOK bool) bool {
	if ownerRun == nil || !savedOK || saved.Dirty {
		return false
	}
	ownerRun.mu.Lock()
	terminal, sessionID := ownerRun.terminalDelivered, ownerRun.sessionID
	ownerRun.mu.Unlock()
	// ownerRun was taken from liveOuter[(owner,outerSession)], which binds
	// it to this conversation; the checkpoint must be the clean one this
	// same run's inner session produced.
	return terminal && sessionID != "" && sessionID == saved.SessionID
}

// waitInteractiveTerminalOwner waits, outside b.mu, for a proven
// terminal-delivered canonical run to release its outer owner slot.
//
// ownerRun must already be classified terminal-delivered by the caller
// (ended() delivered a complete non-tool reply AND saved the clean
// canonical checkpoint — run.terminalDelivered). Anything else — a live
// executing run, a parked tool turn, a failed/cancelled turn, a
// read-only-signaled turn — is refused, not waited on.
//
// The wait never forces the slot free, never clears a dirty checkpoint
// and never starts a second PTY: on ctx cancel or the bounded timeout it
// reports busy so the caller fails closed while the old run keeps owning
// the outer until it is actually reaped and removeRun drops the slot.
// Because ownerRun is the liveOuter *value* published under b.mu, a new
// run can only take the slot after the old one's removeRun deleted it —
// so observing the slot empty or re-owned by a different run proves the
// old child was reaped; observing the same run still there proves it has
// not been reaped yet and the wait must keep failing closed.
func (b *subscriptionBridge) waitInteractiveTerminalOwner(ctx context.Context, owner, outerSession string, ownerRun *subscriptionRun) error {
	key := interactiveSessionStateKey(owner, outerSession)
	deadline := time.Now().Add(interactiveTerminalCleanupWait)
	for {
		b.mu.Lock()
		current := b.liveOuter[key]
		b.mu.Unlock()
		if current != ownerRun {
			return nil // slot freed (nil) or re-owned by a reaped-and-replaced run
		}
		if !time.Now().Before(deadline) {
			return errInteractiveTerminalBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// assistantReplyKey is a history-independent anchor for the last completed
// reply saved in the inner Claude session. Unlike ConvKey it survives an
// outer client compacting or rewriting messages that came before that reply.
func assistantReplyKey(msg Message) string {
	if msg.Role != "assistant" {
		return ""
	}
	h := sha256.New()
	hashMessages(h, []Message{msg}, nil)
	return hex.EncodeToString(h.Sum(nil))
}

func claudeDesktopTurnCompanionMessage(m Message) bool {
	if m.Role != "user" {
		return false
	}
	for _, p := range m.Parts {
		if p.Kind != Text {
			continue
		}
		text := strings.TrimSpace(p.Text)
		if strings.HasPrefix(text, "[Your previous response had no visible output.") ||
			strings.HasPrefix(text, "Your response above was cut off mid-stream.") {
			return true
		}
	}
	return false
}

func claudeDesktopTurnCompanionSuffix(req *Request) []Message {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		if claudeDesktopTurnCompanionMessage(m) {
			return []Message{m}
		}
		return nil
	}
	return nil
}

func claudeDesktopFailedTurnCompanionReply(m Message) bool {
	if m.Role != "assistant" {
		return false
	}
	var text strings.Builder
	for _, p := range m.Parts {
		if p.Kind == Text {
			text.WriteString(p.Text)
		}
	}
	s := strings.TrimSpace(text.String())
	return strings.HasPrefix(s, "API Error: 502 ") &&
		strings.Contains(s, "Claude interactive persisted resume failed: dirty; refusing full history replay")
}

// restoreInteractiveSession decides whether a persisted inner Claude session
// can safely accept only the caller's new suffix. It never authorizes replay
// of the entire outer history.
func (b *subscriptionBridge) restoreInteractiveSession(owner, outerSession string, req *Request) (interactiveSessionEntry, []Message, interactiveRestoreStatus) {
	saved, ok := b.interactiveSession(owner, outerSession)
	if !ok {
		return interactiveSessionEntry{}, nil, interactiveRestoreStateMissing
	}
	if suffix := claudeDesktopTurnCompanionSuffix(req); suffix != nil {
		return saved, suffix, interactiveRestoreCompanion
	}
	if saved.Dirty {
		return saved, nil, interactiveRestoreDirty
	}
	since, status := conversationSuffixAfterDetailed(owner, req.Messages, saved.ConvKey)
	switch status {
	case conversationSuffixOK:
		return saved, since, interactiveRestoreExact
	case conversationSuffixRejected:
		return saved, nil, interactiveRestoreSuffixRejected
	}
	if saved.ReplyKey == "" {
		return saved, nil, interactiveRestoreCheckpointNotFound
	}
	since, status = conversationSuffixAfterReplyDetailed(req.Messages, saved.ReplyKey)
	switch status {
	case conversationSuffixOK:
		return saved, since, interactiveRestoreReplyAnchor
	case conversationSuffixRejected:
		return saved, nil, interactiveRestoreSuffixRejected
	default:
		return saved, nil, interactiveRestoreCheckpointNotFound
	}
}

func conversationSuffixAfterDetailed(owner string, msgs []Message, convKey string) ([]Message, conversationSuffixStatus) {
	if convKey == "" {
		return nil, conversationSuffixCheckpointNotFound
	}
	h := sha256.New()
	_, _ = h.Write([]byte(owner + "\x00"))
	match := -1
	hashMessages(h, msgs, func(i int) {
		if msgs[i].Role == "assistant" && hex.EncodeToString(h.Sum(nil)) == convKey {
			match = i
		}
	})
	return safeConversationSuffixAfterDetailed(msgs, match)
}

func conversationSuffixAfterReplyDetailed(msgs []Message, replyKey string) ([]Message, conversationSuffixStatus) {
	if replyKey == "" {
		return nil, conversationSuffixCheckpointNotFound
	}
	match := -1
	for i, m := range msgs {
		if m.Role != "assistant" || assistantReplyKey(m) != replyKey {
			continue
		}
		if match >= 0 {
			return nil, conversationSuffixRejected
		}
		match = i
	}
	return safeConversationSuffixAfterDetailed(msgs, match)
}

func safeConversationSuffixAfterDetailed(msgs []Message, match int) ([]Message, conversationSuffixStatus) {
	if match < 0 || match >= len(msgs)-1 {
		if match < 0 {
			return nil, conversationSuffixCheckpointNotFound
		}
		return nil, conversationSuffixRejected
	}
	since := msgs[match+1:]
	// A failed hidden turn-companion plus Magpie's synthetic 502 reply was
	// never accepted by the inner Claude session. Skip only that exact pair.
	for len(since) >= 2 && claudeDesktopTurnCompanionMessage(since[0]) && claudeDesktopFailedTurnCompanionReply(since[1]) {
		since = since[2:]
	}
	if len(since) == 0 {
		return nil, conversationSuffixRejected
	}
	for _, m := range since {
		if m.Role == "assistant" {
			return nil, conversationSuffixRejected
		}
		for _, p := range m.Parts {
			if p.Kind == ToolResult {
				return nil, conversationSuffixRejected
			}
		}
	}
	return since, conversationSuffixOK
}
