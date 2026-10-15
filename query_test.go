package memory

import (
	"fmt"
	"strings"
	"testing"
)

func ptr(v Value) *Value  { return &v }
func tv(t Temporal) Value { return Value{Type: "time", Time: &t} }
func TestStructuredQueryAndSorting(t *testing.T) {
	s := openTest(t)
	for i := 0; i < 5; i++ {
		mustCreate(t, s, Record{ID: fmt.Sprintf("m%d", i), Title: fmt.Sprintf("Title %d", 4-i), Kind: "fact", Body: strings.Repeat("padding ", 80) + "\nΗ Δήμητρα έπαιζε μουσική στο στούντιο.\nA recording session in Athens.", Taxonomies: map[string][]Value{"people": {Text("Dimitra")}, "rating": {Number(float64(i))}, "published": {Boolean(i%2 == 0)}, "nested": {{Type: "list", Items: []Value{Text("jazz"), Number(float64(i))}}}}})
	}
	q := Query{All: []Filter{{"rating", "gte", ptr(Number(2))}, {"published", "eq", ptr(Boolean(true))}}, Sort: []Sort{{Field: "taxonomy:rating", Desc: true}}, Page: Page{Limit: 1}}
	r, e := s.Query(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "m4")
	if r.Total != 2 || r.NextOffset == nil {
		t.Fatal(r)
	}
	q.Offset = *r.NextOffset
	r, e = s.Query(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "m2")
	r, e = s.Query(ctx, Query{Any: []Filter{{"rating", "lt", ptr(Number(1))}, {"rating", "gt", ptr(Number(3))}}})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "m0", "m4")
	for _, f := range []Filter{{"nested", "eq", ptr(Text("jazz"))}, {"people", "contains", ptr(Text("mitr"))}, {"absent", "not_exists", nil}, {"absent", "ne", ptr(Text("x"))}, {"people", "exists", nil}} {
		r, e = s.Query(ctx, Query{All: []Filter{f}})
		if e != nil || r.Total != 5 {
			t.Fatal(f, r, e)
		}
	}
	r, e = s.Query(ctx, Query{IDs: []string{"m2"}, Kind: "fact", Text: "recording"})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "m2")
	if !strings.Contains(r.Items[0].Excerpt, "recording") || r.Items[0].BodyStartLine != 3 {
		t.Fatal("irrelevant excerpt", r.Items[0])
	}
	for _, term := range []string{"μουσική", "Δήμητρα", "Athens"} {
		r, e = s.Search(ctx, Recall{Text: term, Mode: "lexical"})
		if e != nil || r.Total != 5 {
			t.Fatal(term, r, e)
		}
	}
	stopwords, e := s.Search(ctx, Recall{Text: "στην με and the", Mode: "lexical"})
	if e != nil || stopwords.Total != 0 {
		t.Fatal("function words produced lexical evidence", stopwords, e)
	}
	r, e = s.Query(ctx, Query{Sort: []Sort{{Field: "title"}}, Page: Page{Offset: 3, Limit: 20}})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "m1", "m0")
}
func TestTemporalSemantics(t *testing.T) {
	for _, value := range []Temporal{{Date: "2024"}, {Date: "2024-02"}, {Date: "2024-02-29"}, {Start: "2008", End: "2010"}, {End: "2010"}, {Start: "2008"}, {Unknown: true, Description: "before moving"}, {Date: "2008", Approximate: true}} {
		if _, _, e := value.bounds(); e != nil {
			t.Fatal(value, e)
		}
	}
	for _, value := range []Temporal{{Date: "2023-02-29"}, {Date: "2024-13"}, {Date: "0000"}, {Date: "today"}, {Date: "2008", Start: "2009"}, {Start: "2010", End: "2008"}, {Unknown: true, Start: "2008"}, {Description: "sometime"}} {
		if _, _, e := value.bounds(); e == nil {
			t.Fatal("accepted", value)
		}
	}
	lo, hi, e := dateBounds("2024-02")
	if e != nil || hi-lo+1 != 29*86400 {
		t.Fatal(lo, hi, e)
	}
	s := openTest(t)
	dates := map[string]Temporal{"known": {Date: "2008"}, "approx": {Start: "2008", End: "2010", Approximate: true}, "unknown": {Unknown: true}, "later": {Date: "2015"}, "open": {End: "2007"}}
	for id, date := range dates {
		mustCreate(t, s, Record{ID: id, Title: id, Taxonomies: map[string][]Value{"event_time": {tv(date)}}})
	}
	q := Query{All: []Filter{{"event_time", "overlaps", ptr(tv(Temporal{Date: "2008"}))}}}
	r, e := s.Query(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "known")
	if r.UncertainExcluded != 2 {
		t.Fatal(r)
	}
	q.IncludeUncertain = true
	r, e = s.Query(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "approx", "known", "unknown")
	if r.Items[0].Certainty != "possible" || r.Items[2].Certainty != "unknown" {
		t.Fatal(r)
	}
	q.IncludeUncertain = false
	q.All[0].Op = "before"
	r, e = s.Query(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "open")
	q.All[0].Op = "after"
	r, e = s.Query(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "later")
}
func TestGraph(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, Record{ID: "dimitra", Title: "Dimitra", Kind: "person"})
	mustCreate(t, s, Record{ID: "a", Title: "A", Taxonomies: map[string][]Value{"people": {Reference("dimitra")}}, Links: []Link{{"related", "b"}, {"related", "missing"}}})
	mustCreate(t, s, Record{ID: "b", Title: "B", Links: []Link{{"related", "a"}, {"part-of", "c"}}})
	mustCreate(t, s, Record{ID: "c", Title: "C"})
	r, e := s.Links(ctx, "dimitra", GraphOptions{Direction: "incoming"})
	if e != nil || r.Total != 1 || r.Edges[0].From != "a" {
		t.Fatal(r, e)
	}
	r, e = s.Traverse(ctx, "a", GraphOptions{Depth: 2, Direction: "outgoing"})
	if e != nil || r.Total != 5 {
		t.Fatal(r, e)
	}
	r, e = s.Links(ctx, "a", GraphOptions{Direction: "outgoing", Relation: "related", Page: Page{Limit: 1}})
	if e != nil || r.Total != 2 || r.NextOffset == nil {
		t.Fatal(r, e)
	}
	r, e = s.Links(ctx, "a", GraphOptions{Direction: "outgoing", Relation: "related", Page: Page{Offset: 1}})
	if e != nil || r.Edges[0].Resolved {
		t.Fatal(r, e)
	}
	batch, e := s.GetMany(ctx, []string{"a", "unknown", "c"})
	if e != nil || len(batch.Items) != 2 || len(batch.Missing) != 1 {
		t.Fatal(batch, e)
	}
}
func TestQueryInputErrorsAndEmptyCollection(t *testing.T) {
	s := openTest(t)
	r, e := s.Query(ctx, Query{})
	if e != nil || r.Total != 0 || len(r.Items) != 0 {
		t.Fatal(r, e)
	}
	for _, q := range []Query{{All: []Filter{{"x", "eq", nil}}}, {All: []Filter{{"x", "gt", ptr(Text("a"))}}}, {All: []Filter{{"x", "overlaps", ptr(Text("2008"))}}}, {All: []Filter{{"x", "invented", nil}}}, {Page: Page{Offset: -1}}, {Sort: []Sort{{Field: "whatever"}}}} {
		if _, e = s.Query(ctx, q); e == nil {
			t.Fatal("accepted invalid query", q)
		}
	}
}

func TestExcerptUsesMatchingTerms(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, Record{Title: "Account", Body: "The opening paragraph.\n\nWe recorded music together."})
	r, e := s.Search(ctx, Recall{Text: "the recording"})
	if e != nil || len(r.Items) != 1 || r.Items[0].BodyStartLine != 3 {
		t.Fatal(r, e)
	}
}
