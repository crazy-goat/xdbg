package main

import (
	"bufio"
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- DBGp packet framing (readPacket) ---------------------------------------

func readerFor(data string) *session {
	s := newSession("", "")
	s.r = bufio.NewReader(strings.NewReader(data))
	return s
}

func TestReadPacketValid(t *testing.T) {
	s := readerFor("5\x00hello\x00")
	got, err := s.readPacket()
	if err != nil || got != "hello" {
		t.Fatalf("readPacket = %q, %v; want %q", got, err, "hello")
	}
}

func TestReadPacketEmptyPayload(t *testing.T) {
	s := readerFor("0\x00\x00")
	got, err := s.readPacket()
	if err != nil || got != "" {
		t.Fatalf("readPacket = %q, %v; want empty payload", got, err)
	}
}

func TestReadPacketSequential(t *testing.T) {
	s := readerFor("1\x00a\x003\x00bcd\x00")
	for _, want := range []string{"a", "bcd"} {
		got, err := s.readPacket()
		if err != nil || got != want {
			t.Fatalf("readPacket = %q, %v; want %q", got, err, want)
		}
	}
}

func TestReadPacketBadLength(t *testing.T) {
	s := readerFor("nope\x00xx\x00")
	got, err := s.readPacket()
	if err == nil || !strings.Contains(err.Error(), "bad length") {
		t.Fatalf("readPacket = %q, %v; want a bad length error", got, err)
	}
}

func TestReadPacketTruncatedBody(t *testing.T) {
	// Announces 10 bytes, delivers 3, then EOF.
	s := readerFor("10\x00abc")
	got, err := s.readPacket()
	if err == nil {
		t.Fatalf("readPacket = %q, %v; want a truncation error", got, err)
	}
}

func TestReadPacketMissingLengthTerminator(t *testing.T) {
	// No NUL after the length: ReadString(0) hits EOF before any delimiter.
	s := readerFor("5hello")
	got, err := s.readPacket()
	if err == nil {
		t.Fatalf("readPacket = %q, %v; want an error for a missing terminator", got, err)
	}
}

// --- XML parsing (unmarshal) ------------------------------------------------

func TestUnmarshalMalformed(t *testing.T) {
	for name, xml := range map[string]string{
		"unclosed tag": `<response command="eval" status="break"`,
		"not xml":      `just text`,
		"empty":        ``,
	} {
		t.Run(name, func(t *testing.T) {
			var r xResp
			if err := unmarshal(xml, &r); err == nil {
				t.Fatalf("unmarshal(%q) = nil error, want an error", xml)
			}
		})
	}
}

func TestUnmarshalResponseFields(t *testing.T) {
	xml := xmlProlog + `<response command="stack_get" status="break" reason="ok" id="7">` +
		`<message filename="file:///d/index.php" lineno="12"/>` +
		`<stack level="0" where="main" filename="file:///d/a.php" lineno="3"/>` +
		`<stack level="1" where="foo" filename="file:///d/b.php" lineno="9"/>` +
		`<breakpoint id="5" type="line" state="enabled" filename="file:///d/a.php" lineno="3"/>` +
		`<error code="12"><message>boom</message></error>` +
		`</response>`

	var r xResp
	if err := unmarshal(xml, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Status != "break" || r.Reason != "ok" || r.Command != "stack_get" || r.ID != "7" {
		t.Fatalf("attributes = %+v", r)
	}
	if r.Message == nil || r.Message.Lineno != 12 || !strings.HasSuffix(r.Message.Filename, "index.php") {
		t.Fatalf("message = %+v", r.Message)
	}
	if len(r.Stacks) != 2 || r.Stacks[1].Where != "foo" || r.Stacks[1].Lineno != 9 {
		t.Fatalf("stacks = %+v", r.Stacks)
	}
	if len(r.Breakpoints) != 1 || r.Breakpoints[0].ID != "5" || r.Breakpoints[0].State != "enabled" {
		t.Fatalf("breakpoints = %+v", r.Breakpoints)
	}
	if r.Error == nil || r.Error.Code != "12" || r.Error.Message != "boom" {
		t.Fatalf("error = %+v", r.Error)
	}
}

func TestUnmarshalDeclaredLatin1Charset(t *testing.T) {
	// The DBGp engine declares iso-8859-1; the identity charset reader must accept it.
	var r xResp
	if err := unmarshal(xmlProlog+`<response status="break"/>`, &r); err != nil {
		t.Fatalf("unmarshal with iso-8859-1 prolog: %v", err)
	}
	if r.Status != "break" {
		t.Fatalf("status = %q, want break", r.Status)
	}
}

func TestUnmarshalNestedProperties(t *testing.T) {
	xml := xmlProlog + `<response command="context_get" status="break">` +
		`<property name="$arr" type="array" children="2">` +
		`<property name="0" type="int"><![CDATA[1]]></property>` +
		`<property name="1" type="int"><![CDATA[2]]></property>` +
		`</property>` +
		`</response>`
	var r xResp
	if err := unmarshal(xml, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(r.Props) != 1 || len(r.Props[0].Children) != 2 {
		t.Fatalf("props = %+v", r.Props)
	}
	if got := decodeVal(r.Props[0].Children[1]); got != "2" {
		t.Fatalf("child value = %q, want 2", got)
	}
}

// --- value decoding (decodeVal) ---------------------------------------------

func TestDecodeValPlain(t *testing.T) {
	if got := decodeVal(xProp{Type: "string", Value: "  hello  "}); got != "hello" {
		t.Fatalf("decodeVal = %q, want %q", got, "hello")
	}
}

func TestDecodeValBase64(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("héllo wörld"))
	p := xProp{Type: "string", Encoding: "base64", Value: "  " + enc + "\n"}
	if got := decodeVal(p); got != "héllo wörld" {
		t.Fatalf("decodeVal = %q, want the decoded UTF-8 text", got)
	}
}

func TestDecodeValInvalidBase64FallsBack(t *testing.T) {
	p := xProp{Type: "string", Encoding: "base64", Value: "not base64!"}
	if got := decodeVal(p); got != "not base64!" {
		t.Fatalf("decodeVal = %q, want the raw value on a decode failure", got)
	}
}

// --- one-line rendering (summarize) -----------------------------------------

func TestSummarizeChildren(t *testing.T) {
	p := xProp{Type: "array", Children: []xProp{{}, {}, {}}}
	if got := summarize(p); got != "array {3 children}" {
		t.Fatalf("summarize = %q", got)
	}
}

func TestSummarizeShortValue(t *testing.T) {
	if got := summarize(xProp{Type: "int", Value: "42"}); got != "42" {
		t.Fatalf("summarize = %q, want 42", got)
	}
}

func TestSummarizeTruncatesLongValue(t *testing.T) {
	long := strings.Repeat("x", 400)
	got := summarize(xProp{Type: "string", Value: long})
	if len([]rune(got)) != 301 || !strings.HasSuffix(got, "…") {
		t.Fatalf("summarize length = %d, want 301 ending in an ellipsis", len([]rune(got)))
	}
}

func TestSummarizeTruncatesOnRuneBoundary(t *testing.T) {
	// A one-byte prefix followed by two-byte runes puts the 300-byte offset in
	// the middle of a rune, so byte slicing would produce invalid UTF-8.
	long := "a" + strings.Repeat("é", 350)
	got := summarize(xProp{Type: "string", Value: long})
	if !utf8.ValidString(got) {
		t.Fatalf("summarize = %q, want valid UTF-8", got)
	}
	if len([]rune(got)) != 301 || !strings.HasSuffix(got, "…") {
		t.Fatalf("summarize length = %d runes, want 301 ending in an ellipsis", len([]rune(got)))
	}
}
