package bridge

// The standalone HTTP service: bridge-auth-v1 verification on the plugin's
// POST /v1/messages, plus the MCP callback the packaged helper calls. The
// auth key is runtime-generated and kept in a file the plugin setup script
// never reads; a stale or wrong key is refused before any Claude run
// starts, matching production's behavior of treating an unsigned hook as
// no hook.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runtimeTUI is the extracted subscription runtime's HTTP service. It owns
// the bridge and the verification key the plugin handshakes over.
type runtimeTUI struct {
	bridge *subscriptionBridge
	key    []byte // bridge-auth-v1 shared secret
	mu     sync.Mutex
	// clock is a field so a test can pin the timestamp window.
	clock func() time.Time
}

// NewRuntime makes a runtime bound to cfg's models/helper/bridge. key is
// the raw bytes of the bridge-auth-v1 key, exactly as the plugin computes
// them: the plugin signs its requests with a key derived from the value
// the runtime wrote to --key-file, so callers must hand the same bytes.
func NewRuntime(key []byte) *runtimeTUI {
	return &runtimeTUI{
		bridge: newSubscriptionBridge(),
		key:    append([]byte(nil), key...),
		clock:  time.Now,
	}
}

// serveHTTP routes: only the MCP callback and the signed messages endpoint.
// Anything else is refused with a 404 so the loopback surface stays small.
func (rt *runtimeTUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		rt.serveSignedMessages(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/_magpie/claude-mcp/"):
		token := strings.TrimPrefix(r.URL.Path, "/_magpie/claude-mcp/")
		if token == "" {
			http.Error(w, "missing run token", http.StatusNotFound)
			return
		}
		// keep path-shape identical to production so mcpCall needs no edits
		mr := new(http.Request)
		*mr = *r
		mr.URL = cloneURL(r.URL)
		mr.URL.Path = "/_magpie/claude-mcp/"
		// PathValue is filled by the mux; emulate it for the extracted one.
		rt.serveMCP(w, mr, token)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func cloneURL(u *url.URL) *url.URL {
	v := *u
	return &v
}

// serveMCP hands a run token to the bridge's mcpCall; the token is the
// last path element so RunMCP's /_magpie/claude-mcp/{token} holds.
func (rt *runtimeTUI) serveMCP(w http.ResponseWriter, r *http.Request, token string) {
	// Reuse production mcpCall; it reads r.PathValue("token"), so attach it.
	r2 := r.WithContext(context.WithValue(r.Context(), mcpTokenKey{}, token))
	rt.bridge.mcpCall(w, r2)
}

type mcpTokenKey struct{}

// bridgeAuthV1 verifies the plugin's bridge-auth-v1 signature on body.
// Returns the session the request was authenticated for, or an error whose
// text is safe to send back. Timestamp window and replay cache match the
// spec the plugin implements: |now-ts| <= 300s, nonce unique within it.
func (rt *runtimeTUI) bridgeAuthV1(r *http.Request, body []byte) (string, error) {
	const window = 300
	ts := strings.TrimSpace(r.Header.Get("x-bridge-timestamp"))
	nonce := strings.TrimSpace(r.Header.Get("x-bridge-nonce"))
	session := strings.TrimSpace(r.Header.Get("x-bridge-session"))
	sig := strings.TrimSpace(r.Header.Get("x-bridge-mac"))
	if r.Header.Get("x-bridge-auth-version") != "1" {
		return "", errors.New("bridge-auth-v1 version mismatch")
	}
	if ts == "" || nonce == "" || session == "" || sig == "" {
		return "", errors.New("bridge-auth-v1 headers missing")
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || abs64(rt.clock().Unix()-n) > window {
		return "", errors.New("bridge-auth-v1 timestamp out of range")
	}
	// HMAC over the canonical string the plugin built.
	bodyHash := sha256Hex(body)
	canonical := strings.Join([]string{
		"bridge-auth-v1", r.Method, "/v1/messages", ts, nonce, session, bodyHash,
	}, "\n")
	want := hmacSHA256Hex(rt.key, canonical)
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return "", errors.New("bridge-auth-v1 signature rejected")
	}
	return session, nil
}

// serveSignedMessages unwraps bridge-auth-v1 then calls serveMessages with
// the session-derived owner. The plugin's stable session id is already an
// opaque sha256 of the caller's own session label; the runtime treats it as
// the outer session without re-deriving.
func (rt *runtimeTUI) serveSignedMessages(w http.ResponseWriter, r *http.Request) {
	body, status, err := readBoundedRequestBody(w, r, requestLimits{body: 16 << 20}, nil)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	session, err := rt.bridgeAuthV1(r, body)
	if err != nil {
		writeError(w, protoAnthropic, http.StatusUnauthorized, "magpie-tui-runtime: "+err.Error())
		return
	}
	// model must be one the runtime advertised in --models-file. A request
	// for anything else is refused before a Claude run starts; the plugin's
	// own validation is a convenience, not the trust boundary.
	model := ""
	var q struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &q) == nil {
		model = strings.TrimSpace(q.Model)
	}
	if model == "" {
		writeError(w, protoAnthropic, http.StatusBadRequest, "model is required")
		return
	}
	if _, ok := config.Models[model]; !ok {
		writeError(w, protoAnthropic, http.StatusBadRequest, fmt.Sprintf("model %q is not served by this runtime", model))
		return
	}
	var usage Usage
	// The bridge session is the conversation's outer session: the plugin
	// derives it from the caller's own session label and signs it, so it
	// is the canonical key checkpoint matching uses. sessionOf(r.Header)
	// would read agent headers the plugin's fetch does not forward.
	rt.serveMessages(w, r, model, session, body, &usage)
}

// ---- small crypto helpers, exact match to the plugin's signing side ----

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSHA256Hex(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	io.WriteString(m, msg)
	return hex.EncodeToString(m.Sum(nil))
}

// sha256New / hexEncode12 are the hash helpers firstWords needs; kept here
// rather than re-importing crypto in helpers.go twice.
func sha256New() hashHash { return sha256.New() }

func hexEncode12(h hashHash) string {
	return hex.EncodeToString(h.Sum(nil)[:12])
}

type hashHash interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

// abs64 is math.Abs for int64 without the conversion pitfalls.
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Shutdown ends every run the bridge owns; called on SIGTERM/SIGINT so
// parked MCP calls are answered and Claude processes are cleaned up
// instead of left running under the user's init.
func (rt *runtimeTUI) Shutdown() {
	rt.bridge.abortAll()
}
