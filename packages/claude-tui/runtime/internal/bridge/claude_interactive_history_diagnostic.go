package bridge

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
)

// These summaries locate a mismatch without logging prompts, tool arguments,
// results, IDs or even their hashes. They never participate in acceptance.
type historyMessageFingerprint struct {
	role  string
	shape string // kinds only, never prompt text, tool IDs, names or hashes
	words [32]byte
}

func historyPartShape(m Message) string {
	var kinds []string
	for _, p := range m.Parts {
		switch p.Kind {
		case Text:
			kinds = append(kinds, "text")
		case ToolCall:
			kinds = append(kinds, "call")
		case ToolResult:
			kinds = append(kinds, "result")
		case Thinking:
			kinds = append(kinds, "thinking")
		case Image:
			kinds = append(kinds, "image")
		case File:
			kinds = append(kinds, "file")
		default:
			kinds = append(kinds, "other")
		}
	}
	return strings.Join(kinds, ",")
}

// Bound the suffix summary and emit only structural counts. This explains why
// an explicit recovery turn was not recognized, without recording its text.
func interactiveRecoveryDiagnostic(req *Request) string {
	var rows []string
	for i := max(0, len(req.Messages)-6); i < len(req.Messages); i++ {
		m := req.Messages[i]
		role := m.Role
		switch role {
		case "user", "assistant", "system":
		default:
			role = "other"
		}
		text, calls, results, reminders := 0, 0, 0, 0
		for _, p := range m.Parts {
			switch p.Kind {
			case Text:
				text++
				if strings.Contains(p.Text, "<system-reminder>") {
					reminders++
				}
			case ToolCall:
				calls++
			case ToolResult:
				results++
			}
		}
		rows = append(rows, fmt.Sprintf("row=%d role=%s text=%d calls=%d results=%d reminders=%d recovery_user=%t synthetic_error=%t", i, role, text, calls, results, reminders, interactiveRecoveryUserMessage(m), interactiveRecoverySyntheticAssistant(m)))
	}
	return strings.Join(rows, "; ")
}

func interactiveHistoryFingerprints(msgs []Message) []historyMessageFingerprint {
	msgs = withoutDynamicTokenBudgetNotices(msgs)
	words := messageWords(msgs)
	out := make([]historyMessageFingerprint, 0, len(msgs))
	for i, word := range words {
		if word == "" {
			continue
		}
		role := msgs[i].Role
		switch role {
		case "user", "assistant", "system":
		default:
			role = "other"
		}
		out = append(out, historyMessageFingerprint{role: role, shape: historyPartShape(msgs[i]), words: sha256.Sum256([]byte(word))})
	}
	return out
}

func (r *subscriptionRun) historyMismatchDiagnostic(msgs []Message) string {
	r.mu.Lock()
	old := slices.Clone(r.heardHistory)
	r.mu.Unlock()
	if old == nil {
		return "history_baseline_unavailable"
	}
	current := interactiveHistoryFingerprints(msgs)
	for i, before := range old {
		if i < len(current) && before == current[i] {
			continue
		}
		afterRole, afterShape := "missing", "missing"
		if i < len(current) {
			afterRole = current[i].role
			afterShape = current[i].shape
		}
		return fmt.Sprintf("normalized_message=%d old_role=%s new_role=%s old_parts=%s new_parts=%s old_messages=%d current_messages=%d", i, before.role, afterRole, before.shape, afterShape, len(old), len(current))
	}
	return "normalized_history_unchanged"
}
