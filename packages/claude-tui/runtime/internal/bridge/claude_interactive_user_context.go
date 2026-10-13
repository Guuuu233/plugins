package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A genuine adjacent user message must not become MCP tool data. Preserve
// result ordering and remapped IDs from match, while taking text provenance
// from the original wire messages (also retained for history ownership).
func interactiveToolHandoff(req *Request, matched []Part, heardEnd int) ([]Part, string, error) {
	start := len(req.Messages)
	for start > 0 && req.Messages[start-1].Role != "assistant" {
		start--
	}
	start = max(start, heardEnd)
	clean := slices.Clone(matched)
	for i := range clean {
		clean[i].Text = ""
	}
	var userText []string
	index := -1
	for _, m := range req.Messages[start:] {
		for _, p := range m.Parts {
			switch p.Kind {
			case ToolResult:
				index++
				if index >= len(clean) {
					return nil, "", errors.New("interactive tool result count changed during handoff")
				}
				clean[index].Text = p.Text
			case Text:
				if m.Role == "user" && p.Text != "" {
					userText = append(userText, p.Text)
				} else if index >= 0 && p.Text != "" {
					if clean[index].Text != "" {
						clean[index].Text += "\n\n"
					}
					clean[index].Text += p.Text
				}
			}
		}
	}
	if index+1 != len(clean) {
		return nil, "", errors.New("interactive tool result count changed during handoff")
	}
	return clean, strings.Join(userText, "\n\n"), nil
}

type interactiveUserContext struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	ToolCallIDs []string `json:"tool_call_ids"`
}

func (r *subscriptionRun) prepareInteractiveUserContext(text string, results []Part) (string, error) {
	if text == "" {
		return "", nil
	}
	if len(text) > 1<<20 || len(results) == 0 {
		return "", errors.New("interactive user update is too large or has no matching tool result")
	}
	path := filepath.Join(r.tmp, "user-context.json")
	ready, err := os.ReadFile(path + ".ready")
	if err != nil || string(ready) != "1\n" {
		return "", errors.New("interactive wrapper does not support separate mid-turn user messages")
	}
	payload := interactiveUserContext{ID: randomToken(), Text: text}
	for _, p := range results {
		payload.ToolCallIDs = append(payload.ToolCallIDs, p.CallID)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if len(data) > 1_100_000 {
		return "", errors.New("interactive user update exceeds the wrapper payload limit")
	}
	// The run's claimResume lock serializes updates. An earlier acknowledgement
	// can never satisfy this nonce, and the wrapper sees only a complete file.
	if err := os.Remove(path + ".ack"); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	f, err := os.CreateTemp(r.tmp, "user-context-write-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.interactiveUserNonce, r.interactiveUserAcked = payload.ID, false
	r.mu.Unlock()
	if err = os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return payload.ID, nil
}

func (r *subscriptionRun) recordInteractiveUserContextAck() string {
	r.mu.Lock()
	nonce, acked := r.interactiveUserNonce, r.interactiveUserAcked
	r.mu.Unlock()
	if nonce == "" || acked {
		if acked {
			return nonce
		}
		return ""
	}
	var ack struct {
		ID string `json:"id"`
	}
	data, err := os.ReadFile(filepath.Join(r.tmp, "user-context.json.ack"))
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil && json.Unmarshal(data, &ack) == nil && ack.ID == nonce && nonce == r.interactiveUserNonce {
		r.interactiveUserAcked = true
	}
	if r.interactiveUserAcked {
		return r.interactiveUserNonce
	}
	return ""
}

// PostToolUse runs only after MCP results are released. Wait afterwards,
// never before sending callbacks, and fail closed if the wrapper lost the
// message rather than silently returning the answer to the earlier request.
func (r *subscriptionRun) awaitInteractiveUserContext(ctx context.Context, nonce string) error {
	if nonce == "" {
		return nil
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if r.recordInteractiveUserContextAck() == nonce {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.interactiveDone:
			if r.recordInteractiveUserContextAck() == nonce {
				return nil
			}
			return errors.New("interactive wrapper ended before acknowledging the user update")
		case <-deadline.C:
			return fmt.Errorf("interactive wrapper did not acknowledge the user update within 15 seconds")
		case <-tick.C:
		}
	}
}
