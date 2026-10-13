package bridge

// Small helpers extracted verbatim or minimally reduced from the deployed
// gateway's other files. Functions are kept byte-for-byte where feasible;
// only the provider/settings/dependency edges are cut.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// protoAnthropic is the only protocol the plugin runtime serves. Kept as
// a typed constant so signatures that took provider.Protocol still read
// the same way, and misuse fails the type check.
const protoAnthropic = "anthropic"

// marshalPlain is json.Marshal leaving <, > and & as they are.
func marshalPlain(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// objectRootSubset is provider.ObjectRoot for the extracted runtime's one
// use: an Anthropic tool's input schema must not carry anyOf/oneOf/allOf
// at its root. We keep production's behavior — merge allOf branches into
// an object root, refuse on anything else — via a trimmed rewrite that
// covers the same case production's callers hit.
func objectRootSubset(ps map[string]any) bool {
	_, any1 := ps["anyOf"]
	_, one := ps["oneOf"]
	if any1 || one {
		return false
	}
	list, _ := ps["allOf"].([]any)
	if ps["type"] == "object" && len(list) == 0 {
		return false
	}
	props, _ := ps["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	var required []any
	if r, ok := ps["required"].([]any); ok {
		required = r
	}
	for _, b := range list {
		bm, _ := b.(map[string]any)
		if bm == nil {
			return false
		}
		// merge a branch's properties and required into the root
		for k, v := range bm["properties"].(map[string]any) {
			props[k] = v
		}
		if req, ok := bm["required"].([]any); ok {
			required = append(required, req...)
		}
	}
	if len(props) > 0 || len(required) > 0 {
		ps["type"] = "object"
		ps["properties"] = props
		if len(required) > 0 {
			ps["required"] = required
		}
	}
	delete(ps, "allOf")
	delete(ps, "anyOf")
	delete(ps, "oneOf")
	return true
}

// firstWords tells an agent's subagents from it: they share its session
// but not what they were asked. Only the words are taken — not how the
// agent marked them for caching — as they stay the same for the
// conversation's whole life.
func firstWords(req *Request) string {
	h := sha256New()
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == Text {
				h.Write([]byte(p.Text))
			}
		}
		break
	}
	return hexEncode12(h)
}

// userText is what the user said to begin the turn: the last user
// message's text, without what agents add to it for the model
// (<system-reminder>…), its middle left out when long.
var reminders = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

func userText(req *Request) string {
	var parts []string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == Text {
				parts = append(parts, p.Text)
			}
		}
		break
	}
	text := strings.TrimSpace(reminders.ReplaceAllString(strings.Join(parts, "\n"), ""))
	const head, tail = 3000, 1000
	if r := []rune(text); len(r) > head+tail {
		text = string(r[:head]) + "\n…\n" + string(r[len(r)-tail:])
	}
	return text
}

// hasReply reports whether msgs hold a reply: without one, a request is
// a one-off ask however many user messages it comes in.
func hasReply(msgs []Message) bool {
	for _, m := range msgs {
		if m.Role == "assistant" {
			return true
		}
	}
	return false
}

// compacting reports whether req is an agent compacting its conversation.
var compactSystems = []string{
	// Kimi Code (prompts/compact.md)
	"You are now given a task to compact this conversation context",
}
var compactAsks = []string{
	"summarize", "compact",
}

func compacting(req *Request) bool {
	if req == nil {
		return false
	}
	for _, p := range compactSystems {
		if strings.Contains(req.System, p) {
			return true
		}
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind != Text {
				continue
			}
			for _, a := range compactAsks {
				if strings.Contains(p.Text, a) {
					return true
				}
			}
		}
		break
	}
	return false
}

// autoModeClassifier reports whether req is Claude Code's auto mode
// classifier asking about an action.
func autoModeClassifier(req *Request) bool {
	if len(req.Tools) > 0 || !strings.Contains(req.System, "<block>") && !strings.Contains(req.System, "<severity>") {
		return false
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind == Text && strings.HasPrefix(strings.TrimSpace(p.Text), "<transcript>") {
				return true
			}
		}
	}
	return false
}

// requestEffort is the reasoning a request asks its model for, in its own
// API's words: reasoning_effort, reasoning.effort, or — for Anthropic, when
// it asks to think — output_config's effort or thinking's budget as the
// level it is nearest. "" when it asks for none. Extracted Anthropic-only.
func requestEffort(proto string, body []byte) string {
	if proto != protoAnthropic {
		return ""
	}
	var v struct {
		Thinking *struct {
			Type   string `json:"type"`
			Budget int    `json:"budget_tokens"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if json.Unmarshal(body, &v) != nil || v.Thinking == nil || v.Thinking.Type != "enabled" && v.Thinking.Type != "adaptive" {
		return ""
	}
	if e := effortOf(v.OutputConfig.Effort); e != "" {
		return e
	}
	return effortOfBudget(v.Thinking.Budget)
}

// searchingKey marks a search magpie asks for itself, which must not
// search by way of another model again.
type searchingKey struct{}

func searching(ctx context.Context) bool {
	v, _ := ctx.Value(searchingKey{}).(bool)
	return v
}

// canSearch is whether the runtime can answer its web_search tool.
// Production resolves this from its provider table and search APIs; the
// plugin runtime's only source is Config.SearchHandler.
func canSearch() bool { return config.SearchHandler != nil }

// searchTool is magpie's web_search the runtime offers a model that asked
// for web search; name is chosen not to collide with a caller's own.
func searchTool(tools []Tool) Tool {
	name := "web_search"
	for slices.ContainsFunc(tools, func(t Tool) bool { return t.Name == name }) {
		name = "magpie_" + name
	}
	return Tool{Name: name,
		Description: "Search the web for current information: news, releases, documentation, prices, anything after your training or that you are unsure of. Returns what the pages found say, with their URLs. Cite the URLs you use.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to search for"}},"required":["query"]}`)}
}

// claudeTier is the tier a model id is a Claude model of (claude-opus-4-8,
// a provider's anthropic/claude-sonnet-5), "" when it is none.
var claudeFamily = regexp.MustCompile(`(^|[^a-z0-9])claude`)

func claudeTier(id string) string {
	m := strings.ToLower(strings.TrimSuffix(id, "[1m]"))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if !claudeFamily.MatchString(m) {
		return ""
	}
	for _, t := range []string{"haiku", "sonnet", "opus"} {
		if strings.Contains(m, t) {
			return t
		}
	}
	return ""
}

// imagePart reads a data: URL into an inline image, or keeps the URL.
func imagePart(u string) Part {
	if strings.HasPrefix(u, "data:") {
		if meta, data, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ","); ok {
			mt := strings.TrimSuffix(meta, ";base64")
			return Part{Kind: Image, MediaType: mt, Data: data}
		}
	}
	return Part{Kind: Image, URL: u}
}

// upstreamStop is why a reply says it ended, in the upstream's own words:
// an Anthropic stop_reason, a Chat finish_reason, a Responses reply's
// status. Extracted Anthropic-only; other providers are unreachable here.
func upstreamStop(b []byte) string {
	if !bytes.Contains(b, []byte(`stop_reason"`)) && !bytes.Contains(b, []byte(`finish_reason"`)) && !bytes.Contains(b, []byte(`"response.`)) {
		return ""
	}
	var v struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Delta      struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Response struct {
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	switch {
	case v.Delta.StopReason != "":
		return v.Delta.StopReason
	case v.StopReason != "":
		return v.StopReason
	case strings.HasPrefix(v.Type, "response.") && v.Response.Status != "" && v.Response.Status != "in_progress" && v.Response.Status != "queued":
		if d := v.Response.IncompleteDetails; d != nil && d.Reason != "" {
			return v.Response.Status + ": " + d.Reason
		}
		return v.Response.Status
	}
	for _, c := range v.Choices {
		if c.FinishReason != "" {
			return c.FinishReason
		}
	}
	return ""
}

// claudeSignInRequired says message is Claude Code's, or magpie's, word
// that a Claude sign-in is gone and has to be made again: not a failure of
// the moment, as a network error or another Claude Code holding the
// refresh is ("Failed to refresh OAuth token: another Claude Code process
// is refreshing it"). Kept verbatim; the plugin's runtime has no saved
// account store to mark lapsed, so NoteSignInFailed is a no-op by default.
func claudeSignInRequired(message string) bool {
	for _, text := range []string{
		"OAuth session expired and could not be refreshed", // Claude Code: its refresh refused
		"OAuth token revoked",                              // Claude Code: Anthropic revoked it
		"OAuth access token has been revoked",              // Anthropic's own 401
		"Claude Code could not authenticate this account; sign in again in magpie",
	} {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

// render is the non-streaming reply, Anthropic only.
func render(proto string, res Result, r *Request) []byte {
	if proto != protoAnthropic {
		return nil
	}
	return renderAnthropic(res, r.Model)
}

// usageSniffer picks the usage block out of a provider reply that is being
// copied to the client untouched. Streams are read line by line as they
// pass; a plain JSON body is kept and parsed at the end. Extracted subset:
// Anthropic SSE only, which is the only upstream this runtime serves.
type usageSniffer struct {
	sse    bool
	buf    []byte
	data   []byte
	over   bool
	u      Usage
	served string
	ended  bool
	failed string
}

func newSniffer(proto string, contentType string) *usageSniffer {
	return &usageSniffer{sse: strings.HasPrefix(contentType, "text/event-stream")}
}

func (s *usageSniffer) write(b []byte) {
	if !s.sse {
		if len(s.buf)+len(b) > 8<<20 {
			s.over = true
			return
		}
		s.buf = append(s.buf, b...)
		return
	}
	s.buf = append(s.buf, b...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		s.line(bytes.TrimSuffix(s.buf[:i], []byte{'\r'}))
		s.buf = s.buf[i+1:]
	}
	if len(s.buf) > 1<<20 {
		s.buf = nil
		s.data = nil
	}
}

func (s *usageSniffer) line(line []byte) {
	if len(line) == 0 {
		s.flushEvent()
		return
	}
	if rest, ok := bytes.CutPrefix(line, []byte("event:")); ok && lastEvent(string(bytes.TrimSpace(rest))) {
		s.ended = true
		if string(bytes.TrimSpace(rest)) == "error" {
			s.failed = "upstream stream failed"
		}
	}
	if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		rest = bytes.TrimPrefix(rest, []byte{' '})
		if len(s.data)+len(rest)+1 > 1<<20 {
			s.data = nil
			return
		}
		if len(s.data) > 0 {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, rest...)
		if json.Valid(bytes.TrimSpace(s.data)) {
			s.flushEvent()
		}
	}
}

func (s *usageSniffer) flushEvent() {
	if len(s.data) > 0 {
		s.parse(bytes.TrimSpace(s.data))
		s.data = nil
	}
}

func (s *usageSniffer) parse(b []byte) {
	if s.sse && string(b) == "[DONE]" {
		s.ended = true
		return
	}
	var t struct {
		Type  string `json:"type"`
		Error any    `json:"error"`
	}
	if s.sse && json.Unmarshal(b, &t) == nil {
		switch {
		case lastEvent(t.Type):
			s.ended = true
		case t.Type == "":
			s.ended = s.ended || t.Error != nil
		}
		if t.Type == "error" || t.Type == "" && t.Error != nil {
			s.failed = apiError(b, "upstream stream failed")
			s.u.ErrType = errorType(b)
		}
	}
	if st := upstreamStop(b); st != "" {
		s.u.Stop = st
	}
	var v struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &v) == nil {
		if v.Model != "" {
			s.served = v.Model
		}
		s.u.Input += v.Usage.InputTokens + v.Usage.CacheCreationTokens + v.Usage.CacheReadTokens
		s.u.Output += v.Usage.OutputTokens
	}
}

// usage is what the whole reply said it used, once it ended.
func (s *usageSniffer) usage() Usage {
	if len(s.buf) > 0 && !s.over && !s.sse {
		s.parse(bytes.TrimSpace(s.buf))
		s.buf = nil
	}
	return s.u
}

// lastEvent is an SSE event type that ends its reply: Anthropic's
// message_stop, a Chat [DONE], a Responses completion.
func lastEvent(t string) bool {
	switch t {
	case "message_stop", "response.completed", "response.incomplete", "response.failed", "error":
		return true
	}
	return false
}

// apiError is what a provider's error JSON says, in one line.
func apiError(b []byte, fallback string) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil && v.Error.Message != "" {
		return v.Error.Message
	}
	if fallback == "" {
		return "request failed"
	}
	return fallback
}

// errorType is the error's own type field, so a caller can tell a refusal
// apart from a rate limit.
func errorType(b []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
		Type  string          `json:"type"`
		Code  json.RawMessage `json:"code"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	name := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s)
		}
		return ""
	}
	if s := name(v.Error); s != "" {
		return s
	}
	return v.Type
}

// ---- small pieces taken from other deployed files, kept byte-equal ----

// renderUsage renders the reply and records the response id the body's
// own usage block carried.
func renderUsage(proto string, res Result, req *Request, usage *Usage) []byte {
	out := render(proto, res, req)
	sniff := newSniffer(proto, "application/json")
	sniff.write(out)
	usage.ResponseID = sniff.usage().ResponseID
	return out
}

// withFields sets top-level fields, keeping every other field as it was.
// Extracted verbatim from gateway.go.
func withFields(body []byte, fields map[string]any) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return body
	}
	for k, v := range fields {
		m[k] = v
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // <, > and & as the agent wrote them
	if enc.Encode(m) != nil {
		return body
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// refusedCode is the code of a stream's error event (its data) when it is
// the vendor's safety filter refusing, for a translation of it to keep.
// The plugin's runtime needs only to tag an Anthropic stream's own refusal
// kind (a request shouldn't be treated as a generic API failure, which
// would be retried against another account); the standalone version reads
// the error's own code/type without consulting a refusal-word table.
func refusedCode(data string) string {
	var v struct {
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(data), &v) != nil {
		return ""
	}
	for _, raw := range []json.RawMessage{v.Response.Error, v.Error, json.RawMessage(data)} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if msg, ok := policyRefusal(raw); ok {
			_ = msg
			if c := errorCode(raw); c != "" && c != "error" {
				return c
			}
			return "content_filter"
		}
	}
	return ""
}

// errorCode is the code a provider error object names itself.
func errorCode(raw json.RawMessage) string {
	var e struct {
		Code any    `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	if c, ok := e.Code.(string); ok && c != "" {
		return c
	}
	return e.Type
}

// filterReasons is the small set of vendor refusal kinds the runtime can
// see: only what an Anthropic error stream actually sends. The production
// table covers OpenAI/Azure/etc; anything beyond these is treated as a
// plain upstream error, not a refusal.
var filterReasons = map[string]string{
	"content_filter":           "content_filter",
	"content_policy_violation": "content_policy_violation",
	"refusal":                  "refusal",
	"invalid_prompt":           "invalid_prompt",
}

var policyCode = regexp.MustCompile(`^[a-z]+_policy$`)
var flaggedWords = regexp.MustCompile(`(?i)flagged|usage polic`)

// policyRefusal tells whether an error is the vendor's safety filter
// refusing the request. Extracted to keep refusedCode working without the
// full refusal-note machinery; the reason string is returned only so the
// caller's (string, bool) shape matches production's.
func policyRefusal(raw []byte) (string, bool) {
	var e struct {
		Code    any    `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   *struct {
			Code    any    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return "", false
	}
	code, typ, msg := e.Code, e.Type, e.Message
	if e.Error != nil {
		code, typ, msg = e.Error.Code, e.Error.Type, e.Error.Message
	}
	c, _ := code.(string)
	if c == "" {
		c = typ
	}
	note := func(reason string) (string, bool) {
		m := strings.TrimSpace(msg)
		if len(m) > 300 {
			m = m[:300] + "…"
		}
		if m != "" {
			return reason + " — " + m, true
		}
		return reason, true
	}
	switch r, ok := filterReasons[c]; {
	case ok:
		return note(r)
	case policyCode.MatchString(c):
		return note(c)
	case c == "invalid_prompt" && flaggedWords.MatchString(msg):
		return note(c)
	}
	return "", false
}

// newUUID is the run's user message id, as production's kiro.go makes it.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
