package memory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/lang/el"
	"github.com/blevesearch/bleve/v2/analysis/lang/en"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/token/porter"
	unicodeTokenizer "github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
)

func fieldKey(s string) string             { return hex.EncodeToString([]byte(s)) }
func exactKey(name string, v Value) string { return fieldKey(name) + ":" + fieldKey(valueKey(v)) }
func valueText(v Value) string {
	switch v.Type {
	case "text":
		return v.Text
	case "number":
		return strconv.FormatFloat(*v.Number, 'g', -1, 64)
	case "boolean":
		return strconv.FormatBool(*v.Boolean)
	case "reference":
		return v.Ref
	case "time":
		return strings.Join([]string{v.Time.Date, v.Time.Start, v.Time.End, v.Time.Description}, " ")
	case "list":
		parts := []string{}
		for _, x := range v.Items {
			parts = append(parts, valueText(x))
		}
		return strings.Join(parts, " ")
	}
	return ""
}
func indexMapping() (*mapping.IndexMappingImpl, error) {
	m := bleve.NewIndexMapping()
	m.ScoringModel = "bm25"
	if e := m.AddCustomAnalyzer("memory_text", map[string]any{"type": custom.Name, "tokenizer": unicodeTokenizer.Name, "token_filters": []string{lowercase.Name, en.StopName, el.StopName, porter.Name}}); e != nil {
		return nil, e
	}
	d := bleve.NewDocumentMapping()
	for _, name := range []string{"keys", "names", "refs", "kind"} {
		f := bleve.NewKeywordFieldMapping()
		d.AddFieldMappingsAt(name, f)
	}
	for name, analyzer := range map[string]string{"text": "memory_text", "title": "memory_text"} {
		f := bleve.NewTextFieldMapping()
		f.Analyzer = analyzer
		d.AddFieldMappingsAt(name, f)
	}
	m.DefaultMapping = d
	return m, nil
}
func searchContent(r Record) string {
	var b strings.Builder
	b.WriteString(r.Title)
	b.WriteByte('\n')
	b.WriteString(r.Body)
	names := make([]string, 0, len(r.Taxonomies))
	for n := range r.Taxonomies {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteByte('\n')
		b.WriteString(n)
		for _, v := range leaves(r.Taxonomies[n]) {
			b.WriteByte(' ')
			switch v.Type {
			case "text":
				b.WriteString(v.Text)
			case "time":
				b.WriteString(v.Time.Description)
			default:
				b.WriteString(valueKey(v))
			}
		}
	}
	return b.String()
}
func indexRecord(r Record) map[string]any {
	keys, names, refs := []string{}, []string{}, []string{}
	numbers := map[string][]float64{}
	for name, values := range r.Taxonomies {
		names = append(names, fieldKey(name))
		for _, v := range append(append([]Value{}, values...), leaves(values)...) {
			keys = append(keys, exactKey(name, v))
			if v.Type == "number" {
				numbers[fieldKey(name)] = append(numbers[fieldKey(name)], *v.Number)
			}
		}
	}
	for _, l := range allLinks(r) {
		refs = append(refs, l.Target)
	}
	content := searchContent(r)
	return map[string]any{"keys": keys, "names": names, "numbers": numbers, "refs": refs, "kind": r.Kind, "text": content, "title": r.Title}
}
func term(field, value string) query.Query {
	q := bleve.NewTermQuery(value)
	q.SetField(field)
	return q
}
func lexical(text string) query.Query {
	qs := []query.Query{}
	for _, field := range []string{"text", "title"} {
		q := bleve.NewMatchQuery(text)
		q.SetField(field)
		if field == "title" {
			q.SetBoost(2)
		}
		qs = append(qs, q)
	}
	return bleve.NewDisjunctionQuery(qs...)
}
func bleveRequest(text string, ids []string) *bleve.SearchRequest {
	q := bleve.NewConjunctionQuery(lexical(text), bleve.NewDocIDQuery(ids))
	r := bleve.NewSearchRequestOptions(q, len(ids), 0, false)
	r.SortBy([]string{"-_score", "_id"})
	return r
}

type Filter struct {
	Taxonomy string `json:"taxonomy"`
	Op       string `json:"op"`
	Value    *Value `json:"value,omitempty"`
}
type Sort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}
type Query struct {
	All              []Filter `json:"all,omitempty"`
	Any              []Filter `json:"any,omitempty"`
	IDs              []string `json:"ids,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	Text             string   `json:"text,omitempty"`
	Sort             []Sort   `json:"sort,omitempty"`
	IncludeUncertain bool     `json:"include_uncertain,omitempty"`
	Page
}
type Match struct {
	Taxonomy  string `json:"taxonomy"`
	Op        string `json:"op"`
	Certainty string `json:"certainty"`
}
type Hit struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	Kind          string           `json:"kind,omitempty"`
	Path          string           `json:"path"`
	Hash          string           `json:"hash"`
	Excerpt       string           `json:"excerpt"`
	BodyStartLine int              `json:"body_start_line"`
	BodyEndLine   int              `json:"body_end_line"`
	Sources       []map[string]any `json:"sources,omitempty"`
	Matches       []Match          `json:"matches,omitempty"`
	Certainty     string           `json:"certainty"`
	Score         float64          `json:"score,omitempty"`
}
type Results struct {
	Items             []Hit        `json:"items"`
	Total             int          `json:"total"`
	NextOffset        *int         `json:"next_offset,omitempty"`
	UncertainExcluded int          `json:"uncertain_excluded,omitempty"`
	Diagnostics       []Diagnostic `json:"diagnostics,omitempty"`
	Mode              string       `json:"mode"`
	Exhaustive        bool         `json:"exhaustive"`
	Degraded          []string     `json:"degraded,omitempty"`
}

func (q Query) validate() error {
	if _, _, e := q.Page.bounds(0); e != nil {
		return e
	}
	for _, f := range append(append([]Filter{}, q.All...), q.Any...) {
		if !validName(f.Taxonomy) || strings.TrimSpace(f.Taxonomy) != f.Taxonomy {
			return errors.New("filter requires a canonical taxonomy name")
		}
		switch f.Op {
		case "exists", "not_exists":
			if f.Value != nil {
				return errors.New("existence filter takes no value")
			}
			continue
		case "eq", "ne", "contains", "gt", "gte", "lt", "lte", "overlaps", "before", "after":
		default:
			return fmt.Errorf("unknown filter operator %q", f.Op)
		}
		if f.Value == nil {
			return errors.New("filter value required")
		}
		if e := f.Value.validate(0); e != nil {
			return e
		}
		if f.Op == "contains" && f.Value.Type != "text" {
			return errors.New("contains needs text")
		}
		if strings.Contains("|gt|gte|lt|lte|", "|"+f.Op+"|") && f.Value.Type != "number" {
			return errors.New("numeric comparison needs number")
		}
		if (f.Op == "overlaps" || f.Op == "before" || f.Op == "after") && f.Value.Type != "time" {
			return errors.New("temporal comparison needs time")
		}
	}
	for _, s := range q.Sort {
		if s.Field != "id" && s.Field != "title" && s.Field != "created_at" && s.Field != "updated_at" && !strings.HasPrefix(s.Field, "taxonomy:") {
			return fmt.Errorf("invalid sort field %q", s.Field)
		}
	}
	return nil
}
func candidate(f Filter) query.Query {
	switch f.Op {
	case "eq":
		return term("keys", exactKey(f.Taxonomy, *f.Value))
	case "exists", "contains", "overlaps", "before", "after":
		return term("names", fieldKey(f.Taxonomy))
	case "gt", "gte", "lt", "lte":
		lo, hi := math.Inf(-1), math.Inf(1)
		li, hiInc := true, true
		n := *f.Value.Number
		switch f.Op {
		case "gt":
			lo = n
			li = false
		case "gte":
			lo = n
		case "lt":
			hi = n
			hiInc = false
		case "lte":
			hi = n
		}
		q := bleve.NewNumericRangeInclusiveQuery(&lo, &hi, &li, &hiInc)
		q.SetField("numbers." + fieldKey(f.Taxonomy))
		return q
	default:
		return bleve.NewMatchAllQuery()
	}
}
func (q Query) candidate() query.Query {
	parts := []query.Query{}
	for _, f := range q.All {
		parts = append(parts, candidate(f))
	}
	if len(q.Any) > 0 {
		any := []query.Query{}
		for _, f := range q.Any {
			any = append(any, candidate(f))
		}
		parts = append(parts, bleve.NewDisjunctionQuery(any...))
	}
	if len(q.IDs) > 0 {
		parts = append(parts, bleve.NewDocIDQuery(q.IDs))
	}
	if q.Kind != "" {
		parts = append(parts, term("kind", q.Kind))
	}
	if q.Text != "" {
		parts = append(parts, lexical(q.Text))
	}
	if len(parts) == 0 {
		return bleve.NewMatchAllQuery()
	}
	return bleve.NewConjunctionQuery(parts...)
}
func filterMatch(r Record, f Filter) string {
	values, exists := r.Taxonomies[f.Taxonomy]
	if f.Op == "exists" {
		if exists {
			return "true"
		}
		return "false"
	}
	if f.Op == "not_exists" {
		if !exists {
			return "true"
		}
		return "false"
	}
	all := append(append([]Value{}, values...), leaves(values)...)
	best := "false"
	for _, v := range all {
		result := "false"
		switch f.Op {
		case "eq", "ne":
			if valueKey(v) == valueKey(*f.Value) {
				result = "true"
			}
		case "contains":
			if v.Type == "text" && strings.Contains(strings.ToLower(v.Text), strings.ToLower(f.Value.Text)) {
				result = "true"
			}
		case "gt", "gte", "lt", "lte":
			if v.Type == "number" {
				a, b := *v.Number, *f.Value.Number
				if f.Op == "gt" && a > b || f.Op == "gte" && a >= b || f.Op == "lt" && a < b || f.Op == "lte" && a <= b {
					result = "true"
				}
			}
		case "overlaps", "before", "after":
			if v.Type == "time" {
				result = compareTime(*v.Time, *f.Value.Time, f.Op)
			}
		}
		if result == "true" {
			best = result
			break
		}
		if result == "possible" {
			best = result
		} else if result == "unknown" && best == "false" {
			best = result
		}
	}
	if f.Op == "ne" {
		if best == "true" {
			return "false"
		}
		return "true"
	}
	return best
}
func matchRecord(r Record, q Query) (string, []Match) {
	certainty := "true"
	matches := []Match{}
	for _, f := range q.All {
		m := filterMatch(r, f)
		if m == "false" {
			return m, nil
		}
		matches = append(matches, Match{f.Taxonomy, f.Op, m})
		if m == "unknown" || m == "possible" && certainty == "true" {
			certainty = m
		}
	}
	if len(q.Any) > 0 {
		best := "false"
		var found []Match
		for _, f := range q.Any {
			m := filterMatch(r, f)
			if m != "false" {
				found = append(found, Match{f.Taxonomy, f.Op, m})
			}
			if m == "true" {
				best = m
			} else if best != "true" && (m == "possible" || m == "unknown" && best == "false") {
				best = m
			}
		}
		if best == "false" {
			return best, nil
		}
		matches = append(matches, found...)
		if certainty == "true" {
			certainty = best
		} else if best == "unknown" {
			certainty = best
		}
	}
	return certainty, matches
}
func excerpt(body, text string) (string, int, int) {
	lines := strings.Split(body, "\n")
	start := 0
	tokens := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	found := false
	for i, l := range lines {
		for _, token := range tokens {
			if strings.Contains(strings.ToLower(l), token) {
				start = i
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	var out []rune
	end := start
	for i := start; i < len(lines); i++ {
		line := []rune(lines[i])
		remaining := 320 - len(out)
		if remaining <= 0 {
			break
		}
		if i > start {
			out = append(out, '\n')
			remaining--
		}
		if len(line) > remaining {
			line = line[:remaining]
		}
		out = append(out, line...)
		end = i
		if len(out) >= 320 {
			break
		}
	}
	return string(out), start + 1, end + 1
}
func hit(d Document, text string) Hit {
	x, start, end := excerpt(d.Record.Body, text)
	return Hit{ID: d.Record.ID, Title: d.Record.Title, Kind: d.Record.Kind, Path: d.Path, Hash: d.Hash, Excerpt: x, BodyStartLine: start, BodyEndLine: end, Sources: d.Record.Sources, Certainty: "definite"}
}
func snippetTerms(idx bleve.Index, text string) string {
	if text == "" {
		return ""
	}
	analyzer := idx.Mapping().AnalyzerNamed("memory_text")
	if analyzer == nil {
		return text
	}
	terms := []string{}
	for _, token := range analyzer.Analyze([]byte(text)) {
		terms = append(terms, string(token.Term))
	}
	return strings.Join(terms, " ")
}
func (tx *session) query(ctx context.Context, q Query) (Results, error) {
	out := Results{Items: []Hit{}, Mode: "query", Diagnostics: tx.state.Diagnostics}
	if e := q.validate(); e != nil {
		return out, e
	}
	req := bleve.NewSearchRequestOptions(q.candidate(), len(tx.state.Docs), 0, false)
	req.SortBy([]string{"_id"})
	res, e := tx.index.SearchInContext(ctx, req)
	if e != nil {
		return out, e
	}
	snippetText := snippetTerms(tx.index, q.Text)
	for _, h := range res.Hits {
		d, ok := tx.state.Docs[h.ID]
		if !ok {
			continue
		}
		certainty, matches := matchRecord(d.Record, q)
		if certainty == "false" {
			continue
		}
		if certainty != "true" && !q.IncludeUncertain {
			out.UncertainExcluded++
			continue
		}
		item := hit(d, snippetText)
		item.Certainty = certainty
		if certainty == "true" {
			item.Certainty = "definite"
		}
		for i := range matches {
			if matches[i].Certainty == "true" {
				matches[i].Certainty = "definite"
			}
		}
		item.Matches = matches
		out.Items = append(out.Items, item)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		for _, s := range q.Sort {
			cmp := compareSort(tx.state.Docs[a.ID].Record, tx.state.Docs[b.ID].Record, s.Field)
			if cmp != 0 {
				if s.Desc {
					return cmp > 0
				}
				return cmp < 0
			}
		}
		return a.ID < b.ID
	})
	out.Total = len(out.Items)
	out.Exhaustive = len(out.Diagnostics) == 0 && out.UncertainExcluded == 0
	start, end, _ := q.Page.bounds(out.Total)
	out.Items = out.Items[start:end]
	if end < out.Total {
		out.NextOffset = &end
	}
	return out, nil
}
func compareSort(a, b Record, field string) int {
	if field == "created_at" || field == "updated_at" {
		av, bv := a.CreatedAt, b.CreatedAt
		if field == "updated_at" {
			av, bv = a.UpdatedAt, b.UpdatedAt
		}
		at, _ := time.Parse(time.RFC3339Nano, av)
		bt, _ := time.Parse(time.RFC3339Nano, bv)
		if at.Before(bt) {
			return -1
		}
		if at.After(bt) {
			return 1
		}
		return 0
	}
	if strings.HasPrefix(field, "taxonomy:") {
		name := strings.TrimPrefix(field, "taxonomy:")
		av, bv := a.Taxonomies[name], b.Taxonomies[name]
		if len(av) == 0 || len(bv) == 0 {
			return len(av) - len(bv)
		}
		x, y := av[0], bv[0]
		if x.Type == "number" && y.Type == "number" {
			if *x.Number < *y.Number {
				return -1
			}
			if *x.Number > *y.Number {
				return 1
			}
			return 0
		}
		return strings.Compare(valueKey(x), valueKey(y))
	}
	get := func(r Record) string {
		switch field {
		case "title":
			return r.Title
		case "created_at":
			return r.CreatedAt
		case "updated_at":
			return r.UpdatedAt
		default:
			return r.ID
		}
	}
	return strings.Compare(get(a), get(b))
}

// Query applies combined indexed predicates, deterministic sorting and paging.
// Results explicitly distinguish excluded uncertain matches and invalid sources.
func (s *Store) Query(ctx context.Context, q Query) (Results, error) {
	var out Results
	e := s.with(ctx, false, func(tx *session) error { var e error; out, e = tx.query(ctx, q); return e })
	return out, e
}
func taxonomyNames(docs map[string]Document) []string {
	set := map[string]bool{}
	for _, d := range docs {
		for n := range d.Record.Taxonomies {
			set[n] = true
		}
	}
	out := []string{}
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Taxonomies returns only current taxonomy names and their fingerprint, suitable
// for application-side model context preparation. It does not return entries.
func (s *Store) Taxonomies(ctx context.Context) (Vocabulary, error) {
	var out Vocabulary
	e := s.with(ctx, false, func(tx *session) error {
		out.Names = taxonomyNames(tx.state.Docs)
		b, _ := json.Marshal(out.Names)
		out.Revision = digest(b)
		out.Diagnostics = tx.state.Diagnostics
		return nil
	})
	return out, e
}

// Entries explicitly discovers values for one taxonomy with substring search
// and paging. Count is the number of distinct records assigning that value.
func (s *Store) Entries(ctx context.Context, name, search string, p Page) (Entries, error) {
	out := Entries{Items: []Entry{}}
	e := s.with(ctx, false, func(tx *session) error {
		if !validName(name) {
			return errors.New("taxonomy name required")
		}
		counts := map[string]Entry{}
		req := bleve.NewSearchRequestOptions(term("names", fieldKey(name)), len(tx.state.Docs), 0, false)
		res, e := tx.index.SearchInContext(ctx, req)
		if e != nil {
			return e
		}
		for _, h := range res.Hits {
			d := tx.state.Docs[h.ID]
			seen := map[string]bool{}
			for _, v := range d.Record.Taxonomies[name] {
				k := valueKey(v)
				if seen[k] || !strings.Contains(strings.ToLower(valueText(v)), strings.ToLower(search)) {
					continue
				}
				seen[k] = true
				entry := counts[k]
				entry.Value = v
				entry.Count++
				counts[k] = entry
			}
		}
		keys := []string{}
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out.Total = len(keys)
		start, end, e := p.bounds(out.Total)
		if e != nil {
			return e
		}
		for _, k := range keys[start:end] {
			out.Items = append(out.Items, counts[k])
		}
		if end < out.Total {
			out.NextOffset = &end
		}
		out.Diagnostics = tx.state.Diagnostics
		return nil
	})
	return out, e
}
