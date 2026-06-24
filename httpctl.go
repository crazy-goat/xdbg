package main

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// serveHTTP exposes the same session over a curl-driven control API — handy for
// manual debugging without an MCP client.
func serveHTTP(s *session, addr string) {
	reply := func(w http.ResponseWriter, out string, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		fmt.Fprintln(w, out)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, s.Status()) })
	mux.HandleFunc("/bp", func(w http.ResponseWriter, r *http.Request) {
		line, _ := strconv.Atoi(r.URL.Query().Get("line"))
		out, err := s.SetBreakpoint(r.URL.Query().Get("file"), line)
		reply(w, out, err)
	})
	mux.HandleFunc("/bp/list", func(w http.ResponseWriter, _ *http.Request) { out, err := s.BreakpointList(); reply(w, out, err) })
	mux.HandleFunc("/bp/clear", func(w http.ResponseWriter, _ *http.Request) { out, err := s.BreakpointClearAll(); reply(w, out, err) })
	mux.HandleFunc("/run", func(w http.ResponseWriter, _ *http.Request) { out, err := s.step("run"); reply(w, out, err) })
	mux.HandleFunc("/into", func(w http.ResponseWriter, _ *http.Request) { out, err := s.step("step_into"); reply(w, out, err) })
	mux.HandleFunc("/over", func(w http.ResponseWriter, _ *http.Request) { out, err := s.step("step_over"); reply(w, out, err) })
	mux.HandleFunc("/out", func(w http.ResponseWriter, _ *http.Request) { out, err := s.step("step_out"); reply(w, out, err) })
	mux.HandleFunc("/stack", func(w http.ResponseWriter, _ *http.Request) { out, err := s.Stack(); reply(w, out, err) })
	mux.HandleFunc("/ctx", func(w http.ResponseWriter, r *http.Request) {
		d, _ := strconv.Atoi(r.URL.Query().Get("depth"))
		out, err := s.Context(d)
		reply(w, out, err)
	})
	mux.HandleFunc("/eval", func(w http.ResponseWriter, r *http.Request) { out, err := s.Eval(r.URL.Query().Get("expr")); reply(w, out, err) })
	mux.HandleFunc("/request", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.DoRequest(r.URL.Query().Get("url"), r.URL.Query().Get("method"), nil, r.URL.Query().Get("body"), 15*time.Second)
		reply(w, out, err)
	})
	mux.HandleFunc("/detach", func(w http.ResponseWriter, _ *http.Request) { out, err := s.Detach(); reply(w, out, err) })
	mux.HandleFunc("/cmd", func(w http.ResponseWriter, r *http.Request) {
		t, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
		if t == 0 {
			t = 30000
		}
		out, err := s.RunCommand(r.URL.Query().Get("command"), time.Duration(t)*time.Millisecond)
		reply(w, out, err)
	})
	mux.HandleFunc("/raw", func(w http.ResponseWriter, r *http.Request) { out, err := s.Raw(r.URL.Query().Get("cmd")); reply(w, out, err) })

	log.Printf("HTTP control API on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("http control: %v", err)
	}
}
