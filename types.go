// Package memory stores memories in authoritative Markdown files and provides
// indexed, deterministic queries and optional semantic recall. Collections are
// explicit, isolated directories. See README.md for the persistence contract.
package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("memory not found")
	ErrConflict = errors.New("memory changed: read it again before writing")
	idPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

// CollectionError preserves the underlying error and reports invalid or
// ambiguous files that may explain why a requested record is unavailable.
type CollectionError struct {
	Err         error
	Diagnostics []Diagnostic
}

func (e *CollectionError) Error() string { return e.Err.Error() }
func (e *CollectionError) Unwrap() error { return e.Err }

// Record is both the write input and the authoritative document metadata.
// Body is encoded after frontmatter, not within it. Kind is optional: entities
// are simply records whose kind identifies their user-defined entity type.
type Record struct {
	Format     int                `json:"format"`
	ID         string             `json:"id"`
	Title      string             `json:"title"`
	Kind       string             `json:"kind,omitempty"`
	CreatedAt  string             `json:"created_at"`
	UpdatedAt  string             `json:"updated_at"`
	Taxonomies map[string][]Value `json:"taxonomies,omitempty"`
	Links      []Link             `json:"links,omitempty"`
	Sources    []map[string]any   `json:"sources,omitempty"`
	Extra      map[string]any     `json:"extra,omitempty"`
	Body       string             `json:"body,omitempty"`
}

// Value is explicitly typed; names and meaning remain caller-defined.
type Value struct {
	Type    string    `json:"type"`
	Text    string    `json:"text,omitempty"`
	Number  *float64  `json:"number,omitempty"`
	Boolean *bool     `json:"boolean,omitempty"`
	Ref     string    `json:"ref,omitempty"`
	Time    *Temporal `json:"time,omitempty"`
	Items   []Value   `json:"items,omitempty"`
}

func Text(s string) Value       { return Value{Type: "text", Text: s} }
func Number(n float64) Value    { return Value{Type: "number", Number: &n} }
func Boolean(b bool) Value      { return Value{Type: "boolean", Boolean: &b} }
func Reference(id string) Value { return Value{Type: "reference", Ref: id} }

type Link struct {
	Relation string `json:"relation"`
	Target   string `json:"target"`
}
type Diagnostic struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Document struct {
	Record      Record       `json:"record"`
	Path        string       `json:"path"`
	Hash        string       `json:"hash"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}
type Receipt struct {
	ID          string       `json:"id"`
	Path        string       `json:"path"`
	Hash        string       `json:"hash,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}
type Vocabulary struct {
	Names       []string     `json:"names"`
	Revision    string       `json:"revision"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}
type Page struct {
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
}

func (p Page) bounds(n int) (int, int, error) {
	if p.Offset < 0 || p.Limit < 0 {
		return 0, 0, errors.New("offset and limit must be nonnegative")
	}
	limit := p.Limit
	if limit == 0 {
		limit = 20
	}
	start := min(p.Offset, n)
	end := start + min(limit, n-start)
	return start, end, nil
}

type Entry struct {
	Value Value `json:"value"`
	Count int   `json:"count"`
}
type Entries struct {
	Items       []Entry      `json:"items"`
	Total       int          `json:"total"`
	NextOffset  *int         `json:"next_offset,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}
type SyncReport struct {
	Files       int          `json:"files"`
	Parsed      int          `json:"parsed"`
	Removed     int          `json:"removed"`
	Indexed     int          `json:"indexed"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}
type Stats struct {
	Records     int          `json:"records"`
	Files       int          `json:"files"`
	Taxonomies  int          `json:"taxonomies"`
	Links       int          `json:"links"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func newID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}
func valueKey(v Value) string { b, _ := json.Marshal(normalizeValue(v)); return string(b) }
func validName(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n")
}
func (v Value) validate(depth int) error {
	if depth > 16 {
		return errors.New("value nesting exceeds 16")
	}
	fields := 0
	if v.Text != "" {
		fields++
	}
	if v.Number != nil {
		fields++
	}
	if v.Boolean != nil {
		fields++
	}
	if v.Ref != "" {
		fields++
	}
	if v.Time != nil {
		fields++
	}
	if v.Items != nil {
		fields++
	}
	if fields > 1 {
		return errors.New("value contains multiple payload types")
	}
	switch v.Type {
	case "text":
		if fields > 0 && v.Text == "" {
			return errors.New("text payload required")
		}
	case "number":
		if v.Number == nil || math.IsNaN(*v.Number) || math.IsInf(*v.Number, 0) {
			return errors.New("finite number required")
		}
	case "boolean":
		if v.Boolean == nil {
			return errors.New("boolean payload required")
		}
	case "reference":
		if !idPattern.MatchString(v.Ref) {
			return errors.New("valid reference ID required")
		}
	case "time":
		if v.Time == nil {
			return errors.New("time payload required")
		}
		if _, _, e := v.Time.bounds(); e != nil {
			return e
		}
	case "list":
		if v.Items == nil && fields != 0 {
			return errors.New("list cannot have another payload type")
		}
		for _, x := range v.Items {
			if e := x.validate(depth + 1); e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("unknown value type %q", v.Type)
	}
	return nil
}
func normalizeValue(v Value) Value {
	if v.Type == "text" {
		v.Text = strings.TrimSpace(v.Text)
	}
	if v.Items != nil {
		v.Items = append([]Value{}, v.Items...)
	}
	for i := range v.Items {
		v.Items[i] = normalizeValue(v.Items[i])
	}
	return v
}
func (r *Record) normalize() error {
	normalized := make(map[string][]Value, len(r.Taxonomies))
	for name, values := range r.Taxonomies {
		n := strings.TrimSpace(name)
		if !validName(n) {
			return fmt.Errorf("invalid taxonomy name %q", name)
		}
		if _, exists := normalized[n]; exists {
			return fmt.Errorf("taxonomy names collide after trimming: %q", n)
		}
		seen := map[string]bool{}
		normalized[n] = []Value{}
		for _, v := range values {
			if e := v.validate(0); e != nil {
				return fmt.Errorf("taxonomy %s: %w", n, e)
			}
			v = normalizeValue(v)
			k := valueKey(v)
			if !seen[k] {
				normalized[n] = append(normalized[n], v)
				seen[k] = true
			}
		}
	}
	r.Taxonomies = normalized
	for i := range r.Links {
		r.Links[i].Relation = strings.TrimSpace(r.Links[i].Relation)
	}
	return r.validate()
}
func (r Record) validate() error {
	if r.Format != 1 {
		return errors.New("format must be 1")
	}
	if !idPattern.MatchString(r.ID) {
		return errors.New("ID must be 1..128 ASCII letters, digits, dot, underscore or hyphen, starting with a letter or digit")
	}
	if !validName(r.Title) {
		return errors.New("title must be nonempty, single-line and at most 256 bytes")
	}
	if r.Kind != "" && !validName(r.Kind) {
		return errors.New("kind must be single-line and at most 256 bytes")
	}
	for _, stamp := range []string{r.CreatedAt, r.UpdatedAt} {
		if _, e := time.Parse(time.RFC3339Nano, stamp); e != nil {
			return fmt.Errorf("invalid created_at/updated_at: %w", e)
		}
	}
	for name, values := range r.Taxonomies {
		if !validName(name) {
			return fmt.Errorf("invalid taxonomy name %q", name)
		}
		for _, v := range values {
			if e := v.validate(0); e != nil {
				return fmt.Errorf("taxonomy %s: %w", name, e)
			}
		}
	}
	for _, l := range r.Links {
		if !validName(l.Relation) || !idPattern.MatchString(l.Target) {
			return errors.New("links need a relation and valid target ID")
		}
	}
	return nil
}
func leaves(values []Value) []Value {
	var out []Value
	var visit func(Value)
	visit = func(v Value) {
		if v.Type == "list" {
			for _, x := range v.Items {
				visit(x)
			}
		} else {
			out = append(out, v)
		}
	}
	for _, v := range values {
		visit(v)
	}
	return out
}
