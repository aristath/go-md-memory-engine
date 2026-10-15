// Command memory-measure runs a reproducible real-text indexing workload and
// optional bilingual recall evaluation. It uses and removes temporary collections.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	memory "markdownmemory"
)

type measurement struct {
	Records         int     `json:"records"`
	CorpusBytes     int64   `json:"corpus_bytes"`
	CacheBytes      int64   `json:"cache_bytes"`
	IndexMS         float64 `json:"index_ms"`
	StructuredP50   float64 `json:"structured_p50_ms"`
	StructuredP95   float64 `json:"structured_p95_ms"`
	LexicalP50      float64 `json:"lexical_p50_ms"`
	LexicalP95      float64 `json:"lexical_p95_ms"`
	TaxonomiesP50   float64 `json:"taxonomies_p50_ms"`
	OneEditSyncMS   float64 `json:"one_edit_sync_ms"`
	OneEditParsed   int     `json:"one_edit_parsed"`
	UnchangedParsed int     `json:"unchanged_parsed"`
	SemanticColdMS  float64 `json:"semantic_cold_ms,omitempty"`
	SemanticP50     float64 `json:"semantic_p50_ms,omitempty"`
	SemanticP95     float64 `json:"semantic_p95_ms,omitempty"`
	HybridP50       float64 `json:"hybrid_p50_ms,omitempty"`
	Error           string  `json:"error,omitempty"`
}
type qualityResult struct {
	Query        string   `json:"query"`
	Expected     string   `json:"expected"`
	Mode         string   `json:"mode"`
	Retrieved    []string `json:"retrieved"`
	Hit1         bool     `json:"hit_at_1"`
	Milliseconds float64  `json:"milliseconds"`
	Degraded     []string `json:"degraded,omitempty"`
}
type report struct {
	Timestamp      string          `json:"timestamp"`
	Go             string          `json:"go"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	CPUs           int             `json:"cpus"`
	CPU            string          `json:"cpu"`
	Dataset        string          `json:"dataset"`
	SourceComments int             `json:"source_comments"`
	Samples        int             `json:"samples_per_operation"`
	Measurements   []measurement   `json:"measurements,omitempty"`
	EmbeddingModel string          `json:"embedding_model,omitempty"`
	Quality        []qualityResult `json:"quality,omitempty"`
	Limitations    []string        `json:"limitations"`
	Failures       []string        `json:"failures,omitempty"`
}

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	sizes := flag.String("sizes", "100,1000,10000", "comma-separated real-text collection sizes")
	samples := flag.Int("samples", 10, "latency samples per operation")
	base := flag.String("embedding-base", os.Getenv("MEMORY_EMBEDDING_BASE"), "optional live embedding base URL")
	model := flag.String("embedding-model", os.Getenv("MEMORY_EMBEDDING_MODEL"), "live embedding model")
	batch := flag.Int("embedding-batch-size", 0, "inputs per embedding request; 0 uses 16")
	qualityOnly := flag.Bool("quality-only", false, "skip scale measurements")
	flag.Parse()
	if *samples < 1 {
		return fmt.Errorf("samples must be positive")
	}
	r := report{Timestamp: time.Now().UTC().Format(time.RFC3339), Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Samples: *samples, Dataset: "Go standard-library documentation comment groups (120..3000 bytes), one comment per memory; deterministic selection in sorted source-file order. Go sources are BSD licensed; see $GOROOT/LICENSE. This models varied real text, not personal-memory distribution.", Limitations: []string{"Latency includes collection reconciliation, derived-manifest decoding and opening/closing the persistent index, as a fresh CLI operation would.", "Warm filesystem caches; sequential workload on this host. These are not universal capacity or concurrency guarantees.", "Exact cosine semantic ranking is O(eligible chunks * embedding dimensions); no ANN approximation.", "Bilingual recall cases are hand-authored functional probes, not an independent retrieval benchmark."}}
	cpu, _ := os.ReadFile("/proc/cpuinfo")
	for _, line := range strings.Split(string(cpu), "\n") {
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			r.CPU = strings.TrimSpace(parts[1])
			break
		}
	}
	if !*qualityOnly {
		corpus, e := comments()
		if e != nil {
			return e
		}
		r.SourceComments = len(corpus)
		for _, part := range strings.Split(*sizes, ",") {
			n, e := strconv.Atoi(part)
			if e != nil || n < 1 {
				return fmt.Errorf("invalid size %q", part)
			}
			var config *memory.EmbeddingConfig
			if *base != "" {
				config = &memory.EmbeddingConfig{BaseURL: *base, Model: *model, APIKey: os.Getenv("MEMORY_EMBEDDING_API_KEY"), BatchSize: *batch}
			}
			fmt.Fprintf(os.Stderr, "Measuring %d records\n", n)
			m, e := measure(n, *samples, corpus, config)
			if e != nil {
				m.Error = e.Error()
				r.Failures = append(r.Failures, fmt.Sprintf("%d records: %v", n, e))
			}
			r.Measurements = append(r.Measurements, m)
		}
	}
	if *base != "" {
		r.EmbeddingModel = *model
		var e error
		r.Quality, e = evaluate(*base, *model)
		if e != nil {
			r.Failures = append(r.Failures, "quality evaluation: "+e.Error())
		}
	} else if *qualityOnly {
		return fmt.Errorf("quality-only requires an embedding endpoint")
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if e := enc.Encode(r); e != nil {
		return e
	}
	if len(r.Failures) > 0 {
		return fmt.Errorf("measurement failures are recorded in JSON output")
	}
	return nil
}
func comments() ([]string, error) {
	out := []string{}
	root := filepath.Join(runtime.GOROOT(), "src")
	files := []string{}
	e := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() && (d.Name() == "testdata" || d.Name() == "vendor") {
			return fs.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	sort.Strings(files)
	for _, p := range files {
		f, e := parser.ParseFile(token.NewFileSet(), p, nil, parser.ParseComments)
		if e != nil {
			continue
		}
		for _, c := range f.Comments {
			text := c.Text()
			if len(text) >= 120 && len(text) <= 3000 {
				out = append(out, text)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Go documentation comments found")
	}
	return out, nil
}
func elapsed(start time.Time) float64 { return float64(time.Since(start).Nanoseconds()) / 1e6 }
func percentile(values []float64, p float64) float64 {
	sort.Float64s(values)
	return values[min(len(values)-1, int(float64(len(values))*p))]
}
func measure(n, samples int, corpus []string, config *memory.EmbeddingConfig) (measurement, error) {
	ctx := context.Background()
	m := measurement{Records: n}
	root, e := os.MkdirTemp("", "memory-measure-")
	if e != nil {
		return m, e
	}
	defer os.RemoveAll(root)
	s, e := memory.Open(root, memory.Options{Embedding: config})
	if e != nil {
		return m, e
	}
	defer s.Close()
	for i := 0; i < n; i++ {
		r := memory.Record{Format: 1, ID: fmt.Sprintf("m%06d", i), Title: fmt.Sprintf("Documentation %d", i), CreatedAt: "2020-01-01T00:00:00Z", UpdatedAt: "2020-01-01T00:00:00Z", Body: corpus[i%len(corpus)], Taxonomies: map[string][]memory.Value{"group": {memory.Text(fmt.Sprintf("group-%d", i%100))}, "ordinal": {memory.Number(float64(i))}}}
		b, e := memory.Encode(r)
		if e != nil {
			return m, e
		}
		m.CorpusBytes += int64(len(b))
		if e = os.WriteFile(filepath.Join(root, r.ID+".md"), b, 0600); e != nil {
			return m, e
		}
	}
	start := time.Now()
	report, e := s.Rebuild(ctx)
	if e != nil {
		return m, e
	}
	m.IndexMS = elapsed(start)
	if report.Indexed != n {
		return m, fmt.Errorf("indexed %d of %d", report.Indexed, n)
	}
	structured, lexical, vocab := []float64{}, []float64{}, []float64{}
	for i := 0; i < samples; i++ {
		v := memory.Text(fmt.Sprintf("group-%d", i%100))
		start = time.Now()
		if _, e = s.Query(ctx, memory.Query{All: []memory.Filter{{Taxonomy: "group", Op: "eq", Value: &v}}}); e != nil {
			return m, e
		}
		structured = append(structured, elapsed(start))
		start = time.Now()
		if _, e = s.Search(ctx, memory.Recall{Text: "request response", Mode: "lexical"}); e != nil {
			return m, e
		}
		lexical = append(lexical, elapsed(start))
		start = time.Now()
		if _, e = s.Taxonomies(ctx); e != nil {
			return m, e
		}
		vocab = append(vocab, elapsed(start))
	}
	m.StructuredP50 = percentile(structured, .5)
	m.StructuredP95 = percentile(structured, .95)
	m.LexicalP50 = percentile(lexical, .5)
	m.LexicalP95 = percentile(lexical, .95)
	m.TaxonomiesP50 = percentile(vocab, .5)
	if config != nil {
		fmt.Fprintf(os.Stderr, "Embedding and measuring semantic/hybrid recall for %d records\n", n)
		start = time.Now()
		r, e := s.Search(ctx, memory.Recall{Text: "receiving an HTTP request and sending a response", Mode: "semantic"})
		if e != nil {
			return m, e
		}
		if len(r.Degraded) > 0 {
			return m, fmt.Errorf("semantic measurement degraded: %v", r.Degraded)
		}
		m.SemanticColdMS = elapsed(start)
		semantic, hybrid := []float64{}, []float64{}
		for i := 0; i < samples; i++ {
			for _, mode := range []string{"semantic", "hybrid"} {
				start = time.Now()
				r, e := s.Search(ctx, memory.Recall{Text: "receiving an HTTP request and sending a response", Mode: mode})
				if e != nil {
					return m, e
				}
				if len(r.Degraded) > 0 {
					return m, fmt.Errorf("%s measurement degraded: %v", mode, r.Degraded)
				}
				if mode == "semantic" {
					semantic = append(semantic, elapsed(start))
				} else {
					hybrid = append(hybrid, elapsed(start))
				}
			}
		}
		m.SemanticP50 = percentile(semantic, .5)
		m.SemanticP95 = percentile(semantic, .95)
		m.HybridP50 = percentile(hybrid, .5)
	}
	unchanged, e := s.Sync(ctx, false)
	if e != nil {
		return m, e
	}
	m.UnchangedParsed = unchanged.Parsed
	d, e := s.Get(ctx, "m000000")
	if e != nil {
		return m, e
	}
	d.Record.Body += "\nAn external edit.\n"
	b, e := memory.Encode(d.Record)
	if e != nil {
		return m, e
	}
	if e = os.WriteFile(filepath.Join(root, d.Path), b, 0600); e != nil {
		return m, e
	}
	start = time.Now()
	edited, e := s.Sync(ctx, false)
	if e != nil {
		return m, e
	}
	m.OneEditSyncMS = elapsed(start)
	m.OneEditParsed = edited.Parsed
	e = filepath.WalkDir(filepath.Join(root, ".memory"), func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			info, e := d.Info()
			if e != nil {
				return e
			}
			m.CacheBytes += info.Size()
		}
		return nil
	})
	return m, e
}
func evaluate(base, model string) ([]qualityResult, error) {
	ctx := context.Background()
	root, e := os.MkdirTemp("", "memory-live-quality-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(root)
	s, e := memory.Open(root, memory.Options{Embedding: &memory.EmbeddingConfig{BaseURL: base, Model: model, APIKey: os.Getenv("MEMORY_EMBEDDING_API_KEY")}})
	if e != nil {
		return nil, e
	}
	defer s.Close()
	accounts := []struct{ id, body string }{
		{"recording", "Το καλοκαίρι του 2008 πήγα με τη Δήμητρα στο στούντιο στην Αθήνα. Εκεί ηχογραφήσαμε ένα τραγούδι: εκείνη έπαιζε πιάνο και εγώ κιθάρα."},
		{"cooking", "Με την Ελένη μαγειρέψαμε φασολάδα στην κουζίνα της στη Θεσσαλονίκη. Βάλαμε φασόλια, καρότα, σέλινο και λίγο ελαιόλαδο."},
		{"travel", "In 2019 Eleni and I visited Crete. We took the overnight ferry and spent a week walking along the coast."},
		{"coffee", "I prefer my coffee plain, without sugar. I usually drink a cup after breakfast."},
		{"bicycle", "Dimitra helped me repair my bicycle. We replaced the broken chain and adjusted the brakes."},
		{"garden", "My grandfather planted lemon trees in the garden. We watered them every evening during the summer."},
	}
	for i, a := range accounts {
		if _, e = s.Create(ctx, memory.Record{ID: a.id, Title: fmt.Sprintf("Account %d", i+1), Body: a.body}); e != nil {
			return nil, e
		}
	}
	probes := []struct{ text, id string }{{"Playing musical instruments while recording a song in a studio", "recording"}, {"Ηχογράφηση τραγουδιού με κιθάρα και πιάνο", "recording"}, {"Preparing a bean soup with vegetables in a kitchen", "cooking"}, {"Προτιμήσεις για καφέ χωρίς ζάχαρη", "coffee"}, {"Ταξίδι στην Κρήτη με πλοίο", "travel"}, {"Fixing a damaged bike chain and brakes", "bicycle"}}
	out := []qualityResult{}
	for _, mode := range []string{"lexical", "semantic", "hybrid"} {
		for _, p := range probes {
			start := time.Now()
			r, e := s.Search(ctx, memory.Recall{Text: p.text, Mode: mode, Page: memory.Page{Limit: 3}})
			if e != nil {
				return nil, e
			}
			row := qualityResult{Query: p.text, Expected: p.id, Mode: mode, Milliseconds: elapsed(start), Degraded: r.Degraded, Retrieved: []string{}}
			for _, h := range r.Items {
				row.Retrieved = append(row.Retrieved, h.ID)
			}
			row.Hit1 = len(row.Retrieved) > 0 && row.Retrieved[0] == p.id
			out = append(out, row)
		}
	}
	return out, nil
}
