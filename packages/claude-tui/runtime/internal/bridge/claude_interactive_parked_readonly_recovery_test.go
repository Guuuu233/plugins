package bridge

import (
	"strings"
	"testing"
	"time"
)

func TestInteractiveExplicitRecoveryReapsAbandonedParkedOwner(t *testing.T) {
	dir := olDir(t)
	olWrapper(t, dir, "tool-free-recovery.sh", olAnswerScript(""))
	s := New()
	defer bridges.Delete(s.subscription)
	b := s.subscription
	rt := &runtimeTUI{bridge: b}
	owner, outer := "owner", "abandoned-readonly"
	olSave(t, b, owner, outer, "inner-abandoned", true)
	old := &subscriptionRun{bridge: b, token: "old-stalled", owner: owner, model: olModel, outerSession: outer, interactive: true, abandonedByClient: true, parkedAt: time.Now(), pending: map[string]chan mcpToolResult{}, shown: []shownCall{{id: "write-1", name: "write_file"}}, timer: time.AfterFunc(time.Hour, func() {})}
	defer old.timer.Stop()
	b.mu.Lock()
	b.runs[old.token] = old
	b.liveOuter[interactiveSessionStateKey(owner, outer)] = old
	b.mu.Unlock()
	// A stalled tool_result+new explicit user recovery has no further pending
	// tool callbacks; it must free the old PTY slot and launch tool-free.
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"tools":[{"name":"write_file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"先读一下配置"},{"role":"assistant","content":"配置已读"},{"role":"user","content":"write it"},{"role":"assistant","content":[{"type":"tool_use","id":"write-1","name":"write_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"write-1","content":"written"},{"type":"text","text":"/magpie-recover"}]}]}`)
	code, msg, out := olServe(t, rt, b, owner, outer, body)
	if code != 200 || !strings.Contains(out, "OL_ANSWER") {
		t.Fatalf("explicit read-only recovery failed: %d %s %s", code, msg, out)
	}
	old.mu.Lock()
	closed := old.closed
	old.mu.Unlock()
	if !closed {
		t.Fatal("abandoned owner not reaped before recovery")
	}
}

func TestInteractiveExplicitRecoveryRefusesOutstandingTool(t *testing.T) {
	dir := olDir(t)
	_ = dir
	s := New()
	defer bridges.Delete(s.subscription)
	b := s.subscription
	rt := &runtimeTUI{bridge: b}
	owner, outer := "owner", "tool-active"
	olSave(t, b, owner, outer, "inner-active", true)
	old := &subscriptionRun{bridge: b, token: "still-working", owner: owner, model: olModel, outerSession: outer, interactive: true, parkedAt: time.Now(), pending: map[string]chan mcpToolResult{"write-1": make(chan mcpToolResult, 1)}, shown: []shownCall{{id: "write-1", name: "write_file"}}, timer: time.AfterFunc(time.Hour, func() {})}
	defer old.timer.Stop()
	b.mu.Lock()
	b.runs[old.token] = old
	b.liveOuter[interactiveSessionStateKey(owner, outer)] = old
	b.mu.Unlock()
	body := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"先读一下配置"},{"role":"assistant","content":"配置已读"},{"role":"user","content":"write it"},{"role":"assistant","content":[{"type":"tool_use","id":"write-1","name":"write_file","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"write-1","content":"still working"},{"type":"text","text":"/magpie-recover"}]}]}`)
	code, _, _ := olServe(t, rt, b, owner, outer, body)
	if code != 409 {
		t.Fatalf("unsettled callback was interrupted for recovery: %d", code)
	}
	old.mu.Lock()
	closed := old.closed
	old.mu.Unlock()
	if closed {
		t.Fatal("tool-active run aborted")
	}
	old.abort()
}
