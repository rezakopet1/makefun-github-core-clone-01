// The forge's HTTP server: health probe, REST v3, GraphQL v4, MCP and git
// smart HTTP, all from one in-memory store. Lifecycle stays in main.go.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 32 * 1024 * 1024

var expectedToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

var requestCounter atomic.Uint64

func nextRequestID() string {
	return "req-" + itoa(int(requestCounter.Add(1)))
}

// forgeHandler is the whole server: one Store, routing, auth, JSON senders.
type forgeHandler struct {
	store *Store
}

func writeJSON(w http.ResponseWriter, status int, body []byte, extra map[string]string) {
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("Content-Length", itoa(len(body)))
	h.Set("X-GitHub-Request-Id", nextRequestID())
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		w.Write(body)
	}
}

func writeJSONValue(w http.ResponseWriter, status int, value any, extra map[string]string) {
	if value == nil {
		h := w.Header()
		h.Set("Content-Type", jsonType)
		h.Set("X-GitHub-Request-Id", nextRequestID())
		for k, v := range extra {
			h.Set(k, v)
		}
		w.WriteHeader(status)
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		writeJSON(w, 500, []byte("{\"message\":\"Internal Server Error\"}\n"), nil)
		return
	}
	writeJSON(w, status, append(raw, '\n'), extra)
}

func writeBytes(w http.ResponseWriter, status int, body []byte, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", itoa(len(body)))
	h.Set("X-GitHub-Request-Id", nextRequestID())
	w.WriteHeader(status)
	w.Write(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSONValue(w, status, map[string]any{"message": message}, nil)
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	limited := io.LimitReader(r.Body, maxBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	return raw, nil
}

var errBodyTooLarge = &httpError{"body too large"}

type httpError struct{ msg string }

func (e *httpError) Error() string { return e.msg }

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[len("Bearer "):])
}

func tokenAccepted(r *http.Request) bool {
	token := bearerToken(r)
	if token == "" {
		return false
	}
	if expectedToken != "" {
		return token == expectedToken
	}
	return true
}

// parsedTarget splits a raw request target the way the Node server did:
// raw (still %-encoded) path segments with '.' / '..' resolved and '\'
// treated as '/', plus the query string.
type parsedTarget struct {
	segs  []string
	query url.Values
}

func parseTarget(rawURL string) (*parsedTarget, bool) {
	raw := rawURL
	if raw == "" {
		raw = "/"
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c <= 0x20 || c == 0x7f {
			return nil, false
		}
	}
	pathPart, queryString := raw, ""
	if q := strings.Index(raw, "?"); q >= 0 {
		pathPart, queryString = raw[:q], raw[q+1:]
	}
	if h := strings.Index(pathPart, "#"); h >= 0 {
		pathPart = pathPart[:h]
	}
	if h := strings.Index(queryString, "#"); h >= 0 {
		queryString = queryString[:h]
	}
	if pathPart == "" {
		pathPart = "/"
	}
	pathPart = strings.ReplaceAll(pathPart, "\\", "/")
	var segs []string
	for _, s := range strings.Split(pathPart, "/") {
		switch s {
		case "", ".":
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, s)
		}
	}
	query, _ := url.ParseQuery(queryString)
	return &parsedTarget{segs: segs, query: query}, true
}

// lockMode classifies a route as read-only (shared lock) or mutating.
func (h *forgeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			writeError(w, 500, "Internal Server Error")
		}
	}()
	h.serve(w, r)
}

func (h *forgeHandler) serve(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	parsed, ok := parseTarget(r.RequestURI)
	if !ok {
		writeError(w, 404, "Not Found")
		return
	}
	segs, query := parsed.segs, parsed.query
	path := "/" + strings.Join(segs, "/")
	if len(segs) == 0 {
		path = "/"
	}

	if method == "GET" && path == "/healthz" {
		writeJSONValue(w, 200, map[string]any{"ok": true}, nil)
		return
	}

	// MCP: JSON-RPC over Streamable HTTP.
	if path == "/mcp" {
		if method == "DELETE" {
			hdr := w.Header()
			hdr.Set("X-GitHub-Request-Id", nextRequestID())
			w.WriteHeader(200)
			return
		}
		if method != "POST" {
			writeError(w, 404, "Not Found")
			return
		}
		if !tokenAccepted(r) {
			writeError(w, 401, "Requires authentication")
			return
		}
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		raw, err := readBody(r)
		if err != nil {
			writeError(w, 500, "Internal Server Error")
			return
		}
		var body map[string]any
		var idRaw json.RawMessage
		hasID := false
		if len(raw) > 0 {
			var parsedBody map[string]json.RawMessage
			if err := json.Unmarshal(raw, &parsedBody); err != nil || parsedBody == nil {
				writeJSONValue(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}}, nil)
				return
			}
			// Re-decode into generic values for the handler, keeping the raw id.
			var generic map[string]any
			if err := json.Unmarshal(raw, &generic); err != nil {
				writeJSONValue(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}}, nil)
				return
			}
			body = generic
			if rawID, ok := parsedBody["id"]; ok {
				idRaw = rawID
				hasID = true
			}
		}
		status, outcome := HandleMcp(h.store, body, hasID, idRaw)
		if status == 202 {
			hdr := w.Header()
			hdr.Set("X-GitHub-Request-Id", nextRequestID())
			w.WriteHeader(202)
			return
		}
		writeJSONValue(w, status, outcome, nil)
		return
	}

	// GraphQL v4.
	if path == "/api/graphql" {
		if method != "POST" {
			writeError(w, 404, "Not Found")
			return
		}
		if !tokenAccepted(r) {
			writeError(w, 401, "Requires authentication")
			return
		}
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		raw, err := readBody(r)
		if err != nil {
			writeError(w, 500, "Internal Server Error")
			return
		}
		var body any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				writeError(w, 400, "malformed JSON body")
				return
			}
		}
		status, outcome := HandleGraphqlOperation(h.store, body)
		writeJSONValue(w, status, outcome, nil)
		return
	}

	// REST v3.
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if !tokenAccepted(r) {
			writeError(w, 401, "Requires authentication")
			return
		}
		restSegs := segs[2:]
		origin := "http://" + r.Host
		if origin == "http://" {
			origin = "http://127.0.0.1"
		}
		isWrite := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
		if isWrite {
			h.store.mu.Lock()
			defer h.store.mu.Unlock()
		} else {
			h.store.mu.RLock()
			defer h.store.mu.RUnlock()
		}
		var body map[string]any
		if isWrite {
			raw, err := readBody(r)
			if err != nil {
				writeError(w, 500, "Internal Server Error")
				return
			}
			if len(raw) > 0 {
				var parsedBody any
				if err := json.Unmarshal(raw, &parsedBody); err != nil {
					writeError(w, 400, "malformed JSON body")
					return
				}
				obj, isObj := parsedBody.(map[string]any)
				if parsedBody != nil && !isObj {
					writeError(w, 400, "malformed JSON body")
					return
				}
				body = obj
			}
		}
		result := HandleRest(h.store, method, restSegs, query, body, origin)
		h.sendRestResult(w, result)
		return
	}

	// Git smart HTTP.
	h.serveGit(w, r, method, segs, query)
}

func (h *forgeHandler) sendRestResult(w http.ResponseWriter, result restResult) {
	if result.status == 204 {
		hdr := w.Header()
		hdr.Set("Content-Type", jsonType)
		hdr.Set("X-GitHub-Request-Id", nextRequestID())
		w.WriteHeader(204)
		return
	}
	if result.bodyBytes != nil {
		writeJSON(w, result.status, result.bodyBytes, result.headers)
		return
	}
	writeJSONValue(w, result.status, result.body, result.headers)
}

// ---- git smart HTTP ---------------------------------------------------------

func (h *forgeHandler) serveGit(w http.ResponseWriter, r *http.Request, method string, segs []string, query url.Values) {
	if len(segs) < 2 || !strings.HasSuffix(segs[1], ".git") {
		writeError(w, 404, "Not Found")
		return
	}
	owner, err1 := url.PathUnescape(segs[0])
	repoName, err2 := url.PathUnescape(strings.TrimSuffix(segs[1], ".git"))
	if err1 != nil || err2 != nil {
		writeError(w, 404, "Not Found")
		return
	}
	tail := segs[2:]
	if method == "GET" && len(tail) == 2 && tail[0] == "info" && tail[1] == "refs" {
		service := query.Get("service")
		h.store.mu.RLock()
		repo := h.store.GetRepo(owner, repoName)
		if repo == nil || (service != "git-upload-pack" && service != "git-receive-pack") {
			h.store.mu.RUnlock()
			writeError(w, 404, "Not Found")
			return
		}
		wantV2 := service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2"
		var out []byte
		var ctype string
		if wantV2 {
			out = v2AdvertisementBytes
			ctype = "application/x-git-upload-pack-advertisement"
		} else if service == "git-upload-pack" {
			out = V0Advertisement(h.store, repo, "git-upload-pack")
			ctype = "application/x-git-upload-pack-advertisement"
		} else {
			out = V0Advertisement(h.store, repo, "git-receive-pack")
			ctype = "application/x-git-receive-pack-advertisement"
		}
		h.store.mu.RUnlock()
		writeBytes(w, 200, out, ctype)
		return
	}
	if method == "POST" && len(tail) == 1 && tail[0] == "git-upload-pack" {
		h.store.mu.RLock()
		defer h.store.mu.RUnlock()
		repo := h.store.GetRepo(owner, repoName)
		if repo == nil {
			writeError(w, 404, "Not Found")
			return
		}
		raw, err := readBody(r)
		if err != nil {
			writeError(w, 500, "Internal Server Error")
			return
		}
		isV2 := r.Header.Get("Git-Protocol") == "version=2"
		if isV2 {
			parsed := ParseV2Request(raw)
			if parsed == nil {
				writeBytes(w, 200, pktFlushBytes, "application/x-git-upload-pack-result")
				return
			}
			switch parsed.command {
			case "ls-refs":
				writeBytes(w, 200, LsRefsResponse(h.store, repo, parsed), "application/x-git-upload-pack-result")
			case "fetch":
				writeBytes(w, 200, V2FetchResponse(h.store, repo, parsed), "application/x-git-upload-pack-result")
			default:
				writeBytes(w, 200, pktFlushBytes, "application/x-git-upload-pack-result")
			}
			return
		}
		wants, haves, shallow, depth, hasDepth := ParseV0Fetch(raw)
		writeBytes(w, 200, V0FetchResponse(h.store, repo, wants, haves, depth, hasDepth, shallow), "application/x-git-upload-pack-result")
		return
	}
	if method == "POST" && len(tail) == 1 && tail[0] == "git-receive-pack" {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		repo := h.store.GetRepo(owner, repoName)
		if repo == nil {
			writeError(w, 404, "Not Found")
			return
		}
		raw, err := readBody(r)
		if err != nil {
			writeError(w, 500, "Internal Server Error")
			return
		}
		commands, pack := ParseReceivePack(raw)
		report := ApplyPush(h.store, repo, commands, pack)
		writeBytes(w, 200, PushReportResponse(report), "application/x-git-receive-pack-result")
		return
	}
	writeError(w, 404, "Not Found")
}
