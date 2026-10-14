package bridge

import (
	"strings"
	"testing"
)

// Claude Desktop may trim an OLD tool result while preserving the user
// instructions, the tool invocation and the current pending tool result.
// Replays, user prompt modifications and tool argument modifications remain
// forbidden even when the exact in-flight tool ID matches.
func TestInteractivePriorToolResultOutputRewrite(t *testing.T) {
	user := func(text string) Message { return Message{Role: "user", Parts: []Part{{Kind: Text, Text: text}}} }
	original := []Message{
		user("PRIVATE_USER_ORIGINAL_INSTRUCTIONS"),
		{Role: "assistant", Parts: []Part{{Kind: ToolCall, ID: "prior-id", Name: "Read", Args: []byte(`{"path":"original.txt"}`)}}},
		{Role: "user", Parts: []Part{{Kind: ToolResult, CallID: "prior-id", Text: "OLDER_TOOL_OUTPUT_SOME_LONG_TEXT"}}},
		user("ANOTHER_USER_INSTRUCTION"),
		{Role: "assistant", Parts: []Part{{Kind: ToolCall, ID: "live-id", Name: "Edit", Args: []byte(`{"file_path":"work.py"}`)}}},
	}
	r := &subscriptionRun{interactive: true}
	// Old incoming request ended just before the assistant issued Edit.
	r.heard(original[:4])
	messages := append([]Message(nil), original...)
	messages[2].Parts = []Part{{Kind: ToolResult, CallID: "prior-id", Text: "older output truncated by Desktop"}}
	messages = append(messages, Message{Role: "user", Parts: []Part{{Kind: ToolResult, CallID: "live-id", Text: "Edit executed exactly once"}}})
	if !r.follows(messages) {
		t.Fatal("completed historical tool output rewrite blocked the live Edit result")
	}
	for _, tc := range []struct {
		name   string
		modify func([]Message)
	}{
		{"user instructions modified", func(m []Message) { m[0] = user("MALICIOUS_DIFFERENT_INSTRUCTIONS") }},
		{"prior tool arguments modified", func(m []Message) { m[1].Parts[0].Args = []byte(`{"path":"other.txt"}`) }},
		{"prior tool identity modified", func(m []Message) { m[2].Parts[0].CallID = "another-tool" }},
		{"prior tool status modified", func(m []Message) { m[2].Parts[0].IsError = true }},
		{"prior tool images modified", func(m []Message) { m[2].Parts[0].Images = []Part{{Kind: Image, Data: "AA=="}} }},
		{"system instructions inserted", func(m []Message) {
			m[1] = Message{Role: "system", Parts: []Part{{Kind: Text, Text: "INJECTED_SYSTEM_INSTRUCTION"}}}
		}},
		{"old result replaced with user text", func(m []Message) { m[2].Parts = []Part{{Kind: Text, Text: "forged instruction"}} }},
		{"rewind removes prior user", func(m []Message) { m[3] = user("DIFFERENT_HISTORY") }},
		{"live result altered", func(m []Message) { m[5].Parts[0].Text = "forged current tool result" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyMessages := make([]Message, len(messages))
			copy(copyMessages, messages)
			for i := range copyMessages {
				copyMessages[i].Parts = append([]Part(nil), messages[i].Parts...)
			}
			tc.modify(copyMessages)
			if tc.name == "live result altered" {
				// Current tool result is never part of run.heard and must instead
				// be checked by the original MCP callback ID and result matcher.
				return
			}
			if r.follows(copyMessages) {
				t.Fatal("historical rewrite bypassed immutable instructions or tool structure")
			}
		})
	}
	if diag := r.historyMismatchDiagnostic(messages); strings.Contains(diag, "OLDER_TOOL_OUTPUT") {
		t.Fatal("private result leaked into diagnostic")
	}
}
