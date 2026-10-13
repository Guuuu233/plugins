package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A fast native MCP request can arrive after the run is published but
// before the transcript reader has registered its onCall listener. The
// request must still become visible to the client without a JSONL row.
func TestInteractiveCallbackBeforeReaderStarts(t *testing.T) {
	b := newSubscriptionBridge()
	defer bridges.Delete(b)
	run := &subscriptionRun{bridge: b, token: "fast-native", interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Read": true}, pending: map[string]chan mcpToolResult{}, timer: time.AfterFunc(time.Minute, func() {})}
	defer run.timer.Stop()
	events := run.attach()
	b.mu.Lock()
	b.runs[run.token] = run
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/_magpie/claude-mcp/fast-native", strings.NewReader(`{"tool_call_id":"fast-tool","name":"Read","arguments":{}}`)).WithContext(ctx)
	req.SetPathValue("token", run.token)
	complete := make(chan struct{})
	go func() { b.mcpCall(httptest.NewRecorder(), req); close(complete) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		run.mu.Lock()
		ok := run.pending["fast-tool"] != nil
		run.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	rd, wr := io.Pipe()
	defer rd.Close()
	defer wr.Close()
	go run.readInteractiveOneTurn(rd)
	seen := false
	timeout := time.After(2300 * time.Millisecond)
loop:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			if ev.Kind == KToolStart && ev.ID == "fast-tool" {
				seen = true
			}
		case <-timeout:
			break loop
		}
	}
	if !seen {
		t.Fatal("MCP request arrived before transcript reader but never reached client")
	}
	cancel()
	run.abort()
	select {
	case <-complete:
	case <-time.After(time.Second):
		t.Fatal("callback never stopped")
	}
}

// A duplicate stdio MCP notification for one native tool_use ID must not
// overwrite its waiter and strand the first callback until Desktop times out.
func TestInteractiveDuplicateMCPCallbackRejected(t *testing.T) {
	b := newSubscriptionBridge()
	defer bridges.Delete(b)
	run := &subscriptionRun{bridge: b, token: "duplicate-callback", interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Read": true}, pending: map[string]chan mcpToolResult{}, timer: time.AfterFunc(time.Minute, func() {})}
	defer run.timer.Stop()
	b.mu.Lock()
	b.runs[run.token] = run
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	makeReq := func() *httptest.ResponseRecorder { return httptest.NewRecorder() }
	first := makeReq()
	second := makeReq()
	request := func() *http.Request {
		req := httptest.NewRequest("POST", "/_magpie/claude-mcp/duplicate-callback", strings.NewReader(`{"tool_call_id":"same-id","name":"Read","arguments":{}}`)).WithContext(ctx)
		req.SetPathValue("token", run.token)
		return req
	}
	doneFirst := make(chan struct{})
	go func() { b.mcpCall(first, request()); close(doneFirst) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		run.mu.Lock()
		present := run.pending["same-id"] != nil
		run.mu.Unlock()
		if present {
			break
		}
		time.Sleep(time.Millisecond)
	}
	doneSecond := make(chan struct{})
	go func() { b.mcpCall(second, request()); close(doneSecond) }()
	select {
	case <-doneSecond:
		if second.Code != 409 {
			t.Fatalf("duplicate callback got %d, expected 409", second.Code)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("duplicate callback left a second waiter blocked")
	}
	cancel()
	run.abort()
	select {
	case <-doneFirst:
	case <-time.After(time.Second):
		t.Fatal("original callback remained stuck")
	}
}

// A tool that arrives after the prior tool segment has settled cannot be
// shown on that already-closed HTTP reply. It must return a bounded error
// to Claude, rather than leaving its stdio MCP call blocked for 13 minutes.
func TestInteractiveLateParallelCallbackFailsFastWithoutExecution(t *testing.T) {
	b := newSubscriptionBridge()
	defer bridges.Delete(b)
	run := &subscriptionRun{bridge: b, token: "late-parallel", interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Read": true}, pending: map[string]chan mcpToolResult{}, timer: time.AfterFunc(time.Minute, func() {})}
	defer run.timer.Stop()
	first := run.attach()
	b.mu.Lock()
	b.runs[run.token] = run
	b.mu.Unlock()
	rd, wr := io.Pipe()
	defer rd.Close()
	defer wr.Close()
	done := make(chan struct{})
	go func() { run.readInteractiveOneTurn(rd); close(done) }()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		run.mu.Lock()
		ready := run.onCall != nil
		run.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// First call is visible from the native transcript and settles normally.
	_, _ = io.WriteString(wr, `{"type":"assistant","uuid":"tool-a","message":{"id":"msg-a","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"first","name":"mcp__magpie__Read","input":{}}]}}`+"\n")
	for range first {
	}
	run.mu.Lock()
	hadSegment := run.segment != nil
	run.mu.Unlock()
	if hadSegment {
		t.Fatal("first segment not closed before late callback")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("POST", "/_magpie/claude-mcp/late-parallel", strings.NewReader(`{"tool_call_id":"second","name":"Read","arguments":{}}`)).WithContext(ctx)
	req.SetPathValue("token", run.token)
	response := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() { b.mcpCall(response, req); close(completed) }()
	select {
	case <-completed:
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"is_error":true`) {
			t.Fatalf("undeliverable callback should return MCP tool error without running user tool: HTTP=%d", response.Code)
		}
	case <-time.After(2200 * time.Millisecond):
		t.Fatal("second parallel callback blocked after first tool handoff closed")
	}
	_ = wr.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader leaked")
	}
	run.abort()
}

// If Claude abandons a native MCP request while the reader is still
// initializing, the stale buffered callback must never execute a tool.
func TestInteractiveCancelledEarlyCallbackNeverExecutes(t *testing.T) {
	b := newSubscriptionBridge()
	defer bridges.Delete(b)
	run := &subscriptionRun{bridge: b, token: "cancel-early", interactive: true, model: "claude-opus-5-5", tools: map[string]bool{"Write": true}, pending: map[string]chan mcpToolResult{}, timer: time.AfterFunc(time.Minute, func() {})}
	defer run.timer.Stop()
	events := run.attach()
	b.mu.Lock()
	b.runs[run.token] = run
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/_magpie/claude-mcp/cancel-early", strings.NewReader(`{"tool_call_id":"cancel-tool","name":"Write","arguments":{}}`)).WithContext(ctx)
	req.SetPathValue("token", run.token)
	done := make(chan struct{})
	go func() { b.mcpCall(httptest.NewRecorder(), req); close(done) }()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		run.mu.Lock()
		present := run.pending["cancel-tool"] != nil
		run.mu.Unlock()
		if present {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled MCP callback did not exit")
	}
	rd, wr := io.Pipe()
	defer rd.Close()
	defer wr.Close()
	readerDone := make(chan struct{})
	go func() { run.readInteractiveOneTurn(rd); close(readerDone) }()
	select {
	case ev := <-events:
		if ev.Kind == KToolStart {
			t.Fatal("client was told to execute a cancelled tool")
		}
	case <-time.After(1600 * time.Millisecond):
	}
	run.mu.Lock()
	pending := len(run.pending)
	run.mu.Unlock()
	if pending != 0 {
		t.Fatal("cancelled native callback waiter leaked")
	}
	_ = wr.Close()
	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("reader leaked")
	}
	run.abort()
}
