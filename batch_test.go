package memory

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func encodedVector(v ...float32) string {
	b := make([]byte, len(v)*4)
	for i, n := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(n))
	}
	return base64.StdEncoding.EncodeToString(b)
}
func TestBatchCachedQueriesAndEvidenceProjection(t *testing.T) {
	c, calls, _ := embeddingFixture(t)
	s, err := Open(t.TempDir(), Options{Embedding: c})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustCreate(t, s, Record{ID: "a", Body: "music; original testimony also mentions food"})
	mustCreate(t, s, Record{ID: "b", Body: "food; original testimony also mentions music"})
	a, b := mustGet(t, s, "a"), mustGet(t, s, "b")
	request := SearchBatch{Bodies: map[string]SearchBody{"a": {Hash: a.Hash, Text: "music"}, "b": {Hash: b.Hash, Text: "food"}}, Queries: []Recall{
		{Text: "music", Mode: "semantic", Vectors: []string{encodedVector(1, 0, 0)}, Page: Page{Limit: 1}},
		{Text: "food", Mode: "semantic", Vectors: []string{encodedVector(0, 1, 0)}, Page: Page{Limit: 1}},
	}}
	result, err := s.SearchMany(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	requireIDs(t, result.Results[0], "a")
	requireIDs(t, result.Results[1], "b")
	if calls.Load() != 2 || len(result.Documents) != 2 {
		t.Fatal("queries re-embedded, or snapshot documents absent", calls.Load(), result)
	}
	if _, err = s.SearchMany(ctx, request); err != nil || calls.Load() != 2 {
		t.Fatal("warm search generated embeddings", err, calls.Load())
	}
	// Multi-part messages use the strongest match across their chunks.
	request.Queries = []Recall{{Text: "two subjects", Mode: "semantic", Vectors: []string{encodedVector(1, 0, 0), encodedVector(0, 1, 0)}, Page: Page{Limit: 2}}}
	result, err = s.SearchMany(ctx, request)
	if err != nil || len(result.Results[0].Items) != 2 {
		t.Fatal(result, err)
	}
	b.Record.Body = "changed independently"
	if _, err = s.Update(ctx, b.Record, b.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SearchMany(ctx, request); err == nil {
		t.Fatal("accepted an obsolete projection")
	}
}

func TestPartialEmbeddingPreparationIsResumable(t *testing.T) {
	var calls atomic.Int64
	var failing atomic.Bool
	failing.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		n := calls.Add(int64(len(body.Input)))
		if failing.Load() && n > 16 {
			http.Error(w, "unavailable", 503)
			return
		}
		rows := []map[string]any{}
		for i := range body.Input {
			rows = append(rows, map[string]any{"index": i, "embedding": []float64{1, 0}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	defer server.Close()
	s, err := Open(t.TempDir(), Options{Embedding: &EmbeddingConfig{BaseURL: server.URL, Model: "test", Dimensions: 2, BatchSize: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bodies := map[string]SearchBody{}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("record-%02d", i)
		mustCreate(t, s, Record{ID: id, Body: "body"})
		d := mustGet(t, s, id)
		bodies[id] = SearchBody{Hash: d.Hash, Text: "standalone"}
	}
	if _, err = s.SearchMany(ctx, SearchBatch{Bodies: bodies}); err == nil {
		t.Fatal("expected provider failure")
	}
	if calls.Load() != 17 {
		t.Fatal(calls.Load())
	}
	failing.Store(false)
	if _, err = s.SearchMany(ctx, SearchBatch{Bodies: bodies}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 41 {
		t.Fatal("completed vectors were not reused", calls.Load())
	}
}
