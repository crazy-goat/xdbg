package main

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// quoteArg quotes a DBGp argument. Callers must reject NUL bytes.
func quoteArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// DBGp response structs (we only model the fields we use).

type xResp struct {
	XMLName       xml.Name // root element: response, stream, notify, ...
	TransactionID string   `xml:"transaction_id,attr"`
	Status        string   `xml:"status,attr"`
	Reason        string   `xml:"reason,attr"`
	Command       string   `xml:"command,attr"`
	Success       string   `xml:"success,attr"`
	ID            string   `xml:"id,attr"`    // breakpoint_set returns the new id here
	Message       *xMsg    `xml:"message"`    // xdebug:message on break/step
	Stacks        []xStk   `xml:"stack"`      // stack_get
	Props         []xProp  `xml:"property"`   // context_get / eval / property_get
	Breakpoints   []xBkpt  `xml:"breakpoint"` // breakpoint_list
	Error         *xErr    `xml:"error"`
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
	Name        string  `xml:"name,attr"`
	Type        string  `xml:"type,attr"`
	Encoding    string  `xml:"encoding,attr"`
	NumChildren int     `xml:"numchildren,attr"` // real number of children (all pages)
	HasChildren string  `xml:"children,attr"`    // "1" or "0"; empty for scalars
	Value       string  `xml:",chardata"`
	Children    []xProp `xml:"property"`
}

// UnmarshalXMLAttr decodes the numchildren attribute leniently: surrounding
// spaces are trimmed as encoding/xml does for int fields, and a malformed,
// negative or out-of-range value falls back to 0 instead of failing the whole
// response, and
// summarize then reports len(p.Children). Real Xdebug always emits a valid
// integer, but other engines occasionally do not (issue #100). UnmarshalXML
// calls it for every attribute of a property element.
func (p *xProp) UnmarshalXMLAttr(attr xml.Attr) error {
	if attr.Name.Local != "numchildren" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(attr.Value))
	if err != nil || n < 0 {
		return nil
	}
	p.NumChildren = n
	return nil
}

// UnmarshalXML implements xml.Unmarshaler, so encoding/xml calls it for every
// property element, nested ones included (Children is []xProp). It decodes the
// other fields through a local struct without numchildren, so the decoder never
// parses that attribute as an int, and it takes NumChildren from
// UnmarshalXMLAttr. A plain DecodeElement into xProp would call UnmarshalXML
// again and recurse.
func (p *xProp) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for _, attr := range start.Attr {
		if err := p.UnmarshalXMLAttr(attr); err != nil {
			return err
		}
	}
	var field struct {
		Name        string  `xml:"name,attr"`
		Type        string  `xml:"type,attr"`
		Encoding    string  `xml:"encoding,attr"`
		HasChildren string  `xml:"children,attr"`
		Value       string  `xml:",chardata"`
		Children    []xProp `xml:"property"`
	}
	if err := d.DecodeElement(&field, &start); err != nil {
		return err
	}
	p.Name = field.Name
	p.Type = field.Type
	p.Encoding = field.Encoding
	p.HasChildren = field.HasChildren
	p.Value = field.Value
	p.Children = field.Children
	return nil
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
	if p.Type == "array" || p.Type == "object" || p.HasChildren == "1" || len(p.Children) > 0 {
		n := max(p.NumChildren, len(p.Children)) // numchildren is missing in some engines
		return p.Type + " {" + strconv.Itoa(n) + " children}"
	}
	v := decodeVal(p)
	if r := []rune(v); len(r) > 300 {
		v = string(r[:300]) + "…"
	}
	return v
}
