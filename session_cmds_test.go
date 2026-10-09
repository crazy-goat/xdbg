package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStackFormatsFrames(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="stack_get" transaction_id="1">` +
		`<stack where="{main}" level="0" type="file" filename="file:///d/index.php" lineno="12"/>` +
		`<stack where="App\Foo-&gt;bar" level="1" type="file" filename="file:///usr/share/php/x.php" lineno="3"/>` +
		`</response>`)

	out, err := s.Stack()
	if command := <-response; command != "stack_get -i 1" {
		t.Fatalf("engine got %q", command)
	}
	want := "#0 {main}  /l/index.php:12\n#1 App\\Foo->bar  /usr/share/php/x.php:3\n"
	if err != nil || out != want {
		t.Fatalf("Stack() = %q, %v; want %q", out, err, want)
	}
}

func TestStackEmpty(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="stack_get" transaction_id="1"/>`)

	out, err := s.Stack()
	<-response
	if err != nil || out != "(no stack — not paused?)" {
		t.Fatalf("Stack() = %q, %v", out, err)
	}
}

func TestContextFormatsVariables(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="context_get" transaction_id="1">` +
		`<property name="$n" type="int"><![CDATA[7]]></property>` +
		`<property name="$s" type="string" encoding="base64"><![CDATA[aGVsbG8=]]></property>` +
		`<property name="$u" type="uninitialized"></property>` +
		`</response>`)

	out, err := s.Context(1)
	if command := <-response; command != "context_get -i 1 -d 1" {
		t.Fatalf("engine got %q", command)
	}
	want := "$n (int) = 7\n$s (string) = hello\n$u (uninitialized) = \n"
	if err != nil || out != want {
		t.Fatalf("Context() = %q, %v; want %q", out, err, want)
	}
}

func TestContextEmpty(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="context_get" transaction_id="1"/>`)

	out, err := s.Context(0)
	<-response
	if err != nil || out != "(no variables)" {
		t.Fatalf("Context() = %q, %v", out, err)
	}
}

func TestEvalSendsBase64AndDecodesResult(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="eval" transaction_id="1">` +
		`<property type="string" encoding="base64"><![CDATA[YWJj]]></property></response>`)

	out, err := s.Eval(`strtoupper("x")`)
	if command := <-response; command != `eval -i 1 -- c3RydG91cHBlcigieCIp` {
		t.Fatalf("engine got %q", command)
	}
	if err != nil || out != "abc" {
		t.Fatalf("Eval() = %q, %v", out, err)
	}
}

func TestEvalNoResult(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="eval" transaction_id="1"/>`)

	out, err := s.Eval("1")
	if command := <-response; command != "eval -i 1 -- MQ==" {
		t.Fatalf("engine got %q", command)
	}
	if err != nil || out != "(no result)" {
		t.Fatalf("Eval() = %q, %v", out, err)
	}
}

func TestPropertyGetValue(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="property_get" transaction_id="1">` +
		`<property name="$x" type="int"><![CDATA[42]]></property></response>`)

	out, err := s.PropertyGet("$x", 2)
	if command := <-response; command != `property_get -i 1 -d 2 -n "$x"` {
		t.Fatalf("engine got %q", command)
	}
	if err != nil || out != "42" {
		t.Fatalf("PropertyGet() = %q, %v", out, err)
	}
}

func TestPropertyGetNotFound(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="property_get" transaction_id="1"/>`)

	out, err := s.PropertyGet("$nope", 0)
	if command := <-response; command != `property_get -i 1 -d 0 -n "$nope"` {
		t.Fatalf("engine got %q", command)
	}
	if err != nil || out != "(not found)" {
		t.Fatalf("PropertyGet() = %q, %v", out, err)
	}
}

func TestPropertySetSendsBase64(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="property_set" transaction_id="1" success="1"/>`)

	out, err := s.PropertySet("$x", `"hi"`)
	if command := <-response; command != `property_set -i 1 -n "$x" -- ImhpIg==` {
		t.Fatalf("engine got %q", command)
	}
	if err != nil || out != `$x = "hi"` {
		t.Fatalf("PropertySet() = %q, %v", out, err)
	}
}

func TestStepReportsStateReasonLocation(t *testing.T) {
	s, eng := newActiveSession(t, "started")
	response := eng.answer(xmlProlog + `<response xmlns:xdebug="https://xdebug.org/dbgp/xdebug" command="step_into" transaction_id="1" status="break" reason="ok">` +
		`<xdebug:message filename="file:///d/src/A.php" lineno="9"/></response>`)

	out, err := s.step("step_into")
	if command := <-response; command != "step_into -i 1" {
		t.Fatalf("engine got %q", command)
	}
	want := "state=break reason=ok\nlocation=/l/src/A.php:9"
	if err != nil || out != want {
		t.Fatalf("step() = %q, %v; want %q", out, err, want)
	}
	if status := s.Status(); status != "state=break\nlocation=/l/src/A.php:9\nbreakpoints=0" {
		t.Fatalf("Status() = %q", status)
	}
}

func TestSetBreakpointValidation(t *testing.T) {
	s := newSession("/l", "/d")
	for _, input := range []struct {
		file string
		line int
	}{{"", 3}, {"a.php", 0}, {"a.php", -1}} {
		if _, err := s.SetBreakpoint(input.file, input.line); err == nil || !strings.Contains(err.Error(), "file and line>0 required") {
			t.Errorf("SetBreakpoint(%q, %d) error = %v", input.file, input.line, err)
		}
	}
	if len(s.pending) != 0 {
		t.Fatalf("invalid input must not queue a breakpoint: %+v", s.pending)
	}
}

func TestSetBreakpointQueuedWithoutSession(t *testing.T) {
	s := newSession("/l", "/d")
	out, err := s.SetBreakpoint("/l/src/A.php", 5)
	if err != nil || out != "breakpoint queued q1 /d/src/A.php:5 (applied on next session)" {
		t.Fatalf("SetBreakpoint() = %q, %v", out, err)
	}
	if len(s.pending) != 1 || s.pending[0] != (bp{file: "/d/src/A.php", line: 5, qid: "q1"}) {
		t.Fatalf("pending = %+v", s.pending)
	}
}

func TestSetBreakpointAppliedInSession(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="breakpoint_set" transaction_id="1" state="enabled" id="12"/>`)

	out, err := s.SetBreakpoint("src/A.php", 5)
	if command := <-response; command != `breakpoint_set -i 1 -t line -f "file:///d/src/A.php" -n 5` {
		t.Fatalf("engine got %q", command)
	}
	if err != nil || out != "breakpoint set id=12 /d/src/A.php:5" {
		t.Fatalf("SetBreakpoint() = %q, %v", out, err)
	}
	if len(s.pending) != 1 || s.pending[0].id != "12" {
		t.Fatalf("pending = %+v", s.pending)
	}
}

func TestBreakpointClearAllWithoutSession(t *testing.T) {
	s := newSession("/l", "/d")
	s.pending = []bp{{file: "/d/a.php", line: 1}, {file: "/d/b.php", line: 2}}

	out, err := s.BreakpointClearAll()
	if err != nil || out != "cleared 2 breakpoint(s)" || s.pending != nil {
		t.Fatalf("BreakpointClearAll() = %q, %v, pending %+v", out, err, s.pending)
	}
}

func TestBreakpointClearAllRemovesApplied(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	s.pending = []bp{
		{file: "/d/a.php", line: 1, id: "3"},
		{file: "/d/b.php", line: 2},
		{file: "/d/c.php", line: 4, id: "9"},
	}
	commands := make(chan string, 2)
	go func() {
		for i := 0; i < 2; i++ {
			command, tx := eng.readCmd()
			name, args, ok := strings.Cut(command, " ")
			if !ok {
				eng.t.Errorf("engine got malformed command %q", command)
				return
			}
			commands <- fmt.Sprintf("%s -i %s %s", name, tx, args)
			eng.send(xmlProlog + fmt.Sprintf(`<response command="breakpoint_remove" transaction_id="%s"/>`, tx))
		}
	}()

	out, err := s.BreakpointClearAll()
	if err != nil || out != "cleared 3 breakpoint(s)" {
		t.Fatalf("BreakpointClearAll() = %q, %v", out, err)
	}
	for _, want := range []string{"breakpoint_remove -i 1 -d 3", "breakpoint_remove -i 2 -d 9"} {
		select {
		case command := <-commands:
			if command != want {
				t.Errorf("engine got %q, want %q", command, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("engine did not get %q", want)
		}
	}
}

func TestDetachSendsDetachAndDropsConnection(t *testing.T) {
	s, eng := newActiveSession(t, "break")
	response := eng.answer(xmlProlog + `<response command="detach" transaction_id="1" status="stopping"/>`)

	out, err := s.Detach()
	if command := <-response; command != "detach -i 1" {
		t.Fatalf("engine got %q", command)
	}
	if err != nil || out != "detached" || s.conn != nil || s.state != "no session" {
		t.Fatalf("Detach() = %q, %v; conn nil %v, state %q", out, err, s.conn == nil, s.state)
	}
}

func TestStopWithoutSession(t *testing.T) {
	s := newSession("/l", "/d")
	out, err := s.Stop()
	if err != nil || out != "stopped" || s.state != "no session" {
		t.Fatalf("Stop() = %q, %v, state %q", out, err, s.state)
	}
}
