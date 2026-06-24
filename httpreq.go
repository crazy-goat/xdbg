package main

import (
	"bytes"
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
// To debug interactively, use docker_xdebug_listen before triggering the request.
func (s *session) doAndWait(req *http.Request, timeout time.Duration) (string, error) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()

	// Open the ephemeral listener before firing the request so the Xdebug
	// connection arrives at an open port. The port is closed once the session
	// ends (adopt finishes), preventing stray browser requests from connecting.
	// If the port is busy (another debugger), acquireListener waits up to 10s.
	if err := s.openOnce(timeout, 10*time.Second); err != nil {
		return "", err
	}

	go func() {
		// No client timeout: the request legitimately blocks while paused at a breakpoint.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("request error: %v", err)
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		log.Printf("request completed: %s %s -> %s", req.Method, req.URL, resp.Status)
	}()

	select {
	case <-ready:
		// adopt() already auto-ran when there were no breakpoints; the session
		// is done. Otherwise the engine is paused at the start of the script
		// (state="started") with breakpoints applied — return without detaching
		// so the caller can drive: docker_xdebug_run / _step_* / _eval / …
		s.mu.Lock()
		state := s.state
		s.mu.Unlock()
		if state == "stopping" || state == "no session" {
			return "request fired; script ran to completion", nil
		}
		return "request fired; session paused at script start — call run/step to drive", nil
	case <-time.After(timeout):
		s.closeLn()
		return "", fmt.Errorf("no Xdebug connection within %s — is Xdebug enabled in the container? (docker compose exec php-sub-api xdebug 1)", timeout)
	}
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return s.doAndWait(req, timeout)
}

// DoRequestFromFiles is like DoRequest but reads headers and body from files on
// disk. Use this when headers contain sensitive values (tokens, cookies) that
// should not appear inline in tool arguments.
//
// headers_file format: JSON object {"Name": "value"} or HTTP-style lines
// "Name: Value" (one per line, # comments ignored).
// body_file: raw request body bytes.
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
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}

	return s.doAndWait(req, timeout)
}

// parseHeadersFile parses a headers file with HTTP-style "Name: Value" lines.
// Blank lines and lines starting with # are ignored.
func parseHeadersFile(data []byte) (map[string]string, error) {
	m := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			return nil, fmt.Errorf("invalid header line: %q", line)
		}
		m[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}
	return m, nil
}
