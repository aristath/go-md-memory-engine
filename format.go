package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Parse decodes only JSON frontmatter. Body bytes, including line endings and
// trailing whitespace, are preserved. Unknown envelope fields are errors; put
// arbitrary caller metadata in Extra and arbitrary categories in Taxonomies.
func Parse(data []byte) (Record, error) {
	var r Record
	if !utf8.Valid(data) {
		return r, fmt.Errorf("document is not UTF-8")
	}
	first := bytes.IndexByte(data, '\n')
	if first < 0 || strings.TrimSuffix(string(data[:first]), "\r") != "---" {
		return r, fmt.Errorf("expected --- and JSON frontmatter")
	}
	start := first + 1
	end := -1
	body := 0
	for pos := start; pos < len(data); {
		n := bytes.IndexByte(data[pos:], '\n')
		if n < 0 {
			n = len(data) - pos
		}
		line := data[pos : pos+n]
		if strings.TrimSuffix(string(line), "\r") == "---" {
			end = pos
			body = pos + n
			if body < len(data) {
				body++
			}
			break
		}
		pos += n + 1
	}
	if end < 0 {
		return r, fmt.Errorf("missing frontmatter closing ---")
	}
	d := json.NewDecoder(bytes.NewReader(data[start:end]))
	d.UseNumber()
	d.DisallowUnknownFields()
	if e := d.Decode(&r); e != nil {
		return r, fmt.Errorf("frontmatter: %w", e)
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return r, fmt.Errorf("frontmatter must contain one JSON object")
	}
	if r.Body != "" {
		return r, fmt.Errorf("body belongs after frontmatter")
	}
	r.Body = string(data[body:])
	if e := r.normalize(); e != nil {
		return r, e
	}
	return r, nil
}

func decodeJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

// Encode renders metadata without parsing or reformatting the Markdown body.
func Encode(r Record) ([]byte, error) {
	body := r.Body
	r.Body = ""
	if e := r.normalize(); e != nil {
		return nil, e
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return nil, e
	}
	return append(append(append([]byte("---\n"), b...), []byte("\n---\n")...), []byte(body)...), nil
}
