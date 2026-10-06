package main

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// DBGp response structs (we only model the fields we use).

type xResp struct {
	Status      string  `xml:"status,attr"`
	Reason      string  `xml:"reason,attr"`
	Command     string  `xml:"command,attr"`
	Success     string  `xml:"success,attr"`
	ID          string  `xml:"id,attr"`    // breakpoint_set returns the new id here
	Message     *xMsg   `xml:"message"`    // xdebug:message on break/step
	Stacks      []xStk  `xml:"stack"`      // stack_get
	Props       []xProp `xml:"property"`   // context_get / eval / property_get
	Breakpoints []xBkpt `xml:"breakpoint"` // breakpoint_list
	Error       *xErr   `xml:"error"`
}

func (r *xResp) err(cmd string) error {
	if r == nil || r.Error == nil {
		return nil
	}
	return fmt.Errorf("%s error %s: %s", cmd, r.Error.Code, strings.TrimSpace(r.Error.Message))
}

type xMsg struct {
	Filename string `xml:"filename,attr"`
	Lineno   int    `xml:"lineno,attr"`
}

type xStk struct {
	Where    string `xml:"where,attr"`
	Level    int    `xml:"level,attr"`
	Filename string `xml:"filename,attr"`
	Lineno   int    `xml:"lineno,attr"`
}

type xProp struct {
	Name     string  `xml:"name,attr"`
	Type     string  `xml:"type,attr"`
	Encoding string  `xml:"encoding,attr"`
	Value    string  `xml:",chardata"`
	Children []xProp `xml:"property"`
}

type xBkpt struct {
	ID       string `xml:"id,attr"`
	Type     string `xml:"type,attr"`
	Filename string `xml:"filename,attr"`
	Lineno   int    `xml:"lineno,attr"`
	State    string `xml:"state,attr"`
}

type xErr struct {
	Code    string `xml:"code,attr"`
	Message string `xml:"message"`
}

// unmarshal parses DBGp XML, tolerating its declared iso-8859-1 charset
// (content is ASCII/base64, so an identity charset reader is safe).
func unmarshal(data string, v any) error {
	dec := xml.NewDecoder(strings.NewReader(data))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return dec.Decode(v)
}

// decodeVal returns a property's value, base64-decoded when needed.
func decodeVal(p xProp) string {
	v := p.Value
	if p.Encoding == "base64" {
		if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.Value)); err == nil {
			v = string(dec)
		}
	}
	return strings.TrimSpace(v)
}

// summarize renders a property as one readable line.
func summarize(p xProp) string {
	if len(p.Children) > 0 {
		return p.Type + " {" + strconv.Itoa(len(p.Children)) + " children}"
	}
	v := decodeVal(p)
	if len(v) > 300 {
		v = v[:300] + "…"
	}
	return v
}
