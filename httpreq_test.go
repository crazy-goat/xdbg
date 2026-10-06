package main

import (
	"bufio"
	"fmt"
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
	for _, data := range []string{`{"X-A":`, `{"X-A":1}`, `{"X-A":"1"} trailing`} {
		t.Run(data, func(t *testing.T) {
			got, err := parseHeadersFile([]byte(data))
			if err == nil || got != nil {
				t.Fatalf("parseHeadersFile(%q) = %v, %v; want a JSON error", data, got, err)
			}
		})
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
