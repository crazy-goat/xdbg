package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// doAndWait fires a pre-built HTTP request in a goroutine, then waits for the
// Xdebug engine connection on the listener. The debugger is immediately detached
// so the script runs to completion — no follow-up run/step call needed.
// To debug interactively, use listen before triggering the request.
func (s *session) doAndWait(req *http.Request, timeout time.Duration) (string, error) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()

	// Open the ephemeral listener before firing the request so the Xdebug
	// connection arrives at an open port. The port is closed once the session
	// ends (adopt finishes), preventing stray browser requests from connecting.
	// If the port is busy (another debugger), acquireListener waits up to 10s.
	acceptResult, err := s.openOnce(timeout, 10*time.Second)
	if err != nil {
		return "", err
	}

	reqErr := make(chan error, 1)
	go func() {
		// No client timeout: the request legitimately blocks while paused at a breakpoint.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("request error: %v", err)
			reqErr <- err
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		log.Printf("request completed: %s %s -> %s", req.Method, req.URL, resp.Status)
	}()

	if err := s.waitForSession(ready, acceptResult, reqErr, timeout, "no Xdebug connection within %s — is Xdebug enabled in the container? (docker compose exec php set-xdebug-on)"); err != nil {
		return "", err
	}
	// A client error may have raced a handshake failure on the interrupt path.
	if err := s.handshakeError(); err != nil {
		return "", err
	}

	// adopt() already auto-ran when there were no breakpoints; the session
	// is done. Otherwise the engine is paused at the start of the script
	// (state="started") with breakpoints applied — return without detaching
	// so the caller can drive: run / step_* / eval / …
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state == "stopping" || state == "no session" {
		return "request fired; script ran to completion", nil
	}
	return "request fired; session paused at script start — call run/step to drive", nil
}

// requestErrorUnlessReady preserves a completed DBGp result after a late client
// error. A closed ready means adopt already finished; an accepted connection
// (acceptResult nil) means the handshake is still running and its result must
// win over the client error.
func (s *session) requestErrorUnlessReady(ready <-chan struct{}, acceptResult <-chan error, err error) error {
	select {
	case <-ready:
		return nil
	case aerr := <-acceptResult:
		if aerr == nil {
			return s.awaitHandshake(ready)
		}
	default:
	}
	if s.cancelPendingAccept(ready) {
		select {
		case <-ready:
			return nil
		default:
		}
		return s.awaitHandshake(ready)
	}
	return fmt.Errorf("request failed: %w", err)
}

// DoRequest fires an arbitrary HTTP request (method/headers/body) at the app,
// then waits for the resulting Xdebug engine connection, applies breakpoints
// (done in adopt) and runs to the first break.
//
// Because the container has xdebug.start_with_request=yes, any request makes
// php-fpm dial the DBGp port. The request is sent in a goroutine — it blocks at
// the breakpoint and won't return until the session is resumed — while we wait
// on the listener in the foreground.
func (s *session) DoRequest(rawurl, method string, headers map[string]string, body string, timeout time.Duration) (string, error) {
	if rawurl == "" {
		return "", fmt.Errorf("url required")
	}
	if method == "" {
		method = "GET"
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	req, err := http.NewRequest(strings.ToUpper(method), rawurl, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request build: %w", err)
	}
	applyHeaders(req, headers)

	return s.doAndWait(req, timeout)
}

func applyHeaders(req *http.Request, h map[string]string) {
	for k, v := range h {
		if strings.EqualFold(k, "Host") {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
}

// DoRequestFromFiles is like DoRequest but reads headers and body from files on
// disk. Use files to keep sensitive header values (tokens, cookies) out of tool arguments.
//
// headers_file accepts a JSON object with string values (one line or multiple
// lines), or HTTP-style "Name: Value" lines. The line format ignores blank lines
// and lines that start with #. Header names in the line format must use RFC 7230 token characters.
// Host sets the request Host, regardless of case.
// body_file contains raw request body bytes.
func (s *session) DoRequestFromFiles(rawurl, method, headersFile, bodyFile string, timeout time.Duration) (string, error) {
	if rawurl == "" {
		return "", fmt.Errorf("url required")
	}
	if method == "" {
		method = "GET"
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	var bodyReader io.Reader = strings.NewReader("")
	if bodyFile != "" {
		data, err := os.ReadFile(bodyFile)
		if err != nil {
			return "", fmt.Errorf("body_file: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(strings.ToUpper(method), rawurl, bodyReader)
	if err != nil {
		return "", fmt.Errorf("request build: %w", err)
	}

	if headersFile != "" {
		data, err := os.ReadFile(headersFile)
		if err != nil {
			return "", fmt.Errorf("headers_file: %w", err)
		}
		headers, err := parseHeadersFile(data)
		if err != nil {
			return "", fmt.Errorf("headers_file parse: %w", err)
		}
		applyHeaders(req, headers)
	}

	return s.doAndWait(req, timeout)
}

// parseHeadersFile parses a JSON object with string values or HTTP-style
// "Name: Value" lines. The line format ignores blank lines and lines that start with #.
func parseHeadersFile(data []byte) (map[string]string, error) {
	m := map[string]string{}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		// JSON null silently becomes an empty string, so inspect the raw values.
		var values map[string]json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		for name, value := range values {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("header %q must have a string value, not null", name)
			}
		}
		return m, nil
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			return nil, fmt.Errorf("invalid header line: %q", line)
		}
		name := strings.TrimSpace(line[:idx])
		if !validHeaderName(name) {
			return nil, fmt.Errorf("invalid header name %q on line %d", name, i+1)
		}
		m[name] = strings.TrimSpace(line[idx+1:])
	}
	return m, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}
