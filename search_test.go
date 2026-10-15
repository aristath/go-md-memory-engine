package memory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func embeddingFixture(t *testing.T) (*EmbeddingConfig, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	calls := &atomic.Int64{}
	fail := &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "do not expose provider diagnostics", 503)
			return
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Error("unexpected endpoint", r.URL.Path)
		}
		var input struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		if e := json.NewDecoder(r.Body).Decode(&input); e != nil {
			t.Error(e)
		}
		data := []map[string]any{}
		for i, text := range input.Input {
			calls.Add(1)
			s := strings.ToLower(text)
			v := []float64{0.01, 0.01, 0.01}
			if strings.Contains(s, "music") || strings.Contains(s, "μουσική") || strings.Contains(s, "recording") {
				v[0] = 1
			}
			if strings.Contains(s, "food") || strings.Contains(s, "φαγητό") {
				v[1] = 1
			}
			if strings.Contains(s, "travel") {
				v[2] = 1
			}
			data = append(data, map[string]any{"index": i, "embedding": v})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return &EmbeddingConfig{BaseURL: server.URL + "/v1", Model: "test-multilingual", Dimensions: 3}, calls, fail
}

func TestVectorCacheIsolationDeletionAndRebuild(t *testing.T) {
	c, calls, _ := embeddingFixture(t)
	s, e := Open(t.TempDir(), Options{Embedding: c})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	mustCreate(t, s, Record{ID: "a", Title: "A", Body: "music"})
	mustCreate(t, s, Record{ID: "b", Title: "B", Body: "food"})
	if _, e = s.Search(ctx, Recall{Text: "music", Mode: "semantic"}); e != nil {
		t.Fatal(e)
	}
	// A corrupt optional cache must not affect ordinary indexed operations.
	if e = os.WriteFile(filepath.Join(s.path, ".memory", "vectors.gob"), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	r, e := s.Search(ctx, Recall{Text: "music"})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "a")
	before := calls.Load()
	r, e = s.Search(ctx, Recall{Text: "music", Mode: "semantic"})
	if e != nil || len(r.Degraded) > 0 || calls.Load() != before+3 {
		t.Fatal(r, e, calls.Load(), before)
	}
	d := mustGet(t, s, "b")
	if _, e = s.Delete(ctx, "b", d.Hash); e != nil {
		t.Fatal(e)
	}
	before = calls.Load()
	r, e = s.Search(ctx, Recall{Text: "music", Mode: "semantic"})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "a")
	if calls.Load() != before+1 {
		t.Fatal("unmodified record re-embedded")
	}
	b, e := os.ReadFile(filepath.Join(s.path, ".memory", "vectors.gob"))
	if e != nil {
		t.Fatal(e)
	}
	var cached map[string]vector
	if e = decodeVectors(b, &cached); e != nil {
		t.Fatal(e)
	}
	if _, ok := cached["b"]; ok {
		t.Fatal("deleted vector retained after semantic sync")
	}
	if _, e = s.Rebuild(ctx); e != nil {
		t.Fatal(e)
	}
	before = calls.Load()
	if _, e = s.Search(ctx, Recall{Text: "music", Mode: "semantic"}); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != before+2 {
		t.Fatal("rebuild retained derived vectors")
	}
}
func TestSemanticHybridCacheAndDegradation(t *testing.T) {
	c, calls, fail := embeddingFixture(t)
	s, e := Open(t.TempDir(), Options{Embedding: c})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	mustCreate(t, s, Record{ID: "music", Title: "A", Body: "Παίξαμε μουσική στο στούντιο.", Taxonomies: map[string][]Value{"people": {Text("Dimitra")}}})
	mustCreate(t, s, Record{ID: "food", Title: "B", Body: "We enjoyed food together."})
	for _, mode := range []string{"semantic", "hybrid"} {
		r, e := s.Search(ctx, Recall{Text: "recording music", Mode: mode, Page: Page{Limit: 1}})
		if e != nil || r.Items[0].ID != "music" || len(r.Degraded) > 0 {
			t.Fatal(mode, r, e)
		}
	}
	before := calls.Load()
	if _, e = s.Search(ctx, Recall{Text: "music", Mode: "semantic"}); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != before+1 {
		t.Fatal("cached documents re-embedded", before, calls.Load())
	}
	d := mustGet(t, s, "music")
	d.Record.Body = "We now discuss travel."
	if _, e = s.Update(ctx, d.Record, d.Hash); e != nil {
		t.Fatal(e)
	}
	before = calls.Load()
	if _, e = s.Search(ctx, Recall{Text: "travel", Mode: "semantic"}); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != before+2 {
		t.Fatal("changed content not re-embedded")
	}
	s.options.Embedding.Model = "different-model"
	before = calls.Load()
	if _, e = s.Search(ctx, Recall{Text: "travel", Mode: "semantic"}); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != before+3 {
		t.Fatal("model switch reused stale vectors")
	}
	fail.Store(true)
	r, e := s.Search(ctx, Recall{Text: "food", Mode: "hybrid"})
	if e != nil || r.Mode != "lexical" || len(r.Degraded) != 1 {
		t.Fatal(r, e)
	}
	requireIDs(t, r, "food")
	if strings.Contains(r.Degraded[0], "do not expose") {
		t.Fatal("provider body leaked")
	}
	r, e = s.Query(ctx, Query{IDs: []string{"music"}})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "music")
}
func TestSemanticFilteringChunksAndNoEndpoint(t *testing.T) {
	c, _, _ := embeddingFixture(t)
	c.ChunkRunes = 100
	s, e := Open(t.TempDir(), Options{Embedding: c})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	mustCreate(t, s, Record{ID: "long", Title: "Long", Body: strings.Repeat("padding ", 100) + "music recording at the studio", Taxonomies: map[string][]Value{"selected": {Boolean(true)}}})
	mustCreate(t, s, Record{ID: "other", Title: "Other", Body: "food"})
	r, e := s.Search(ctx, Recall{Text: "μουσική", Mode: "semantic", Filter: Query{All: []Filter{{"selected", "eq", ptr(Boolean(true))}}}})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "long")
	if !strings.Contains(r.Items[0].Excerpt, "music") {
		t.Fatal("did not return relevant semantic chunk", r)
	}
	other := openTest(t)
	mustCreate(t, other, Record{Title: "Music", Body: "music"})
	r, e = other.Search(ctx, Recall{Text: "music", Mode: "semantic"})
	if e != nil || len(r.Degraded) != 1 || r.Mode != "lexical" {
		t.Fatal(r, e)
	}
}
func TestEmbeddingResponseValidation(t *testing.T) {
	for _, body := range []string{`{"data":[]}`, `{"data":[{"index":0,"embedding":[]}]}`, `{"data":[{"index":0,"embedding":[0,0]}]}`, `{"data":[{"index":2,"embedding":[1,0]}]}`, `{"data":[{"index":0,"embedding":[1]}]}`, `not JSON`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		c := EmbeddingConfig{BaseURL: server.URL, Model: "m", Dimensions: 2, BatchSize: 1, TimeoutSeconds: 1}
		if _, e := c.embed(ctx, []string{"x"}); e == nil {
			t.Error("accepted response", body)
		}
		server.Close()
	}
	for _, base := range []string{"file:///tmp/foo", "http://secret@example.com/v1", "/relative", "http://example.com/v1?token=secret"} {
		if _, e := (EmbeddingConfig{BaseURL: base, Model: "m"}).normalized(); e == nil {
			t.Fatal(base)
		}
	}
}
