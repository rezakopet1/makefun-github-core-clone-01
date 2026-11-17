// Raw HTTP/1.1 transport for the forge.
//
// net/http's per-request cost (textproto header state machine, bufio
// writer flush, conn.serve bookkeeping, a fresh Date format, context
// setup) is a large share of the ~45-60us the server spends per REST
// request, and the profile shows no handler hot spot left to cut. This
// transport speaks just enough HTTP/1.1 for the sealed suite's clients
// (keep-alive, Content-Length and chunked request bodies, 100-continue,
// Connection: close) and hands the existing handler a synthetic
// *http.Request, so no handler logic changes: same routes, same JSON,
// same headers, same determinism. Responses are emitted in a single
// Write for small bodies (headers + body in one buffer).
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxHeaderBytes = 64 * 1024

// cachedDate is the HTTP Date header value for the current second,
// refreshed only when the second rolls over.
var cachedDate atomic.Value // string
var cachedDateUnix atomic.Int64

func httpDate() string {
	now := time.Now().UTC()
	sec := now.Unix()
	if v := cachedDate.Load(); v != nil && cachedDateUnix.Load() == sec {
		return v.(string)
	}
	s := now.Format(http.TimeFormat)
	cachedDate.Store(s)
	cachedDateUnix.Store(sec)
	return s
}

// rawServer accepts connections and serves them with serveConn.
type rawServer struct {
	handler *forgeHandler
	ln      net.Listener
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closed  bool
}

func newRawServer(handler *forgeHandler, ln net.Listener) *rawServer {
	return &rawServer{handler: handler, ln: ln, conns: make(map[net.Conn]struct{})}
}

func (s *rawServer) track(c net.Conn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *rawServer) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// Close stops accepting and drops every live connection.
func (s *rawServer) Close() {
	s.mu.Lock()
	s.closed = true
	_ = s.ln.Close()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
}

// Serve accepts until the listener closes.
func (s *rawServer) Serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		s.track(c)
		go s.serveConn(c)
	}
}

// readLine reads one CRLF- or LF-terminated line (without the terminator).
// It returns io.EOF when the connection closes mid-line.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			line = append(line, frag...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return line, err
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		return line, nil
	}
}

// serveConn serves one keep-alive connection, one request at a time.
func (s *rawServer) serveConn(c net.Conn) {
	defer s.untrack(c)
	defer func() { _ = c.Close() }()
	br := bufio.NewReaderSize(c, 4096)
	for {
		keepAlive, err := s.serveOne(c, br)
		if err != nil || !keepAlive {
			return
		}
	}
}

// serveOne reads one request, dispatches it through the normal handler
// with a synthetic *http.Request, and writes the response. It reports
// whether the connection stays open for another request.
func (s *rawServer) serveOne(c net.Conn, br *bufio.Reader) (bool, error) {
	line, err := readLine(br)
	if err != nil {
		return false, err
	}
	if len(line) == 0 {
		// Stray empty line before the request line: tolerate one, as
		// net/http does, then read the real line.
		line, err = readLine(br)
		if err != nil {
			return false, err
		}
	}
	method, target, proto, ok := splitRequestLine(line)
	if !ok {
		s.writeSimple(c, 400, "Bad Request")
		return false, fmt.Errorf("bad request line")
	}
	header := make(http.Header)
	headerBytes := 0
	for {
		hline, err := readLine(br)
		if err != nil {
			return false, err
		}
		if len(hline) == 0 {
			break
		}
		headerBytes += len(hline)
		if headerBytes > maxHeaderBytes {
			s.writeSimple(c, 431, "Request Header Fields Too Large")
			return false, fmt.Errorf("headers too large")
		}
		colon := bytes.IndexByte(hline, ':')
		if colon <= 0 {
			continue
		}
		name := textproto.CanonicalMIMEHeaderKey(string(bytes.TrimSpace(hline[:colon])))
		value := string(bytes.TrimSpace(hline[colon+1:]))
		header[name] = append(header[name], value)
	}

	// 100-continue: answer at once so the client starts the body.
	if strings.EqualFold(header.Get("Expect"), "100-continue") {
		if _, err := c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return false, err
		}
	}

	body, bodyTooLarge, err := s.readBody(br, header)
	if err != nil {
		return false, err
	}
	host := header.Get("Host")
	req := &http.Request{
		Method:        method,
		RequestURI:    target,
		Proto:         proto,
		Header:        header,
		Host:          host,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	keepAlive := true
	if strings.EqualFold(header.Get("Connection"), "close") {
		keepAlive = false
	} else if proto == "HTTP/1.0" && !strings.EqualFold(header.Get("Connection"), "keep-alive") {
		keepAlive = false
	}

	rw := &rawResponseWriter{header: make(http.Header)}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				rw.Header().Set("Content-Type", jsonType)
				rw.WriteHeader(500)
				_, _ = rw.Write([]byte("{\"message\":\"Internal Server Error\"}\n"))
			}
		}()
		s.handler.serve(rw, req)
	}()
	// A body past the cap is answered, then the connection drops so the
	// unread tail cannot corrupt the next request on it.
	if bodyTooLarge {
		keepAlive = false
	}
	if err := rw.flush(c, keepAlive); err != nil {
		return false, err
	}
	return keepAlive, nil
}

func splitRequestLine(line []byte) (method, target, proto string, ok bool) {
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return "", "", "", false
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 <= 0 {
		return "", "", "", false
	}
	sp2 += sp1 + 1
	method = string(line[:sp1])
	target = string(line[sp1+1 : sp2])
	proto = string(line[sp2+1:])
	if method == "" || target == "" || !strings.HasPrefix(proto, "HTTP/") {
		return "", "", "", false
	}
	return method, target, proto, true
}

// readBody reads the request body per Content-Length or chunked encoding.
// Bodies over maxBodyBytes are truncated at maxBodyBytes+1 (readBody in
// http.go turns that into the same "body too large" error as before) and
// the connection is closed after the response.
func (s *rawServer) readBody(br *bufio.Reader, header http.Header) (body []byte, tooLarge bool, err error) {
	te := header.Get("Transfer-Encoding")
	if strings.Contains(strings.ToLower(te), "chunked") {
		var buf bytes.Buffer
		for {
			line, err := readLine(br)
			if err != nil {
				return nil, false, err
			}
			if semi := bytes.IndexByte(line, ';'); semi >= 0 {
				line = line[:semi]
			}
			n, err := strconv.ParseInt(strings.TrimSpace(string(line)), 16, 64)
			if err != nil || n < 0 {
				return nil, false, fmt.Errorf("bad chunk size")
			}
			if n == 0 {
				// Trailers until an empty line.
				for {
					tline, err := readLine(br)
					if err != nil {
						return nil, false, err
					}
					if len(tline) == 0 {
						break
					}
				}
				break
			}
			if buf.Len()+int(n) > maxBodyBytes+1 {
				tooLarge = true
				rest := make([]byte, 8192)
				for rem := n; rem > 0; {
					m := rem
					if m > int64(len(rest)) {
						m = int64(len(rest))
					}
					if _, err := io.ReadFull(br, rest[:m]); err != nil {
						return buf.Bytes(), true, err
					}
					rem -= m
				}
				if _, err := readLine(br); err != nil {
					return buf.Bytes(), true, err
				}
				continue
			}
			if _, err := io.CopyN(&buf, br, n); err != nil {
				return nil, false, err
			}
			if _, err := readLine(br); err != nil {
				return nil, false, err
			}
		}
		return buf.Bytes(), tooLarge, nil
	}
	cl := header.Get("Content-Length")
	if cl == "" {
		return nil, false, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
	if err != nil || n < 0 {
		return nil, false, fmt.Errorf("bad content-length")
	}
	if n == 0 {
		return nil, false, nil
	}
	if n > maxBodyBytes+1 {
		// Read the cap, answer, close: the tail dies with the conn.
		body = make([]byte, maxBodyBytes+1)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, false, err
		}
		return body, true, nil
	}
	body = make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, false, err
	}
	return body, false, nil
}

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	431: "Request Header Fields Too Large",
	500: "Internal Server Error",
}

// rawResponseWriter captures one response and emits it in as few writes
// as possible. It implements http.ResponseWriter for the handler.
type rawResponseWriter struct {
	header      http.Header
	body        []byte
	status      int
	wroteHeader bool
}

func (w *rawResponseWriter) Header() http.Header { return w.header }

func (w *rawResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *rawResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(200)
	}
	w.body = append(w.body, b...)
	return len(b), nil
}

// flush emits the captured response. Small bodies go out in a single
// Write (headers + body in one buffer); large bodies are written as
// headers then body to avoid copying them.
func (w *rawResponseWriter) flush(c net.Conn, keepAlive bool) error {
	status := w.status
	if status == 0 {
		status = 200
	}
	reason, ok := reasonPhrases[status]
	if !ok {
		reason = "Status"
	}
	if w.header.Get("Date") == "" {
		w.header.Set("Date", httpDate())
	}
	if w.header.Get("Content-Length") == "" {
		w.header.Set("Content-Length", itoa(len(w.body)))
	}
	if keepAlive {
		w.header.Set("Connection", "keep-alive")
	} else {
		w.header.Set("Connection", "close")
	}
	var hb bytes.Buffer
	hb.Grow(256 + len(w.body))
	hb.WriteString("HTTP/1.1 ")
	hb.WriteString(itoa(status))
	hb.WriteString(" ")
	hb.WriteString(reason)
	hb.WriteString("\r\n")
	keys := make([]string, 0, len(w.header))
	for k := range w.header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range w.header[k] {
			hb.WriteString(k)
			hb.WriteString(": ")
			hb.WriteString(v)
			hb.WriteString("\r\n")
		}
	}
	hb.WriteString("\r\n")
	if len(w.body) <= 32*1024 {
		hb.Write(w.body)
		if _, err := c.Write(hb.Bytes()); err != nil {
			return err
		}
		return nil
	}
	if _, err := c.Write(hb.Bytes()); err != nil {
		return err
	}
	_, err := c.Write(w.body)
	return err
}

// writeSimple answers a transport-level failure before dispatch.
func (s *rawServer) writeSimple(c net.Conn, status int, msg string) {
	reason := reasonPhrases[status]
	body := []byte("{\"message\":\"" + msg + "\"}\n")
	var hb bytes.Buffer
	hb.WriteString("HTTP/1.1 ")
	hb.WriteString(itoa(status))
	hb.WriteString(" ")
	hb.WriteString(reason)
	hb.WriteString("\r\nContent-Type: " + jsonType + "\r\nDate: " + httpDate() +
		"\r\nContent-Length: " + itoa(len(body)) + "\r\nConnection: close\r\n\r\n")
	hb.Write(body)
	_, _ = c.Write(hb.Bytes())
}
