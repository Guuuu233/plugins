package bridge

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// The real 2026-10-14 outage: two tool_use rows share one assistant message
// ID but the second row arrives >300ms after the first. The first response
// segment is already closed. Processing the late row must not erase its
// shown tool IDs or kill the persistent transcript reader.
func TestInteractiveLateSplitToolCannotEraseFirstHandoff(t *testing.T) {
	was := unstreamedQuiet
	unstreamedQuiet = 45 * time.Millisecond
	t.Cleanup(func() { unstreamedQuiet = was })
	run := &subscriptionRun{interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Edit": true, "Bash": true}, pending: map[string]chan mcpToolResult{}}
	rd, wr := io.Pipe()
	defer rd.Close()
	defer wr.Close()
	first := run.attach()
	done := make(chan struct{})
	go func() { run.readInteractiveOneTurn(rd); close(done) }()
	msgID := "msg_same_parallel"
	send := func(id, tool string) {
		t.Helper()
		line := `{"type":"assistant","uuid":"` + id + `","message":{"id":"` + msgID + `","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"` + id + `","name":"mcp__magpie__` + tool + `","input":{}}]}}` + "\n"
		if _, err := io.WriteString(wr, line); err != nil {
			t.Fatal(err)
		}
	}
	send("edit_id", "Edit")
	deadline := time.After(2 * time.Second)
	var firstCalls []string
waitFirst:
	for {
		select {
		case ev, ok := <-first:
			if !ok {
				break waitFirst
			}
			if ev.Kind == KToolStart {
				firstCalls = append(firstCalls, ev.ID)
			}
		case <-deadline:
			t.Fatal("first tool turn did not settle")
		}
	}
	if strings.Join(firstCalls, ",") != "edit_id" {
		t.Fatalf("unexpected first calls: %v", firstCalls)
	}
	send("bash_id", "Bash")
	time.Sleep(110 * time.Millisecond)
	run.mu.Lock()
	shown := append([]shownCall(nil), run.shown...)
	run.mu.Unlock()
	if len(shown) != 1 || shown[0].id != "edit_id" {
		t.Fatalf("late parallel row destroyed original handoff: %+v", shown)
	}
	// This is the exact check that returned HTTP 409 in production:
	// the client returned the already-visible Edit result, but the
	// transcript reader had cleared run.shown on the delayed Bash row.
	if err := validateInteractiveNativeResultSet(run, []Part{{Kind: ToolResult, CallID: "edit_id", Text: "edited"}}); err != nil {
		t.Fatalf("legitimate Edit result was rejected as incomplete (HTTP 409): %v", err)
	}
	select {
	case <-done:
		t.Fatal("native transcript reader died after a late parallel tool row")
	default:
	}
	// Once the tool_result resumes the PTY, a subsequent native MCP callback
	// for the second tool must be deliverable without any duplicate Edit.
	run.mu.Lock()
	run.resuming = true
	run.pending["bash_id"] = make(chan mcpToolResult, 1)
	cb := run.onCall
	run.mu.Unlock()
	if cb == nil {
		t.Fatal("native callback listener disappeared")
	}
	second := run.attach()
	cb("bash_id", "Bash", json.RawMessage(`{}`))
	var next []string
	wait := time.After(2500 * time.Millisecond)
waitSecond:
	for {
		select {
		case ev, ok := <-second:
			if !ok {
				break waitSecond
			}
			if ev.Kind == KToolStart {
				next = append(next, ev.ID)
			}
			if ev.Kind == KError {
				t.Fatalf("tool error after resumed callback: %s", ev.Text)
			}
		case <-wait:
			t.Fatal("resumed sibling tool never delivered")
		}
	}
	if strings.Join(next, ",") != "bash_id" {
		t.Fatalf("wrong resumed tool set: %v", next)
	}
	_ = wr.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader leaked")
	}
}
