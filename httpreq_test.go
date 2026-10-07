package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseHeadersFile_JSON(t *testing.T) {
	want := map[string]string{"Authorization": "Bearer x", "X-A": "1"}
	for name, data := range map[string]string{
		"one line": `{"Authorization":"Bearer x","X-A":"1"}`,
		"pretty printed": `{
  "Authorization": "Bearer x",
  "X-A": "1"
}`,
		"surrounding whitespace": " \n\t{\"Authorization\":\"Bearer x\",\"X-A\":\"1\"}\r\n ",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseHeadersFile([]byte(data))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("parseHeadersFile = %v, %v; want %v without an error", got, err, want)
			}
		})
	}
	t.Run("empty string", func(t *testing.T) {
		got, err := parseHeadersFile([]byte(`{"X-A":""}`))
		if err != nil || !reflect.DeepEqual(got, map[string]string{"X-A": ""}) {
			t.Fatalf("parseHeadersFile = %v, %v; want a valid empty string", got, err)
		}
	})
	for _, data := range []string{`{"X-A":`, `{"X-A":null}`, `{"X-A":1}`, `{"X-A":true}`, `{"X-A":[]}`, `{"X-A":{}}`, `{"X-A":"1"} trailing`} {
		t.Run(data, func(t *testing.T) {
			got, err := parseHeadersFile([]byte(data))
			if err == nil || got != nil {
				t.Fatalf("parseHeadersFile(%q) = %v, %v; want a JSON error", data, got, err)
			}
		})
	}
}

func TestDoRequestFromFiles_RejectsNull(t *testing.T) {
	s := newSession("", "")
	// Invalid listen address: a parse error must return before a listener opens.
	s.dbgAddr = "invalid"
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "headers.json")
	if err := os.WriteFile(file, []byte(`{"X-A":null}`), 0600); err != nil {
		t.Fatal(err)
	}

	text, err := s.DoRequestFromFiles(server.URL, "GET", file, "", 5*time.Second)
	server.Close()
	if err == nil || !strings.HasPrefix(err.Error(), "headers_file parse:") || !strings.Contains(err.Error(), "null") || text != "" {
		t.Fatalf("DoRequestFromFiles = %q, %v; want a parse error for null", text, err)
	}
	select {
	case <-received:
		t.Fatal("invalid JSON header values must not send an HTTP request")
	default:
	}
}

func TestParseHeadersFile_Lines(t *testing.T) {
	data := "# comment\r\n\r\nAuthorization: Bearer x\r\nX-Url: http://a\r\nX!#$%&'*+-.^_`|~09AZaz: token\r\n"
	want := map[string]string{
		"Authorization":          "Bearer x",
		"X-Url":                  "http://a",
		"X!#$%&'*+-.^_`|~09AZaz": "token",
	}
	got, err := parseHeadersFile([]byte(data))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parseHeadersFile = %v, %v; want %v without an error", got, err, want)
	}
}

func TestParseHeadersFile_InvalidName(t *testing.T) {
	for _, tc := range []struct {
		name, data, wantErr string
	}{
		{"malformed JSON", `{bad": x`, ""},
		{"empty", " : x", `invalid header name "" on line 1`},
		{"space", "# comment\n\nBad Name: x", `invalid header name "Bad Name" on line 3`},
		{"quote", `Bad"Name: x`, `invalid header name "Bad\"Name" on line 1`},
		{"separator", "Bad(Name): x", `invalid header name "Bad(Name)" on line 1`},
		{"control", "Bad\tName: x", `invalid header name "Bad\tName" on line 1`},
		{"non-ASCII", "X-ä: x", `invalid header name "X-ä" on line 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHeadersFile([]byte(tc.data))
			if err == nil || got != nil {
				t.Fatalf("parseHeadersFile(%q) = %v, %v; want an error", tc.data, got, err)
			}
			if tc.wantErr != "" && err.Error() != tc.wantErr {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func newRequestTestSession(t *testing.T) *session {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen DBGp probe: %v", err)
	}
	s := newSession("", "")
	s.dbgAddr = probe.Addr().String()
	probe.Close()
	t.Cleanup(s.closeLn)
	return s
}

func serveRequestTestEngine(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("engine dial: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(dbgpPacket(xmlProlog + `<init fileuri="file:///index.php"/>`))); err != nil {
		return fmt.Errorf("engine init: %w", err)
	}
	r := bufio.NewReader(conn)
	for {
		command, err := r.ReadString(0)
		if err != nil {
			return fmt.Errorf("engine read: %w", err)
		}
		fields := strings.Fields(strings.TrimSuffix(command, "\x00"))
		if len(fields) < 3 || fields[1] != "-i" {
			return fmt.Errorf("invalid DBGp command: %q", command)
		}
		response := fmt.Sprintf(`<response command="%s" transaction_id="%s" status="stopping"/>`, fields[0], fields[2])
		if _, err := conn.Write([]byte(dbgpPacket(xmlProlog + response))); err != nil {
			return fmt.Errorf("engine response: %w", err)
		}
		if fields[0] == "stop" {
			return nil
		}
	}
}

func TestDoRequest_HostHeader(t *testing.T) {
	for _, tc := range []struct {
		name, headerName, fileData string
	}{
		{"inline", "Host", ""},
		{"inline lowercase", "host", ""},
		{"lines", "", "hOsT: api.example.test\nAuthorization: Bearer x\nX-A: 1\n"},
		{"JSON", "", `{"Host":"api.example.test","Authorization":"Bearer x","X-A":"1"}`},
		{"pretty JSON", "", `{
  "host": "api.example.test",
  "Authorization": "Bearer x",
  "X-A": "1"
}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRequestTestSession(t)
			host := make(chan string, 1)
			engineErr := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host <- r.Host
				for name, want := range map[string]string{"Authorization": "Bearer x", "X-A": "1"} {
					if got := r.Header.Get(name); got != want {
						t.Errorf("received %s = %q, want %q", name, got, want)
					}
				}
				go func() { engineErr <- serveRequestTestEngine(s.dbgAddr) }()
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			var text string
			var err error
			if tc.fileData == "" {
				text, err = s.DoRequest(server.URL, "GET", map[string]string{
					tc.headerName:   "api.example.test",
					"Authorization": "Bearer x",
					"X-A":           "1",
				}, "", 5*time.Second)
			} else {
				file := filepath.Join(t.TempDir(), "headers")
				if err := os.WriteFile(file, []byte(tc.fileData), 0600); err != nil {
					t.Fatal(err)
				}
				text, err = s.DoRequestFromFiles(server.URL, "GET", file, "", 5*time.Second)
			}
			if err != nil || text != "request fired; script ran to completion" {
				t.Fatalf("request = %q, %v; want successful completion", text, err)
			}
			select {
			case got := <-host:
				if got != "api.example.test" {
					t.Errorf("received Host = %q, want api.example.test", got)
				}
			default:
				t.Fatal("the HTTP server received no request")
			}
			select {
			case err := <-engineErr:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the fake DBGp engine did not finish")
			}
		})
	}
}

func TestDoRequest_ClientErrorFailsFast(t *testing.T) {
	t.Run("invalid header", func(t *testing.T) {
		s := newRequestTestSession(t)
		start := time.Now()
		text, err := s.DoRequest("http://127.0.0.1:1/", "GET", map[string]string{"Bad Name": "x"}, "", 10*time.Second)
		elapsed := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "request failed:") || !strings.Contains(err.Error(), "invalid header field name") || text != "" {
			t.Fatalf("DoRequest = %q, %v; want the HTTP client error", text, err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("DoRequest took %s; it must return well before the 10s timeout", elapsed)
		}
		s.mu.Lock()
		listenerClosed := s.ln == nil
		s.mu.Unlock()
		if !listenerClosed {
			t.Fatal("the DBGp listener must close after an HTTP client error")
		}
	})

	t.Run("error after ready", func(t *testing.T) {
		s := newRequestTestSession(t)
		ready := s.ready
		handlerErr := make(chan error, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if err := serveRequestTestEngine(s.dbgAddr); err != nil {
				handlerErr <- err
				return
			}
			select {
			case <-ready:
			case <-time.After(2 * time.Second):
				handlerErr <- fmt.Errorf("the DBGp session did not become ready")
				return
			}
			// Close without HTTP headers after DBGp succeeds to produce a late client error.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				handlerErr <- err
				return
			}
			conn.Close()
			handlerErr <- nil
		}))
		defer server.Close()

		text, err := s.DoRequest(server.URL, "GET", nil, "", 5*time.Second)
		if err != nil || text != "request fired; script ran to completion" {
			t.Fatalf("DoRequest = %q, %v; a late HTTP client error must not override success", text, err)
		}
		select {
		case err := <-handlerErr:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the HTTP handler did not finish")
		}
	})
}

func TestDoAndWaitSlowHandshakeAfterTimeout(t *testing.T) {
	s := newRequestTestSession(t)
	if _, err := s.SetBreakpoint("/index.php", 3); err != nil {
		t.Fatal(err)
	}
	// The engine connects at once but finishes the handshake after the accept
	// timeout. The caller must keep waiting instead of reporting no connection.
	go slowEngine(t, s.dbgAddr, 500*time.Millisecond, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	text, err := s.DoRequest(server.URL, "GET", nil, "", 300*time.Millisecond)
	if err != nil {
		assertNoOrphan(t, s, err)
		return
	}
	if !strings.Contains(text, "paused") || !strings.Contains(s.Status(), "state=started") {
		t.Fatalf("DoRequest = %q, status %q; want an adopted paused session", text, s.Status())
	}
}

func TestDoAndWaitStuckEngine(t *testing.T) {
	oldTimeout, oldGrace := handshakeTimeout, handshakeGrace
	handshakeTimeout, handshakeGrace = 200*time.Millisecond, 0
	t.Cleanup(func() { handshakeTimeout, handshakeGrace = oldTimeout, oldGrace })

	s := newRequestTestSession(t)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	go stuckEngine(t, s.dbgAddr, hold)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	start := time.Now()
	_, err := s.DoRequest(server.URL, "GET", nil, "", 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("DoRequest = nil error, want a handshake timeout")
	}
	if elapsed > 300*time.Millisecond+handshakeTimeout+2*time.Second {
		t.Fatalf("DoRequest took %s; a stuck engine must not block forever", elapsed)
	}
	assertNoOrphan(t, s, err)
}

func TestRequestErrorUnlessReady(t *testing.T) {
	for _, isReady := range []bool{false, true} {
		t.Run(fmt.Sprintf("ready=%t", isReady), func(t *testing.T) {
			s := newSession("", "")
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			s.ln = ln
			t.Cleanup(s.closeLn)
			ready := make(chan struct{})
			if isReady {
				close(ready)
			}
			reqErr := make(chan error, 1)
			reqErr <- io.EOF
			// Both results exist before this error-selected path runs.
			err = s.requestErrorUnlessReady(ready, nil, <-reqErr)
			if isReady {
				if err != nil {
					t.Fatalf("a ready DBGp result must win over the client error: %v", err)
				}
				if s.ln != ln {
					t.Fatal("a late error must not close the listener")
				}
			} else {
				if err == nil || !strings.HasPrefix(err.Error(), "request failed:") || !errors.Is(err, io.EOF) {
					t.Fatalf("error = %v; want the wrapped HTTP client error", err)
				}
				if s.ln != nil {
					t.Fatal("an early client error must close the listener")
				}
			}
		})
	}
}

func servePausedRequestTestEngine(addr string, connReady chan<- net.Conn) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("engine dial: %w", err)
	}
	defer conn.Close()
	connReady <- conn
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(dbgpPacket(xmlProlog + `<init fileuri="file:///index.php"/>`))); err != nil {
		return fmt.Errorf("engine init: %w", err)
	}
	r := bufio.NewReader(conn)
	for _, want := range []string{"feature_set", "feature_set", "feature_set", "breakpoint_set", "run", "stop"} {
		command, err := r.ReadString(0)
		if err != nil {
			return fmt.Errorf("engine read %s: %w", want, err)
		}
		fields := strings.Fields(strings.TrimSuffix(command, "\x00"))
		if len(fields) < 3 || fields[0] != want || fields[1] != "-i" {
			return fmt.Errorf("engine command = %q, want %s", command, want)
		}
		attrs := ` success="1"`
		switch want {
		case "breakpoint_set":
			attrs = ` id="7"`
		case "run":
			attrs = ` status="break" reason="ok"`
		case "stop":
			attrs = ` status="stopped" reason="ok"`
		}
		response := fmt.Sprintf(`<response command="%s" transaction_id="%s"%s/>`, want, fields[2], attrs)
		if _, err := conn.Write([]byte(dbgpPacket(xmlProlog + response))); err != nil {
			return fmt.Errorf("engine response: %w", err)
		}
	}
	return nil
}

type requestTestTransport struct {
	clientErr chan<- error
}

func (tr requestTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	tr.clientErr <- err
	return resp, err
}

func TestDoRequest_PausedSessionSurvivesClientError(t *testing.T) {
	s := newRequestTestSession(t)
	if _, err := s.SetBreakpoint("/index.php", 3); err != nil {
		t.Fatal(err)
	}
	ready := s.ready
	engineErr := make(chan error, 1)
	connReady := make(chan net.Conn, 1)
	httpConnReady := make(chan net.Conn, 1)
	failHTTP := make(chan struct{})
	handlerErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			handlerErr <- err
			return
		}
		defer conn.Close()
		httpConnReady <- conn
		go func() { engineErr <- servePausedRequestTestEngine(s.dbgAddr, connReady) }()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			handlerErr <- fmt.Errorf("the paused DBGp session did not become ready")
			return
		}
		select {
		case <-failHTTP:
		case <-time.After(5 * time.Second):
			handlerErr <- fmt.Errorf("the test did not release the HTTP failure")
			return
		}
		handlerErr <- nil
	}))
	defer server.Close()

	clientErr := make(chan error, 1)
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: requestTestTransport{clientErr: clientErr}}
	defer func() { http.DefaultClient = oldClient }()

	text, err := s.DoRequest(server.URL, "GET", nil, "", 5*time.Second)
	// Close engine and hijacked HTTP connections even if an assertion fails.
	select {
	case conn := <-connReady:
		defer conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("the fake DBGp engine did not connect")
	}
	select {
	case conn := <-httpConnReady:
		defer conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("the HTTP handler did not accept the request")
	}
	if err != nil || text != "request fired; session paused at script start — call run/step to drive" {
		t.Fatalf("DoRequest = %q, %v; want a paused session", text, err)
	}
	if status := s.Status(); !strings.Contains(status, "state=started") {
		t.Fatalf("status = %q; want an active paused session", status)
	}
	close(failHTTP)
	select {
	case err := <-handlerErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the HTTP handler did not fail the request")
	}
	select {
	case err := <-clientErr:
		if err == nil || !errors.Is(err, io.EOF) {
			t.Fatalf("HTTP client error = %v; want EOF after the paused result", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the HTTP client did not report the late error")
	}

	text, err = s.step("run")
	if err != nil || !strings.Contains(text, "state=break") {
		t.Fatalf("run after client error = %q, %v; want a usable paused session", text, err)
	}
	if _, err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-engineErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake DBGp engine did not finish")
	}
}
