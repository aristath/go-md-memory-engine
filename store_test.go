package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func openTest(t *testing.T) *Store {
	t.Helper()
	s, e := Open(t.TempDir(), Options{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func mustCreate(t *testing.T, s *Store, r Record) Receipt {
	t.Helper()
	got, e := s.Create(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Warnings) > 0 {
		t.Fatal(got.Warnings)
	}
	return got
}
func mustGet(t *testing.T, s *Store, id string) Document {
	t.Helper()
	d, e := s.Get(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func rawRecord(t *testing.T, s *Store, path string, r Record) {
	t.Helper()
	if r.CreatedAt == "" {
		r.CreatedAt = "2020-01-01T00:00:00Z"
		r.UpdatedAt = r.CreatedAt
	}
	r.Format = 1
	b, e := Encode(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.path, path), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func requireIDs(t *testing.T, r Results, ids ...string) {
	t.Helper()
	got := []string{}
	for _, h := range r.Items {
		got = append(got, h.ID)
	}
	if fmt.Sprint(got) != fmt.Sprint(ids) {
		t.Fatalf("IDs %v want %v; result %+v", got, ids, r)
	}
}

func TestStorageAndVerbatimBody(t *testing.T) {
	s := openTest(t)
	body := "# Μνήμη\r\n\r\n  Η Δήμητρα είπε: «hello».\r\n```text\n---\n```\n\t  "
	receipt := mustCreate(t, s, Record{Title: "Session", Body: body, Taxonomies: map[string][]Value{" people ": {Text(" Dimitra ")}, "score": {Number(0)}, "enabled": {Boolean(false)}}, Sources: []map[string]any{{"chat": "abc", "message": 123}}})
	d := mustGet(t, s, receipt.ID)
	if d.Record.Body != body {
		t.Fatalf("body altered: %q", d.Record.Body)
	}
	if d.Record.Taxonomies["people"][0].Text != "Dimitra" {
		t.Fatal(d)
	}
	before, e := os.ReadFile(filepath.Join(s.path, d.Path))
	if e != nil {
		t.Fatal(e)
	}
	d.Record.Title = "Corrected title"
	updated, e := s.Update(ctx, d.Record, d.Hash)
	if e != nil || len(updated.Warnings) > 0 {
		t.Fatalf("%+v %v", updated, e)
	}
	after := mustGet(t, s, d.Record.ID)
	if after.Record.Body != body || after.Record.CreatedAt != d.Record.CreatedAt {
		t.Fatal("metadata update changed account or creation date")
	}
	if _, e = s.Update(ctx, d.Record, d.Hash); !errors.Is(e, ErrConflict) {
		t.Fatal("stale update accepted", e)
	}
	if _, e = s.Delete(ctx, d.Record.ID, d.Hash); !errors.Is(e, ErrConflict) {
		t.Fatal("stale delete accepted", e)
	}
	check := mustGet(t, s, d.Record.ID)
	if check.Hash == digest(before) {
		t.Fatal("update did not replace content")
	}
	if _, e = s.Delete(ctx, d.Record.ID, check.Hash); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(ctx, d.Record.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}
func TestTaxonomyEvolutionAndDiscovery(t *testing.T) {
	s := openTest(t)
	a := mustCreate(t, s, Record{ID: "a", Title: "A", Taxonomies: map[string][]Value{"people": {Text("Dimitra"), Text("Dimitra")}, "places": {Text("Athens")}}})
	v, e := s.Taxonomies(ctx)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "Dimitra") || strings.Contains(string(b), "Athens") {
		t.Fatal("vocabulary dumped entries", string(b))
	}
	mustCreate(t, s, Record{ID: "b", Title: "B", Taxonomies: map[string][]Value{"people": {Text("Eleni"), Text("Dimitra")}, "unanticipated taxonomy": {Text("new entry")}}})
	v2, _ := s.Taxonomies(ctx)
	if v.Revision == v2.Revision {
		t.Fatal("revision did not change")
	}
	entries, e := s.Entries(ctx, "people", "", Page{Limit: 1})
	if e != nil || entries.Total != 2 || entries.NextOffset == nil || entries.Items[0].Value.Text != "Dimitra" || entries.Items[0].Count != 2 {
		t.Fatalf("%+v %v", entries, e)
	}
	found, e := s.Entries(ctx, "people", "eleni", Page{})
	if e != nil || found.Total != 1 {
		t.Fatal(found, e)
	}
	if found, e := s.Entries(ctx, "people", "text", Page{}); e != nil || found.Total != 0 {
		t.Fatal("entry search matched the encoding rather than the value", found, e)
	}
	d := mustGet(t, s, "b")
	delete(d.Record.Taxonomies, "unanticipated taxonomy")
	if _, e = s.Update(ctx, d.Record, d.Hash); e != nil {
		t.Fatal(e)
	}
	v3, _ := s.Taxonomies(ctx)
	if v3.Revision != v.Revision {
		t.Fatal("names-only revision changed despite same vocabulary")
	}
	if _, e = s.Delete(ctx, "a", a.Hash); e != nil {
		t.Fatal(e)
	}
	v4, _ := s.Taxonomies(ctx)
	if fmt.Sprint(v4.Names) != "[people]" {
		t.Fatal(v4)
	}
}
func TestExternalEditsRenameAndNoReparse(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, Record{ID: "a", Title: "Original", Body: "old"})
	report, e := s.Sync(ctx, false)
	if e != nil || report.Parsed != 0 || report.Indexed != 0 {
		t.Fatal(report, e)
	}
	d := mustGet(t, s, "a")
	path := filepath.Join(s.path, d.Path)
	info, _ := os.Stat(path)
	rawRecord(t, s, d.Path, Record{ID: "a", Title: "Modified", Body: "new", Taxonomies: map[string][]Value{"new-category": {Text("new")}}})
	// Restoring mtime must not hide a modification (ctime/identity also checked).
	if e = os.Chtimes(path, info.ModTime(), info.ModTime()); e != nil {
		t.Fatal(e)
	}
	report, e = s.Sync(ctx, false)
	if e != nil || report.Parsed != 1 {
		t.Fatal(report, e)
	}
	v, _ := s.Taxonomies(ctx)
	if fmt.Sprint(v.Names) != "[new-category]" {
		t.Fatal(v)
	}
	if e = os.Mkdir(filepath.Join(s.path, "elsewhere"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(path, filepath.Join(s.path, "elsewhere", "different-name.md")); e != nil {
		t.Fatal(e)
	}
	renamed := mustGet(t, s, "a")
	if renamed.Path != "elsewhere/different-name.md" || renamed.Record.Body != "new" {
		t.Fatal(renamed)
	}
	if _, e = s.Update(ctx, d.Record, d.Hash); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(s.path, renamed.Path)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Get(ctx, "a"); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}
func TestMalformedDuplicatesAndReferences(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, Record{ID: "a", Title: "A", Links: []Link{{"related", "missing"}}})
	d := mustGet(t, s, "a")
	data, _ := os.ReadFile(filepath.Join(s.path, d.Path))
	if e := os.WriteFile(filepath.Join(s.path, "duplicate.md"), data, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(s.path, "bad.md"), []byte("not frontmatter"), 0600); e != nil {
		t.Fatal(e)
	}
	report, e := s.Sync(ctx, true)
	if e != nil {
		t.Fatal(e)
	}
	counts := map[string]int{}
	for _, x := range report.Diagnostics {
		counts[x.Code]++
	}
	if counts["duplicate_id"] != 2 || counts["invalid"] != 1 {
		t.Fatal(report)
	}
	results, e := s.Query(ctx, Query{})
	if e != nil || results.Total != 0 || len(results.Diagnostics) != 3 {
		t.Fatal(results, e)
	}
	if _, e = s.Create(ctx, Record{ID: "a", Title: "Overwrite duplicate"}); e == nil {
		t.Fatal("duplicate ID overwritten")
	}
	os.Remove(filepath.Join(s.path, "duplicate.md"))
	report, e = s.Sync(ctx, false)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, x := range report.Diagnostics {
		if x.Code == "broken_reference" {
			found = true
		}
	}
	if !found {
		t.Fatal(report)
	}
}
func TestRebuildAndCopy(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, Record{ID: "a", Title: "Athens", Body: "The recording studio", Taxonomies: map[string][]Value{"people": {Reference("dimitra")}}, Sources: []map[string]any{{"source": "original-chat"}}})
	mustCreate(t, s, Record{ID: "dimitra", Kind: "person", Title: "Dimitra"})
	target := openTest(t)
	for _, id := range []string{"a", "dimitra"} {
		d := mustGet(t, s, id)
		b, _ := os.ReadFile(filepath.Join(s.path, d.Path))
		if e := os.WriteFile(filepath.Join(target.path, id+".md"), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	for _, store := range []*Store{s, target} {
		if store == s {
			if e := os.RemoveAll(filepath.Join(s.path, ".memory", "index")); e != nil {
				t.Fatal(e)
			}
		}
		report, e := store.Rebuild(ctx)
		if e != nil || report.Indexed != 2 {
			t.Fatal(report, e)
		}
		r, e := store.Search(ctx, Recall{Text: "recording", Mode: "lexical"})
		if e != nil {
			t.Fatal(e)
		}
		requireIDs(t, r, "a")
		d := mustGet(t, store, "a")
		if d.Record.Sources[0]["source"] != "original-chat" {
			t.Fatal(d)
		}
	}
}
func TestConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, e := Open(dir, Options{})
			if e != nil {
				errs <- e
				return
			}
			defer s.Close()
			_, e = s.Create(ctx, Record{ID: fmt.Sprintf("m%d", i), Title: "Concurrent"})
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	s, e := Open(dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	stats, e := s.Stats(ctx)
	if e != nil || stats.Records != 8 {
		t.Fatal(stats, e)
	}
	d := mustGet(t, s, "m0")
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			other, e := Open(dir, Options{})
			if e != nil {
				results <- e
				return
			}
			defer other.Close()
			r := d.Record
			r.Body = fmt.Sprint(i)
			_, e = other.Update(ctx, r, d.Hash)
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal(success, conflicts)
	}
}
func TestBoundaries(t *testing.T) {
	s := openTest(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if e := os.WriteFile(secret, []byte("private"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(s.path, "escape")); e != nil {
		t.Fatal(e)
	}
	report, e := s.Sync(ctx, false)
	if e != nil || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "symlink" {
		t.Fatal(report, e)
	}
	for _, id := range []string{"../secret", "/absolute", "a/b", ".."} {
		if _, e = s.Create(ctx, Record{ID: id, Title: "Bad"}); e == nil {
			t.Fatal("accepted ID", id)
		}
	}
	if e = s.safePath("escape/secret.md", false); e == nil {
		t.Fatal("accepted symlink")
	}
	os.RemoveAll(filepath.Join(s.path, "records"))
	if e = os.Symlink(outside, filepath.Join(s.path, "records")); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Create(ctx, Record{ID: "outside", Title: "Bad"}); e == nil {
		t.Fatal("wrote through symlink")
	}
	b, _ := os.ReadFile(secret)
	if string(b) != "private" {
		t.Fatal("outside modified")
	}
	other := openTest(t)
	if _, e = other.Get(ctx, "outside"); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	// Reject links in the index tree too, before calling a path-based library.
	os.Remove(filepath.Join(s.path, "records"))
	if e = os.Symlink(secret, filepath.Join(s.path, ".memory", "state-link")); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Stats(ctx); e == nil {
		t.Fatal("cache symlink accepted")
	}
}
func TestPublicationConflictLeavesOriginal(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, Record{ID: "a", Title: "A"})
	d := mustGet(t, s, "a")
	b, _ := os.ReadFile(filepath.Join(s.path, d.Path))
	e := s.atomic(d.Path, []byte("bad"), func() error { return ErrConflict }, false)
	if !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(filepath.Join(s.path, d.Path))
	if string(b) != string(after) {
		t.Fatal("conflict replaced file")
	}
	if e = s.atomic(d.Path, []byte("bad"), nil, true); !errors.Is(e, os.ErrExist) {
		t.Fatal("exclusive create overwrote", e)
	}
}
func TestCrashPublicationRecovery(t *testing.T) {
	if path := os.Getenv("MEMORY_CRASH_ROOT"); path != "" {
		s, e := Open(path, Options{})
		if e != nil {
			os.Exit(20)
		}
		_ = s.with(ctx, false, func(tx *session) error {
			r := Record{Format: 1, ID: "after-crash", Title: "Durable", CreatedAt: "2020-01-01T00:00:00Z", UpdatedAt: "2020-01-01T00:00:00Z", Body: "published before indexing"}
			b, e := Encode(r)
			if e != nil {
				os.Exit(21)
			}
			if e = s.dirty(); e != nil {
				os.Exit(22)
			}
			if e = s.atomic("crash.md", b, nil, true); e != nil {
				os.Exit(23)
			}
			os.Exit(42)
			return nil
		})
		os.Exit(24)
	}
	s := openTest(t)
	mustCreate(t, s, Record{ID: "before", Title: "Before"})
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashPublicationRecovery$")
	cmd.Env = append(os.Environ(), "MEMORY_CRASH_ROOT="+s.path)
	e := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 42 {
		t.Fatal("child did not reach publication", e)
	}
	d := mustGet(t, s, "after-crash")
	if d.Record.Body != "published before indexing" {
		t.Fatal(d)
	}
	r, e := s.Search(ctx, Recall{Text: "published"})
	if e != nil {
		t.Fatal(e)
	}
	requireIDs(t, r, "after-crash")
	if _, e = os.Stat(filepath.Join(s.path, ".memory", "dirty")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("dirty marker not cleared", e)
	}
}
func TestContextCancellation(t *testing.T) {
	s := openTest(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := s.Sync(c, false); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestFilePermissions(t *testing.T) {
	s := openTest(t)
	r := mustCreate(t, s, Record{Title: "Private"})
	for _, name := range []string{r.Path, ".memory/state.json", ".memory/lock"} {
		i, e := os.Stat(filepath.Join(s.path, name))
		if e != nil || i.Mode().Perm() != 0600 {
			t.Fatal(name, i, e)
		}
	}
	for _, name := range []string{"records", ".memory"} {
		i, e := os.Stat(filepath.Join(s.path, name))
		if e != nil || i.Mode().Perm() != 0700 {
			t.Fatal(name, i, e)
		}
	}
}
func TestInvalidInputs(t *testing.T) {
	s := openTest(t)
	for _, r := range []Record{{Title: ""}, {Title: "A", Taxonomies: map[string][]Value{"a": {{Type: "number"}}}}, {Title: "A", Taxonomies: map[string][]Value{"a": {Text("x")}, " a ": {Text("y")}}}, {Title: "A", CreatedAt: "yesterday"}} {
		if _, e := s.Create(ctx, r); e == nil {
			t.Fatal("invalid record accepted", r)
		}
	}
	stats, e := s.Stats(ctx)
	if e != nil || stats.Records != 0 {
		t.Fatal(stats, e)
	}
}
func TestLockCancellation(t *testing.T) {
	s := openTest(t)
	if _, e := s.Sync(ctx, false); e != nil {
		t.Fatal(e)
	}
	entered := make(chan bool)
	release := make(chan bool)
	go func() { _ = s.with(ctx, false, func(tx *session) error { entered <- true; <-release; return nil }) }()
	<-entered
	other, e := Open(s.path, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	c, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, e = other.Stats(c)
	close(release)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}

func TestMetadataPatchWithoutBodyRoundTrip(t *testing.T) {
	s := openTest(t)
	body := strings.Repeat("Αρχική μαρτυρία\r\n", 1000)
	saved := mustCreate(t, s, Record{ID: "a", Title: "A", Body: body, Taxonomies: map[string][]Value{"people": {Text("Dimitra")}, "remove": {Text("old")}}})
	receipt, e := s.Patch(ctx, "a", saved.Hash, map[string]any{"title": "Updated", "taxonomies": map[string]any{"places": []Value{Text("Athens")}, "remove": nil}})
	if e != nil {
		t.Fatal(e)
	}
	d := mustGet(t, s, "a")
	if d.Record.Body != body || d.Record.Title != "Updated" || len(d.Record.Taxonomies["people"]) != 1 || len(d.Record.Taxonomies["places"]) != 1 {
		t.Fatal(d)
	}
	if _, ok := d.Record.Taxonomies["remove"]; ok {
		t.Fatal("null did not remove taxonomy")
	}
	if _, e = s.Patch(ctx, "a", saved.Hash, map[string]any{"title": "Stale"}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, e = s.Patch(ctx, "a", receipt.Hash, map[string]any{"id": "new"}); e == nil {
		t.Fatal("patched identity")
	}
	if _, e = s.Patch(ctx, "a", receipt.Hash, map[string]any{"taxonomies": map[string]any{"x": []any{map[string]any{"type": "text", "text": "A", "unknown_field": "must not disappear"}}}}); e == nil {
		t.Fatal("patch silently discarded unknown value fields")
	}
	if _, e = s.Patch(ctx, "a", receipt.Hash, map[string]any{"body": ""}); e != nil {
		t.Fatal(e)
	}
	if d := mustGet(t, s, "a"); d.Record.Body != "" {
		t.Fatal("explicit empty body ignored")
	}
}
