package memory

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// EmbeddingConfig uses an OpenAI-compatible /embeddings resource under BaseURL
// (usually http://host:port/v1). Secrets are never written to cache or errors.
type EmbeddingConfig struct {
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	APIKey         string `json:"-"`
	Dimensions     int    `json:"dimensions,omitempty"`
	BatchSize      int    `json:"batch_size,omitempty"`
	ChunkRunes     int    `json:"chunk_runes,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}
type vector struct {
	Content string      `json:"content"`
	Config  string      `json:"config"`
	Values  [][]float64 `json:"values"`
}
type Recall struct {
	Text          string   `json:"text"`
	Vectors       []string `json:"vectors,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	Filter        Query    `json:"filter,omitempty"`
	MinSimilarity *float64 `json:"min_similarity,omitempty"`
	Page
}

func (c EmbeddingConfig) normalized() (EmbeddingConfig, error) {
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
		return c, errors.New("embedding base_url must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if c.Model == "" {
		return c, errors.New("embedding model required")
	}
	if c.Dimensions < 0 || c.BatchSize < 0 || c.ChunkRunes < 0 || c.TimeoutSeconds < 0 {
		return c, errors.New("embedding sizes and timeout must be nonnegative")
	}
	if c.BatchSize == 0 {
		c.BatchSize = 16
	}
	if c.ChunkRunes == 0 {
		c.ChunkRunes = 2000
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 60
	}
	return c, nil
}
func (c EmbeddingConfig) key() string {
	b, _ := json.Marshal([]any{c.BaseURL, c.Model, c.Dimensions, c.ChunkRunes})
	return digest(b)
}
func chunks(text string, size int) []string {
	r := []rune(text)
	if len(r) == 0 {
		return []string{" "}
	}
	out := []string{}
	for start := 0; start < len(r); {
		end := start + min(size, len(r)-start)
		out = append(out, string(r[start:end]))
		if end == len(r) {
			break
		}
		overlap := min(size/10, 200)
		start = end - overlap
	}
	return out
}
func (c EmbeddingConfig) embed(ctx context.Context, texts []string) ([][]float64, error) {
	out := [][]float64{}
	client := http.Client{Timeout: time.Duration(c.TimeoutSeconds) * time.Second}
	dim := c.Dimensions
	for start := 0; start < len(texts); start += c.BatchSize {
		end := start + min(c.BatchSize, len(texts)-start)
		payload := map[string]any{"model": c.Model, "input": texts[start:end], "encoding_format": "float"}
		if c.Dimensions > 0 {
			payload["dimensions"] = c.Dimensions
		}
		b, e := json.Marshal(payload)
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/embeddings", bytes.NewReader(b))
		if e != nil {
			return nil, errors.New("cannot construct embedding request")
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		res, e := client.Do(req)
		if e != nil {
			return nil, fmt.Errorf("embedding request failed for inputs %d..%d of %d (%T); check connectivity or timeout", start, end, len(texts), e)
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, fmt.Errorf("embedding endpoint returned HTTP %d", res.StatusCode)
		}
		var response struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
		}
		e = json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&response)
		res.Body.Close()
		if e != nil {
			return nil, errors.New("invalid embedding JSON response")
		}
		if len(response.Data) != end-start {
			return nil, errors.New("embedding response count mismatch")
		}
		batch := make([][]float64, end-start)
		for _, d := range response.Data {
			if d.Index < 0 || d.Index >= len(batch) || batch[d.Index] != nil {
				return nil, errors.New("embedding response has invalid or duplicate indices")
			}
			if dim == 0 {
				dim = len(d.Embedding)
			}
			if dim == 0 || len(d.Embedding) != dim {
				return nil, errors.New("embedding dimension mismatch")
			}
			norm := 0.0
			for _, v := range d.Embedding {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return nil, errors.New("nonfinite embedding")
				}
				norm += v * v
			}
			if norm == 0 || math.IsInf(norm, 0) {
				return nil, errors.New("invalid embedding norm")
			}
			norm = math.Sqrt(norm)
			for i := range d.Embedding {
				d.Embedding[i] /= norm
			}
			batch[d.Index] = d.Embedding
		}
		out = append(out, batch...)
	}
	return out, nil
}
func (tx *session) vectors(ctx context.Context, c EmbeddingConfig, ids []string) error {
	// Ordinary operations never read vector data. Load this disposable cache
	// only for semantic recall, and discard stale records before scoring.
	if tx.readyVectors == nil {
		tx.readyVectors = map[string]bool{}
	}
	allReady := true
	for _, id := range ids {
		if !tx.readyVectors[id] {
			allReady = false
			break
		}
	}
	if allReady {
		return nil
	}
	b, readErr := tx.s.root.ReadFile(".memory/vectors.gob")
	if readErr == nil {
		if decodeVectors(b, &tx.state.Vectors) != nil || tx.state.Vectors == nil {
			tx.state.Vectors = map[string]vector{}
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}
	eligible := map[string]bool{}
	for _, id := range ids {
		eligible[id] = true
	}
	pruned := false
	for id, v := range tx.state.Vectors {
		d, ok := tx.state.Docs[id]
		if !ok || (eligible[id] && v.Content != digest([]byte(tx.embeddingContent(d)))) {
			delete(tx.state.Vectors, id)
			pruned = true
		}
	}
	texts := []string{}
	pending := []string{}
	counts := []int{}
	for _, id := range ids {
		d := tx.state.Docs[id]
		content := tx.embeddingContent(d)
		hash := digest([]byte(content))
		v, ok := tx.state.Vectors[id]
		if ok && v.Config == c.key() && v.Content == hash && validVector(v, c, len(chunks(content, c.ChunkRunes))) {
			tx.readyVectors[id] = true
			continue
		}
		parts := chunks(content, c.ChunkRunes)
		texts = append(texts, parts...)
		pending = append(pending, id)
		counts = append(counts, len(parts))
	}
	if len(pending) == 0 && !pruned {
		return nil
	}
	embeddings, e := c.embed(ctx, texts)
	if e != nil {
		return e
	}
	offset := 0
	for i, id := range pending {
		n := counts[i]
		tx.state.Vectors[id] = vector{Content: digest([]byte(tx.embeddingContent(tx.state.Docs[id]))), Config: c.key(), Values: embeddings[offset : offset+n]}
		offset += n
		tx.readyVectors[id] = true
	}
	// Only derived data changed. An interrupted cache write can be discarded.
	b, e = encodeVectors(tx.state.Vectors)
	if e != nil {
		return e
	}
	return tx.s.atomic(".memory/vectors.gob", b, nil, false)
}

func encodeVectors(v map[string]vector) ([]byte, error) {
	var b bytes.Buffer
	e := gob.NewEncoder(&b).Encode(v)
	return b.Bytes(), e
}
func decodeVectors(b []byte, v *map[string]vector) error {
	return gob.NewDecoder(bytes.NewReader(b)).Decode(v)
}

func validVector(v vector, c EmbeddingConfig, count int) bool {
	if len(v.Values) != count {
		return false
	}
	dim := c.Dimensions
	for _, row := range v.Values {
		if dim == 0 {
			dim = len(row)
		}
		if dim == 0 || len(row) != dim {
			return false
		}
		norm := 0.0
		for _, x := range row {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return false
			}
			norm += x * x
		}
		if math.Abs(norm-1) > 1e-6 {
			return false
		}
	}
	return true
}

// Search returns ranked lexical, semantic or hybrid candidates. Semantic failure
// falls back explicitly to lexical results; recall is never exhaustive.
func (s *Store) Search(ctx context.Context, r Recall) (Results, error) {
	var result Results
	err := s.with(ctx, false, func(tx *session) error { var err error; result, err = tx.search(ctx, r); return err })
	return result, err
}

func (tx *session) search(ctx context.Context, r Recall) (Results, error) {
	out := Results{Items: []Hit{}}
	e := func() error {
		if strings.TrimSpace(r.Text) == "" {
			return errors.New("recall text required")
		}
		if r.Mode == "" {
			r.Mode = "lexical"
		}
		if r.Mode != "lexical" && r.Mode != "semantic" && r.Mode != "hybrid" {
			return errors.New("mode must be lexical, semantic or hybrid")
		}
		if _, _, e := r.Page.bounds(0); e != nil {
			return e
		}
		if r.MinSimilarity != nil && (math.IsNaN(*r.MinSimilarity) || math.IsInf(*r.MinSimilarity, 0) || *r.MinSimilarity < -1 || *r.MinSimilarity > 1) {
			return errors.New("min_similarity must be between -1 and 1")
		}
		q := r.Filter
		q.Offset = 0
		q.Limit = max(1, len(tx.state.Docs))
		encodedQuery, _ := json.Marshal(q)
		key := digest(encodedQuery)
		eligible, cached := tx.recallQueries[key]
		var e error
		if !cached {
			eligible, e = tx.query(ctx, q)
			if tx.recallQueries != nil && e == nil {
				tx.recallQueries[key] = eligible
			}
		}
		if e != nil {
			return e
		}
		out.Diagnostics = eligible.Diagnostics
		out.UncertainExcluded = eligible.UncertainExcluded
		out.Mode = r.Mode
		byID := map[string]Hit{}
		ids := []string{}
		for _, h := range eligible.Items {
			byID[h.ID] = h
			ids = append(ids, h.ID)
		}
		lexicalScores := map[string]float64{}
		lexicalOrder := []string{}
		if len(ids) > 0 && r.Mode != "semantic" {
			req := bleveRequest(r.Text, ids)
			res, e := tx.index.SearchInContext(ctx, req)
			if e != nil {
				return e
			}
			for _, h := range res.Hits {
				lexicalScores[h.ID] = h.Score
				lexicalOrder = append(lexicalOrder, h.ID)
			}
		}
		semanticScores := map[string]float64{}
		bestChunks := map[string]string{}
		semanticOK := false
		if r.Mode != "lexical" && len(ids) > 0 {
			var semErr error
			if tx.s.options.Embedding == nil {
				semErr = errors.New("no embedding endpoint configured")
			} else {
				c, err := tx.s.options.Embedding.normalized()
				semErr = err
				if semErr == nil {
					semErr = tx.vectors(ctx, c, ids)
				}
				var queryVectors [][]float64
				if semErr == nil {
					if len(r.Vectors) > 0 {
						queryVectors, semErr = decodeQueryVectors(r.Vectors, c.Dimensions)
					} else {
						queryVectors, semErr = c.embed(ctx, []string{r.Text})
					}
				}
				if semErr == nil {
					for _, id := range ids {
						v := tx.state.Vectors[id]
						best := -math.MaxFloat64
						which := 0
						for i, values := range v.Values {
							if len(values) != len(queryVectors[0]) {
								semErr = errors.New("cached/query embedding dimensions differ; configure dimensions and re-embed")
								break
							}
							score := -math.MaxFloat64
							for _, query := range queryVectors {
								dot := 0.0
								for j, x := range values {
									dot += x * query[j]
								}
								score = max(score, dot)
							}
							score = max(-1, min(1, score))
							if score > best {
								best = score
								which = i
							}
						}
						if semErr != nil {
							break
						}
						if r.MinSimilarity == nil || best >= *r.MinSimilarity {
							semanticScores[id] = best
							bestChunks[id] = chunks(tx.embeddingContent(tx.state.Docs[id]), c.ChunkRunes)[which]
						}
					}
				}
			}
			if semErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				out.Degraded = []string{"semantic retrieval unavailable; returning lexical results: " + semErr.Error()}
				semanticScores = map[string]float64{}
			} else {
				semanticOK = true
			}
		}
		scores := lexicalScores
		if semanticOK {
			if r.Mode == "semantic" {
				scores = semanticScores
			} else {
				scores = map[string]float64{}
				for rank, id := range lexicalOrder {
					scores[id] += 1 / float64(61+rank)
				}
				order := rankScores(semanticScores)
				for rank, id := range order {
					scores[id] += 1 / float64(61+rank)
				}
			}
		}
		if r.Mode != "lexical" && !semanticOK && len(ids) > 0 {
			out.Mode = "lexical"
		}
		snippetText := snippetTerms(tx.index, r.Text)
		for _, id := range rankScores(scores) {
			h := byID[id]
			x, a, b := excerpt(tx.state.Docs[id].Record.Body, snippetText)
			h.Excerpt = x
			h.BodyStartLine = a
			h.BodyEndLine = b
			if semanticOK {
				if chunk, ok := bestChunks[id]; ok {
					h.Excerpt = string([]rune(chunk)[:min(320, len([]rune(chunk)))])
					h.BodyStartLine = 0
					h.BodyEndLine = 0
				}
			}
			h.Score = scores[id]
			out.Items = append(out.Items, h)
		}
		out.Total = len(out.Items)
		start, end, _ := r.Page.bounds(out.Total)
		out.Items = out.Items[start:end]
		if end < out.Total {
			out.NextOffset = &end
		}
		return nil
	}()
	return out, e
}
func rankScores(scores map[string]float64) []string {
	out := []string{}
	for id := range scores {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if scores[a] != scores[b] {
			return scores[a] > scores[b]
		}
		return a < b
	})
	return out
}
