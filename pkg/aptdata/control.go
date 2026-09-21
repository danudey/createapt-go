// Package aptdata defines the data model for Debian (apt) repository metadata
// and (de)serializes the deb822 control documents a repository is made of:
// the binary package index (Packages), the source package index (Sources), and
// the per-suite Release file that signs them.
//
// It targets what apt actually consumes — there is no support for Translation
// or Contents files, diff indexes (pdiff), or the legacy trivial ("flat")
// layout.
package aptdata

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// Field is one control-file field: a name and its (possibly multi-line) value.
// Continuation lines are stored joined with "\n" and without their leading
// space, so a value round-trips through Paragraph.Render unchanged.
type Field struct {
	Name  string
	Value string
}

// Paragraph is one deb822 stanza: an ordered list of fields. Order is preserved
// because a Release file (and the Packages entries it covers) must render
// byte-identically to what was signed, and because apt clients and humans alike
// expect the conventional field order.
type Paragraph struct {
	Fields []Field

	// index maps the lowercased field name to its position in Fields. Control
	// field names are case-insensitive, so lookups are too.
	index map[string]int
}

// NewParagraph returns an empty paragraph.
func NewParagraph() *Paragraph {
	return &Paragraph{index: map[string]int{}}
}

// Get returns the value of the named field, or "" if it is absent. The name is
// matched case-insensitively.
func (p *Paragraph) Get(name string) string {
	if p == nil {
		return ""
	}
	if i, ok := p.index[strings.ToLower(name)]; ok {
		return p.Fields[i].Value
	}
	return ""
}

// Has reports whether the named field is present (even with an empty value).
func (p *Paragraph) Has(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.index[strings.ToLower(name)]
	return ok
}

// Set adds the field or replaces an existing one in place, keeping its
// position. Setting a field to "" still records it; use Delete to remove one.
func (p *Paragraph) Set(name, value string) {
	if p.index == nil {
		p.index = map[string]int{}
	}
	key := strings.ToLower(name)
	if i, ok := p.index[key]; ok {
		p.Fields[i].Value = value
		return
	}
	p.index[key] = len(p.Fields)
	p.Fields = append(p.Fields, Field{Name: name, Value: value})
}

// Delete removes the named field, reporting whether it was present.
func (p *Paragraph) Delete(name string) bool {
	key := strings.ToLower(name)
	i, ok := p.index[key]
	if !ok {
		return false
	}
	p.Fields = append(p.Fields[:i], p.Fields[i+1:]...)
	delete(p.index, key)
	for k, pos := range p.index {
		if pos > i {
			p.index[k] = pos - 1
		}
	}
	return true
}

// Clone returns a deep copy.
func (p *Paragraph) Clone() *Paragraph {
	out := &Paragraph{
		Fields: make([]Field, len(p.Fields)),
		index:  make(map[string]int, len(p.index)),
	}
	copy(out.Fields, p.Fields)
	for k, v := range p.index {
		out.index[k] = v
	}
	return out
}

// Render writes the paragraph in deb822 form, terminated by a newline. It does
// not write the blank line that separates paragraphs; WriteParagraphs does.
//
// A value's embedded newlines become continuation lines prefixed with a single
// space, and an empty continuation line becomes " ." — the deb822 encoding for
// a blank line inside a field.
func (p *Paragraph) Render() []byte {
	var buf bytes.Buffer
	for _, f := range p.Fields {
		buf.WriteString(f.Name)
		buf.WriteString(":")
		lines := strings.Split(f.Value, "\n")
		// The first line sits on the field line itself (with a separating
		// space), unless the value starts empty — a folded field such as the
		// Release checksum blocks, whose entries all live on continuation lines.
		if lines[0] != "" {
			buf.WriteString(" ")
			buf.WriteString(lines[0])
		}
		buf.WriteString("\n")
		for _, line := range lines[1:] {
			if strings.TrimSpace(line) == "" {
				buf.WriteString(" .\n")
				continue
			}
			buf.WriteString(" ")
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	return buf.Bytes()
}

// Empty reports whether the paragraph has no fields.
func (p *Paragraph) Empty() bool { return p == nil || len(p.Fields) == 0 }

// ParseParagraphs reads every stanza in a deb822 document. Blank lines separate
// stanzas; comment lines (starting with '#') are ignored, as apt does for
// sources.list-style files.
func ParseParagraphs(data []byte) ([]*Paragraph, error) {
	var out []*Paragraph
	sc := bufio.NewScanner(bytes.NewReader(data))
	// Package descriptions and long dependency lists comfortably exceed
	// bufio's default 64KiB line limit on pathological input.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	cur := NewParagraph()
	var fieldName string
	var value strings.Builder
	lineNo := 0

	flushField := func() {
		if fieldName != "" {
			cur.Set(fieldName, value.String())
			fieldName = ""
			value.Reset()
		}
	}
	flushParagraph := func() {
		flushField()
		if !cur.Empty() {
			out = append(out, cur)
		}
		cur = NewParagraph()
	}

	for sc.Scan() {
		line := sc.Text()
		lineNo++
		// Strip a trailing CR so CRLF documents parse.
		line = strings.TrimSuffix(line, "\r")

		switch {
		case line == "":
			flushParagraph()
		case strings.HasPrefix(line, "#"):
			// Comment; ignored.
		case line[0] == ' ' || line[0] == '\t':
			if fieldName == "" {
				return nil, fmt.Errorf("line %d: continuation line with no preceding field", lineNo)
			}
			body := strings.TrimRight(line[1:], " \t")
			if strings.TrimSpace(body) == "." {
				body = ""
			}
			value.WriteString("\n")
			value.WriteString(body)
		default:
			name, rest, ok := strings.Cut(line, ":")
			if !ok {
				return nil, fmt.Errorf("line %d: malformed field %q (no colon)", lineNo, line)
			}
			if name == "" {
				return nil, fmt.Errorf("line %d: empty field name", lineNo)
			}
			flushField()
			fieldName = name
			value.WriteString(strings.TrimSpace(rest))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flushParagraph()
	return out, nil
}

// WriteParagraphs renders stanzas separated by a blank line, which is the form
// of a Packages or Sources index.
func WriteParagraphs(ps []*Paragraph) []byte {
	var buf bytes.Buffer
	for i, p := range ps {
		if i > 0 {
			buf.WriteString("\n")
		}
		buf.Write(p.Render())
	}
	return buf.Bytes()
}
