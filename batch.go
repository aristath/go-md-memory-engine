package memory

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
)

// SearchBody projects application evidence out of semantic retrieval. The hash
// ties it to the exact stored document read by the caller.
type SearchBody struct {
	Hash string `json:"hash"`
	Text string `json:"text"`
}
type SearchBatch struct {
	Queries []Recall              `json:"queries,omitempty"`
	Bodies  map[string]SearchBody `json:"bodies,omitempty"`
}
type SearchBatchResults struct {
	Results   []Results  `json:"results"`
	Documents []Document `json:"documents"`
}

// SearchMany uses one catalogue snapshot and vector cache for all queries.
// With no queries, it prepares embeddings for the projected documents only.
func (s *Store) SearchMany(ctx context.Context, r SearchBatch) (SearchBatchResults, error) {
	out := SearchBatchResults{Results: []Results{}, Documents: []Document{}}
	seen := map[string]bool{}
	err := s.with(ctx, false, func(tx *session) error {
		tx.bodies = r.Bodies
		tx.recallQueries = map[string]Results{}
		for id, body := range r.Bodies {
			doc, ok := tx.state.Docs[id]
			if !ok || doc.Hash != body.Hash {
				return errors.New("search projection changed; read documents again")
			}
		}
		if len(r.Queries) == 0 {
			if s.options.Embedding == nil {
				return errors.New("no embedding endpoint configured")
			}
			c, err := s.options.Embedding.normalized()
			if err != nil {
				return err
			}
			ids := []string{}
			for id := range r.Bodies {
				ids = append(ids, id)
			}
			return tx.vectors(ctx, c, ids)
		}
		for _, query := range r.Queries {
			if err := ctx.Err(); err != nil {
				return err
			}
			result, err := tx.search(ctx, query)
			if err != nil {
				return err
			}
			out.Results = append(out.Results, result)
			for _, hit := range result.Items {
				if !seen[hit.ID] {
					out.Documents = append(out.Documents, tx.state.Docs[hit.ID])
					seen[hit.ID] = true
				}
			}
		}
		return nil
	})
	return out, err
}
func (tx *session) embeddingContent(doc Document) string {
	record := doc.Record
	if body, ok := tx.bodies[record.ID]; ok {
		record.Body = body.Text
	}
	return searchContent(record)
}
func decodeQueryVectors(encoded []string, dimensions int) ([][]float64, error) {
	out := [][]float64{}
	for _, value := range encoded {
		b, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(b) == 0 || len(b)%4 != 0 {
			return nil, errors.New("invalid query vector")
		}
		n := len(b) / 4
		if dimensions > 0 && n != dimensions {
			return nil, errors.New("query embedding dimensions differ")
		}
		if dimensions == 0 {
			dimensions = n
		}
		if n != dimensions {
			return nil, errors.New("query embedding dimensions differ")
		}
		v := make([]float64, n)
		norm := 0.0
		for i := range v {
			v[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])))
			if math.IsNaN(v[i]) || math.IsInf(v[i], 0) {
				return nil, errors.New("invalid query vector")
			}
			norm += v[i] * v[i]
		}
		if norm == 0 || math.IsInf(norm, 0) {
			return nil, errors.New("invalid query vector")
		}
		norm = math.Sqrt(norm)
		for i := range v {
			v[i] /= norm
		}
		out = append(out, v)
	}
	return out, nil
}
