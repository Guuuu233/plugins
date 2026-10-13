package bridge

// HTTP surface of the extracted runtime: /v1/messages over the existing
// bridge-auth-v1 signature, plus /_magpie/claude-mcp/{token} for the MCP
// helper. Replaces the deployed gateway's route registration; Anthropic is
// the only protocol served, matching the plugin's frontend.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// SessionHeader names the session a call is part of, for the usage log to
// tell several sessions on one model apart; without it, the session an
// agent names itself in sessionHeaders is taken.
const SessionHeader = "X-Magpie-Session"

// sessionHeaders are where agents already name their conversation: OpenCode
// (to its own gateway, and to everyone else), Pi, Codex and Claude Code.
var sessionHeaders = []string{
	"x-opencode-session", "x-session-affinity", "x-session-id",
	"session_id", "session-id", "x-claude-code-session-id",
}

// sessionOf is the session an agent's request says it belongs to, "" when
// it names none.
func sessionOf(in http.Header) string {
	for _, h := range append([]string{SessionHeader}, sessionHeaders...) {
		if v := strings.TrimSpace(in.Get(h)); v != "" {
			if len(v) > 128 {
				v = v[:128]
			}
			return v
		}
	}
	return ""
}

// requestLimits bounds a request's body: its size, and how long a slow
// client may keep it open. The plugin's runtime has no compression or
// remote decode hooks, so decode stays nil.
type requestLimits struct {
	body        int64
	readTimeout time.Duration
}

const (
	defaultBodyLimit   = 16 << 20
	defaultBodyTimeout = 30 * time.Second
)

// readBoundedRequestBody is the extracted equivalent of the gateway's
// bounded body reader, without the decompression hook the host supplies
// for remote-served traffic. A request over the limit or stalled is
// refused, never truncated silently.
func readBoundedRequestBody(w http.ResponseWriter, r *http.Request, limits requestLimits, decode func(*http.Request) (io.ReadCloser, error)) ([]byte, int, error) {
	controller := http.NewResponseController(w)
	reject := func(status int, err error) ([]byte, int, error) {
		w.Header().Set("Connection", "close")
		_ = controller.SetReadDeadline(time.Now())
		if r.Body != nil {
			r.Body.Close()
		}
		_ = controller.SetReadDeadline(time.Time{})
		return nil, status, err
	}
	if limits.body <= 0 {
		limits.body = defaultBodyLimit
	}
	if limits.readTimeout <= 0 {
		limits.readTimeout = defaultBodyTimeout
	}
	if r.ContentLength > limits.body {
		return reject(http.StatusRequestEntityTooLarge, errors.New("request body exceeds runtime limit"))
	}
	if err := controller.SetReadDeadline(time.Now().Add(limits.readTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return reject(http.StatusInternalServerError, errors.New("could not set request body deadline"))
	}
	defer controller.SetReadDeadline(time.Time{})
	readFailure := func(err error) ([]byte, int, error) {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return reject(http.StatusRequestTimeout, errors.New("request body read timed out"))
		}
		return reject(http.StatusBadRequest, err)
	}
	rd := r.Body
	if decode != nil {
		var err error
		rd, err = decode(r)
		if err != nil {
			return readFailure(err)
		}
		defer rd.Close()
	}
	body, err := io.ReadAll(io.LimitReader(rd, limits.body+1))
	if int64(len(body)) > limits.body {
		return reject(http.StatusRequestEntityTooLarge, errors.New("request body exceeds runtime limit"))
	}
	if err != nil {
		return readFailure(err)
	}
	return body, 0, nil
}

// writeJSON is the gateway's JSON answer, unchanged in shape.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // a vendor's <, > and & as it wrote them (#260)
	_ = enc.Encode(v)
}

// tooLong/paceWords/quotaWords come from the deployed gateway so errors
// land with the same status and hint Anthropic callers would see.
var quotaWords = regexp.MustCompile(`(?i)quota|insufficient|balance|credit|billing|out of budget|budget (exceeded|exhausted)|exceeded|rate.?limit|usage.?limit|limit.?reached|hit your .*limit|limit.{0,24}resets|too many requests|overloaded|余额|额度|欠费|限流|频率|套餐|用量|上限`)

var paceWords = regexp.MustCompile(`(?i)rate.?limit|per (minute|hour|day)|\bTP[MD]\b|please wait|try again later|throttl`)

var tooLongRe = regexp.MustCompile(`(?i)context_length_exceeded|prompt is too long|input is too long|(exceeds?|exceeded|over|beyond)( the)?( model'?s?)?( maximum)? context|context (length|limit|window) (exceeded|is exceeded)|maximum context length|too many (input |prompt )?tokens|上下文(长度)?(超|过长)|超(过|出)(了)?(模型)?(的)?(最大)?上下文` +
	`|input token count.*exceeds the maximum|maximum prompt length is \d|reduce the length of the messages|maximum allowed input length|longer than the model'?s context length|available context size|greater than the context length|prompt token count of [\d,]+ exceeds the limit|context window exceeds limit|exceeded model token limit|configured context size|prompt too long|prompt exceeds max length|context_window_exceeded|exceeded (max |maximum )?context length|range of input length should be|token limit exceeded`)

// tooLong is whether a vendor's error says the conversation no longer
// fits its model: said the way the client's own API says it, so the agent
// compacts the conversation and tries again rather than stopping.
func tooLong(status int, msg string) bool {
	if status < 400 || status >= 500 || status == http.StatusTooManyRequests {
		return false
	}
	m := strings.ToLower(msg)
	if strings.Contains(m, "max_tokens") || strings.Contains(m, "max_output_tokens") || strings.Contains(m, "max_completion_tokens") {
		return false
	}
	if paceWords.MatchString(msg) {
		return false
	}
	return tooLongRe.MatchString(msg)
}

// writeError answers in Anthropic's own error shape; the plugin's runtime
// serves only Anthropic Messages.
func writeError(w http.ResponseWriter, proto string, status int, msg string) int {
	typ := "api_error"
	switch {
	case status == 400:
		typ = "invalid_request_error"
	case status == 401:
		typ = "authentication_error"
	case status == 403:
		typ = "permission_error"
	case status == 404:
		typ = "not_found_error"
	case status == 413:
		typ = "request_too_large"
	case status == 429:
		typ = "rate_limit_error"
	case status == 529:
		typ = "overloaded_error"
	}
	var code any
	if tooLong(status, msg) {
		status, typ, code = 400, "invalid_request_error", "context_length_exceeded"
		if !strings.Contains(strings.ToLower(msg), "prompt is too long") {
			msg = "prompt is too long: " + msg
		}
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg, "code": code}})
	return status
}

// keepaliveGap is how long a translated reply's client may hear nothing
// while its provider is heard from (a keepalive, an event that has nothing
// for the client) before it is sent a keepalive of its own.
var keepaliveGap = time.Second

// keepaliveEvery is how often a relayed reply that has gone quiet is kept
// alive, and keepaliveLongest how long it is kept so with no event: a
// reply stuck longer is left for the client's own idle timeout to end.
var keepaliveEvery, keepaliveLongest = 15 * time.Second, 5 * time.Minute

// streamEncoder is the plugin's one relay encoder: Anthropic's own
// message SSE. The interface matches the deployed one so relay() drops in.
type streamEncoder interface {
	event(Event)
	finish()
	// keepalive tells the client the reply goes on, as its protocol does
	// with no answer to give: one its idle timeout counts (#436)
	keepalive()
}

// relayEvents hands see each of events until they end or see says stop,
// keeping the client of sw alive while none comes.
func relayEvents(events <-chan Event, sw *sseWriter, enc streamEncoder, see func(Event) bool) {
	tick := time.NewTicker(keepaliveEvery)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			last = time.Now()
			if !see(ev) {
				return
			}
		case <-tick.C:
			if time.Since(last) < keepaliveLongest && sw.quiet() >= keepaliveEvery/2 {
				enc.keepalive()
			}
		}
	}
}

// encoder makes the stream writer Anthropic's plugin protocol needs;
// proto is kept so the signature matches relay()'s callers verbatim, and
// is asserted to be Anthropic by the caller.
func encoder(proto string, w *sseWriter, r *Request, usage ...*Usage) streamEncoder {
	if proto != protoAnthropic {
		panic("magpie-tui runtime serves only Anthropic messages")
	}
	var enc streamEncoder = &anthropicEncoder{w: w, model: r.Model}
	if len(usage) > 0 && usage[0] != nil {
		enc = &usageEncoder{streamEncoder: enc, usage: usage[0]}
	}
	return enc
}

// usageEncoder counts the reply's usage events, as the gateway's does.
type usageEncoder struct {
	streamEncoder
	usage *Usage
}

func (e *usageEncoder) finish() {
	e.streamEncoder.finish()
}

// sseWriter writes events to a client and flushes each one.
type sseWriter struct {
	w     http.ResponseWriter
	f     http.Flusher
	begun bool
	wrote time.Time // when the client was last written to
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

func (s *sseWriter) begin() {
	if s.begun {
		return
	}
	s.begun = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flush()
	s.wrote = time.Now()
}

func (s *sseWriter) flush() {
	if s.f != nil {
		s.f.Flush()
	}
}

// quiet is how long the client has had nothing from s; a stream not yet
// begun has been quiet for ever.
func (s *sseWriter) quiet() time.Duration {
	if !s.begun {
		return 1<<63 - 1
	}
	return time.Since(s.wrote)
}

// event writes one event; a JSON-marshallable value or a raw string.
// Same byte layout as production's sseWriter so SSE consumers see the
// same frames.
func (s *sseWriter) event(name string, v any) {
	s.begin()
	var b bytes.Buffer
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	switch x := v.(type) {
	case string:
		b.WriteString(x)
	case []byte:
		b.Write(x)
	case json.RawMessage:
		b.Write(x)
	default:
		j, _ := json.Marshal(x)
		b.Write(j)
	}
	b.WriteString("\n\n")
	s.w.Write(b.Bytes())
	s.flush()
	s.wrote = time.Now()
}

// comment writes an SSE comment; see sse.go in the deployed source.
func (s *sseWriter) comment(text string) {
	s.begin()
	io.WriteString(s.w, ": "+text+"\n")
	s.flush()
	s.wrote = time.Now()
}
