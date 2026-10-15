package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatTypesAndPrecision(t *testing.T) {
	r := Record{Format: 1, ID: "a", Title: "A", CreatedAt: "2020-01-01T00:00:00Z", UpdatedAt: "2020-01-01T00:00:00Z", Taxonomies: map[string][]Value{"empty-list": {{Type: "list"}}, "list": {{Type: "list", Items: []Value{Text("x"), Number(1), Boolean(false), Reference("p")}}}}, Sources: []map[string]any{{"message_id": json.Number("9223372036854775807")}}, Extra: map[string]any{"custom": true}, Body: "\r\n# Ελληνικά\n```\n---\n```\n "}
	b, e := Encode(r)
	if e != nil {
		t.Fatal(e)
	}
	parsed, e := Parse(b)
	if e != nil {
		t.Fatal(e)
	}
	if parsed.Body != r.Body || fmt.Sprint(parsed.Sources[0]["message_id"]) != "9223372036854775807" {
		t.Fatal(parsed)
	}
	s := openTest(t)
	mustCreate(t, s, r)
	for i := 0; i < 2; i++ {
		d := mustGet(t, s, "a")
		if fmt.Sprint(d.Record.Sources[0]["message_id"]) != "9223372036854775807" {
			t.Fatal("source ID lost precision", d)
		}
	}
}
func TestMalformedFormat(t *testing.T) {
	for _, b := range [][]byte{[]byte(""), []byte("---\n{}"), []byte("---\n{} {}\n---\n"), []byte("---\n{\"format\":1,\"invented\":1}\n---\n"), {0xff}} {
		if _, e := Parse(b); e == nil {
			t.Fatal("accepted malformed file", string(b))
		}
	}
	for _, v := range []Value{{Type: "text", Number: ptrFloat(0)}, {Type: "reference", Ref: "../escape"}, {Type: "list", Text: "wrong payload"}, {Type: "magic"}, {Type: "time", Time: &Temporal{Start: "2010", End: "2000"}}} {
		if e := v.validate(0); e == nil {
			t.Fatal("invalid value accepted", v)
		}
	}
	cyclic := []Value{{Type: "list"}}
	cyclic[0].Items = cyclic
	if _, e := Encode(Record{Format: 1, ID: "a", Title: "A", Taxonomies: map[string][]Value{"x": cyclic}}); e == nil {
		t.Fatal("cyclic list accepted")
	}
}
func ptrFloat(v float64) *float64 { return &v }
func TestTimePrecisionAndChronologicalSort(t *testing.T) {
	if got := compareTime(Temporal{Date: "2008"}, Temporal{Date: "2008-06"}, "overlaps"); got != "possible" {
		t.Fatal(got)
	}
	if got := compareTime(Temporal{Date: "2008"}, Temporal{Date: "2008-06"}, "before"); got != "possible" {
		t.Fatal(got)
	}
	if got := compareTime(Temporal{Date: "2008-06-01"}, Temporal{Date: "2008"}, "overlaps"); got != "true" {
		t.Fatal(got)
	}
	s := openTest(t)
	for _, r := range []Record{{ID: "a", Title: "A", CreatedAt: "2020-01-01T00:00:00Z"}, {ID: "b", Title: "B", CreatedAt: "2020-01-01T00:00:00.1Z"}, {ID: "c", Title: "C", CreatedAt: "2020-01-01T01:00:00+02:00"}} {
		mustCreate(t, s, r)
	}
	out, e := s.Query(ctx, Query{Sort: []Sort{{Field: "created_at"}}})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, out, "c", "a", "b")
}
func TestCorruptIndexAndManifestRecover(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, Record{ID: "a", Title: "A", Body: "recoverable"})
	if e := os.WriteFile(filepath.Join(s.path, ".memory", "state.json"), []byte("broken cache"), 0600); e != nil {
		t.Fatal(e)
	}
	got := mustGet(t, s, "a")
	if got.Record.Body != "recoverable" {
		t.Fatal(got)
	}
	if e := os.RemoveAll(filepath.Join(s.path, ".memory", "index")); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(s.path, ".memory", "index"), []byte("not an index"), 0600); e != nil {
		t.Fatal(e)
	}
	r, e := s.Search(ctx, Recall{Text: "recoverable"})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "a")
}
func TestOriginalBodyAndCacheRebuild(t *testing.T) {
	s := openTest(t)
	body := strings.Repeat("a", 5000) + "\r\n"
	mustCreate(t, s, Record{ID: "a", Title: "A", Body: body})
	// Delete every derived artifact, retaining only the lock inode to avoid
	// teaching clients to unlink a live coordination lock.
	for _, p := range []string{"state.json", "index", "dirty"} {
		if e := os.RemoveAll(filepath.Join(s.path, ".memory", p)); e != nil {
			t.Fatal(e)
		}
	}
	d := mustGet(t, s, "a")
	if d.Record.Body != body {
		t.Fatal("rebuild lost body")
	}
}

func TestReadsAndPatchesUseAuthoritativeBody(t *testing.T) {
	s := openTest(t)
	saved := mustCreate(t, s, Record{ID: "a", Title: "A", Body: "authoritative account"})
	b, e := os.ReadFile(filepath.Join(s.path, ".memory", "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	var cached state
	if e = decodeJSON(b, &cached); e != nil {
		t.Fatal(e)
	}
	for p, f := range cached.Files {
		f.Record.Body = "corrupted derived copy"
		cached.Files[p] = f
	}
	b, e = json.Marshal(cached)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.path, ".memory", "state.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if d := mustGet(t, s, "a"); d.Record.Body != "authoritative account" {
		t.Fatal("read trusted derived body", d)
	}
	if _, e = s.Patch(ctx, "a", saved.Hash, map[string]any{"title": "Corrected"}); e != nil {
		t.Fatal(e)
	}
	if d := mustGet(t, s, "a"); d.Record.Body != "authoritative account" {
		t.Fatal("patch overwrote original from cache", d)
	}
}
func FuzzParseDoesNotPanic(f *testing.F) {
	f.Add([]byte("---\n{}\n---\nbody"))
	f.Add([]byte("plain text"))
	f.Add([]byte("---\n{\"format\":1,\"id\":\"a\",\"title\":\"A\",\"created_at\":\"2020-01-01T00:00:00Z\",\"updated_at\":\"2020-01-01T00:00:00Z\",\"taxonomies\":{\"x\":[{\"type\":\"list\",\"items\":[]}]}}\n---\nΑρχικό\r\n```\n---\n```"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, e := Parse(b)
		if e == nil {
			encoded, e := Encode(r)
			if e != nil {
				t.Fatal(e)
			}
			again, e := Parse(encoded)
			if e != nil || again.Body != r.Body {
				t.Fatal("round trip", e)
			}
		}
	})
}
