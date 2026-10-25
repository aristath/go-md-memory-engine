package memory

import (
	"encoding/base64"
	"encoding/binary"
	"math"
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
