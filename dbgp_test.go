package main

import (
	"bufio"
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestQuoteArg(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", `""`},
		{"variable", "$x", `"$x"`},
		{"spaces", "$arr['a b']", `"$arr['a b']"`},
		{"double quotes", `$a["k"]`, `"$a[\"k\"]"`},
		{"backslash", `a\b`, `"a\\b"`},
		{"quotes and backslash", `$a["a\b"]`, `"$a[\"a\\b\"]"`},
		{"trailing backslash", `a\`, `"a\\"`},
		{"non-ASCII", "$arr['żółć']", `"$arr['żółć']"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteArg(tc.in); got != tc.want {
				t.Fatalf("quoteArg(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

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

func TestXRespError(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp *xResp
		want string
	}{
		{"nil response", nil, ""},
		{"no engine error", &xResp{}, ""},
		{"engine error", &xResp{Error: &xErr{Code: "300", Message: " \ncan not get property\t "}}, "property_get error 300: can not get property"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.resp.err("property_get")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
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
	for _, tc := range []struct{ name, xml, want string }{
		{
			"real numchildren exceeds the received page",
			`<property name="$arr" type="array" children="1" numchildren="300" page="0" pagesize="100">` +
				`<property name="0" type="int"><![CDATA[1]]></property>` +
				`<property name="1" type="int"><![CDATA[2]]></property>` +
				`</property>`,
			"array {300 children}",
		},
		{
			"empty array",
			`<property name="$arr" type="array" children="0" numchildren="0"></property>`,
			"array {0 children}",
		},
		{
			"object at the depth limit without child elements",
			`<property name="$obj" type="object" children="1" numchildren="5"></property>`,
			"object {5 children}",
		},
		{
			"array without numchildren falls back to the received count",
			`<property name="$arr" type="array">` +
				`<property name="0" type="int"><![CDATA[1]]></property>` +
				`<property name="1" type="int"><![CDATA[2]]></property>` +
				`</property>`,
			"array {2 children}",
		},
		{
			"scalar int",
			`<property name="$i" type="int"><![CDATA[7]]></property>`,
			"7",
		},
		{
			"base64 string",
			`<property name="$s" type="string" encoding="base64"><![CDATA[aGk=]]></property>`,
			"hi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r xResp
			if err := unmarshal(xmlProlog+`<response command="context_get" status="break">`+tc.xml+`</response>`, &r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(r.Props) != 1 {
				t.Fatalf("props = %+v, want exactly one", r.Props)
			}
			if got := summarize(r.Props[0]); got != tc.want {
				t.Fatalf("summarize = %q, want %q", got, tc.want)
			}
		})
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

func TestSummarizeTruncatesLongUTF8Value(t *testing.T) {
	long := "a" + strings.Repeat("é", 350)
	got := summarize(xProp{Type: "string", Value: long})
	if !utf8.ValidString(got) {
		t.Fatalf("summarize = %q, want valid UTF-8", got)
	}
	if n := utf8.RuneCountInString(got); n != 301 {
		t.Fatalf("summarize length = %d, want 301 runes", n)
	}
	if want := "a" + strings.Repeat("é", 299) + "…"; got != want {
		t.Fatalf("summarize = %q, want %q", got, want)
	}
}
