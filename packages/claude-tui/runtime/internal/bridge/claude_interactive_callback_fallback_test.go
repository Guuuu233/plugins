package bridge

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInteractiveMCPCallbackAndTranscriptOrder(t *testing.T) {
	for _, order := range []string{"callback_before_transcript", "transcript_before_callback"} {
		t.Run(order, func(t *testing.T) {
			rd, wr := io.Pipe()
			defer rd.Close()
			defer wr.Close()
			run := &subscriptionRun{interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Read": true}, pending: map[string]chan mcpToolResult{}}
			ch := run.attach()
			done := make(chan struct{})
			go func() { run.readInteractiveOneTurn(rd); close(done) }()
			var callback func(string, string, json.RawMessage)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				run.mu.Lock()
				callback = run.onCall
				run.mu.Unlock()
				if callback != nil {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			if callback == nil {
				t.Fatal("native callback listener never initialized")
			}
			writeNative := func() {
				_, err := io.WriteString(wr, `{"type":"assistant","uuid":"native-row","message":{"id":"native-msg","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"tool_abc","name":"mcp__magpie__Read","input":{"file_path":"demo.txt"}}]}}`+"\n")
				if err != nil {
					t.Fatal(err)
				}
			}
			if order == "transcript_before_callback" {
				writeNative()
			} else {
				callback("tool_abc", "Read", json.RawMessage(`{"file_path":"demo.txt"}`))
			}
			var calls []string
			var starts, stops, errs int
			limit := time.NewTimer(4 * time.Second)
			defer limit.Stop()
		loop:
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						break loop
					}
					switch ev.Kind {
					case KStart:
						starts++
					case KToolStart:
						calls = append(calls, ev.ID)
					case KStop:
						if ev.Stop == "tool" {
							stops++
						}
					case KError:
						errs++
					}
				case <-limit.C:
					t.Fatal("MCP tool was not delivered; native transcript may be blocked")
				}
			}
			if strings.Join(calls, ",") != "tool_abc" || starts != 1 || stops != 1 || errs != 0 {
				t.Fatalf("wrong tool event sequence: start=%d calls=%v stop=%d errors=%d", starts, calls, stops, errs)
			}
			if order == "callback_before_transcript" {
				writeNative()
			} else {
				callback("tool_abc", "Read", json.RawMessage(`{"file_path":"demo.txt"}`))
			}
			// Native rows may arrive long after fallback; they must not clear the
			// external call's ownership or cause a second execution.
			time.Sleep(1250 * time.Millisecond)
			run.mu.Lock()
			shown := append([]shownCall(nil), run.shown...)
			seg := run.segment
			run.mu.Unlock()
			if len(shown) != 1 || shown[0].id != "tool_abc" || seg != nil {
				t.Fatalf("late duplicate altered tool handoff: shown=%+v attached=%t", shown, seg != nil)
			}
			_ = wr.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("reader was not reaped")
			}
		})
	}
}

// The callback must not merely show the client a tool: the returned
// result must reach the original HTTP MCP helper exactly once.
func TestInteractiveCallbackWithoutTranscriptRoundTrip(t *testing.T) {
	b := newSubscriptionBridge()
	defer bridges.Delete(b)
	run := &subscriptionRun{bridge: b, token: "pending-native-call", interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Read": true}, pending: map[string]chan mcpToolResult{}, timer: time.AfterFunc(time.Minute, func() {})}
	defer run.timer.Stop()
	ch := run.attach()
	b.mu.Lock()
	b.runs[run.token] = run
	b.mu.Unlock()
	rd, wr := io.Pipe()
	defer rd.Close()
	defer wr.Close()
	done := make(chan struct{})
	go func() { run.readInteractiveOneTurn(rd); close(done) }()
	// The real reader has to register the callback listener before the MCP
	// helper's request; do not fake a tool_use transcript row.
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		run.mu.Lock()
		ready := run.onCall != nil
		run.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	request := httptest.NewRequest("POST", "/_magpie/claude-mcp/native", strings.NewReader(`{"tool_call_id":"tool_native","name":"Read","arguments":{"file_path":"x"}}`))
	request.SetPathValue("token", run.token)
	output := httptest.NewRecorder()
	complete := make(chan struct{})
	go func() { b.mcpCall(output, request); close(complete) }()
	calls := 0
	limit := time.NewTimer(4 * time.Second)
	defer limit.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				goto toolShown
			}
			if ev.Kind == KToolStart {
				calls++
				if ev.ID != "tool_native" {
					t.Fatalf("wrong tool id: %s", ev.ID)
				}
			}
			if ev.Kind == KError {
				t.Fatalf("callback delivery error: %s", ev.Text)
			}
		case <-limit.C:
			t.Fatal("native MCP callback did not reach Desktop without JSONL")
		}
	}
toolShown:
	if calls != 1 {
		t.Fatalf("callback sent %d tool calls", calls)
	}
	_, err := run.continueWith([]Part{{Kind: ToolResult, CallID: "tool_native", Text: "read complete"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-complete:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP helper callback did not receive returned tool result")
	}
	if output.Code != 200 || !strings.Contains(output.Body.String(), "read complete") {
		t.Fatalf("unexpected MCP result HTTP %d", output.Code)
	}
	run.mu.Lock()
	pending := len(run.pending)
	run.mu.Unlock()
	if pending != 0 {
		t.Fatal("duplicate/unreleased native callback")
	}
	_ = wr.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader remained alive")
	}
	run.abort()
}

// A previous completed client tool leaves toolStopped true until the
// next native transcript row. That row may never appear while Claude
// is already blocked in MCP, so the next callback must clear the marker
// when a new continuation has attached to the same run.
func TestInteractiveSequentialCallbackWithoutNextTranscript(t *testing.T) {
	rd, wr := io.Pipe()
	defer rd.Close()
	defer wr.Close()
	run := &subscriptionRun{interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Read": true, "Edit": true}, pending: map[string]chan mcpToolResult{}}
	first := run.attach()
	done := make(chan struct{})
	go func() { run.readInteractiveOneTurn(rd); close(done) }()
	_, err := io.WriteString(wr, `{"type":"assistant","uuid":"native-first","message":{"id":"native-first-msg","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"first","name":"mcp__magpie__Read","input":{}}]}}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	drain := func(events <-chan Event) []Event {
		t.Helper()
		var got []Event
		until := time.NewTimer(4 * time.Second)
		defer until.Stop()
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return got
				}
				got = append(got, ev)
			case <-until.C:
				t.Fatal("tool handoff never completed")
			}
		}
	}
	a := drain(first)
	if len(a) < 2 {
		t.Fatalf("first tool was not sent: %+v", a)
	}
	run.mu.Lock()
	run.resuming = true
	run.mu.Unlock()
	second := run.attach()
	until := time.Now().Add(time.Second)
	var callback func(string, string, json.RawMessage)
	for time.Now().Before(until) {
		run.mu.Lock()
		callback = run.onCall
		run.mu.Unlock()
		if callback != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if callback == nil {
		t.Fatal("no callback registered")
	}
	// The second tool has no native JSONL row yet; in the failed real
	// session it never received one because the MCP result had not returned.
	callback("second", "Edit", json.RawMessage(`{"file_path":"x","old_string":"a","new_string":"b"}`))
	b := drain(second)
	var n int
	for _, ev := range b {
		if ev.Kind == KToolStart && ev.ID == "second" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("second native callback was suppressed: %d, events=%+v", n, b)
	}
	_ = wr.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader leaked")
	}
}
