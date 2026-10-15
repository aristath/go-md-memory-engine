package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blevesearch/bleve/v2"
)

type Options struct{ Embedding *EmbeddingConfig }
type Store struct {
	root    *os.Root
	path    string
	options Options
	mu      sync.Mutex
}
type cachedFile struct {
	Stamp  string  `json:"stamp"`
	Hash   string  `json:"hash"`
	Record *Record `json:"record,omitempty"`
	Error  string  `json:"error,omitempty"`
}
type state struct {
	Version     int                   `json:"version"`
	Files       map[string]cachedFile `json:"files"`
	Vectors     map[string]vector     `json:"-"`
	Docs        map[string]Document   `json:"-"`
	Diagnostics []Diagnostic          `json:"-"`
}
type session struct {
	s      *Store
	state  *state
	index  bleve.Index
	report SyncReport
}

// Open creates a private collection if absent. No model calls occur on Open.
// The current platform target is Linux; cooperating readers/writers use flock.
func Open(path string, options Options) (*Store, error) {
	if options.Embedding != nil {
		c := *options.Embedding
		options.Embedding = &c
	}
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(abs, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(abs)
	if e != nil {
		return nil, e
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("collection root must be a directory, not a symlink")
	}
	root, e := os.OpenRoot(abs)
	if e != nil {
		return nil, e
	}
	s := &Store{root: root, path: abs, options: options}
	if e = s.safePath(".memory", true); e != nil {
		root.Close()
		return nil, e
	}
	if e = root.MkdirAll(".memory", 0700); e != nil {
		root.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.root.Close() }

func (s *Store) safePath(path string, missing bool) error {
	if !fs.ValidPath(filepath.ToSlash(path)) || path == "." {
		return fmt.Errorf("invalid collection-relative path %q", path)
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		info, e := s.root.Lstat(p)
		if errors.Is(e, fs.ErrNotExist) && missing {
			return nil
		}
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink refused: %s", p)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("not a directory: %s", p)
		}
	}
	return nil
}
func (s *Store) checkCache() error {
	return fs.WalkDir(s.root.FS(), ".memory", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("cache symlink refused: %s", p)
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("nonregular cache entry: %s", p)
		}
		return nil
	})
}
func stamp(info fs.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if ok {
		return fmt.Sprintf("%d:%d:%d:%d:%d:%d", stat.Dev, stat.Ino, info.Size(), info.ModTime().UnixNano(), stat.Ctim.Sec, stat.Ctim.Nsec)
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}
func (s *Store) syncDir(path string) error {
	f, e := s.root.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) atomic(path string, b []byte, check func() error, exclusive bool) error {
	if e := s.safePath(path, true); e != nil {
		return e
	}
	dir := filepath.ToSlash(filepath.Dir(path))
	id, e := newID()
	if e != nil {
		return e
	}
	tmp := dir + "/.tmp-" + id
	f, e := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer s.root.Remove(tmp)
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if check != nil {
		if e = check(); e != nil {
			return e
		}
	}
	if e = s.safePath(path, true); e != nil {
		return e
	}
	if exclusive {
		e = s.root.Link(tmp, path)
	} else {
		e = s.root.Rename(tmp, path)
	}
	if e != nil {
		return e
	}
	return s.syncDir(dir)
}
func (s *Store) dirty() error {
	return s.atomic(".memory/dirty", []byte("rebuild derived state\n"), nil, false)
}

func (s *Store) with(ctx context.Context, force bool, fn func(*session) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.safePath(".memory/lock", true); e != nil {
		return e
	}
	lock, e := s.root.OpenFile(".memory/lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	for {
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Inspect the path-based index only while cooperating processes are idle.
	if e = s.checkCache(); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	st := &state{Version: 1, Files: map[string]cachedFile{}, Vectors: map[string]vector{}}
	b, readErr := s.root.ReadFile(".memory/state.json")
	if readErr == nil {
		if e = decodeJSON(b, st); e != nil || st.Version != 1 || st.Files == nil {
			force = true
			st = &state{Version: 1, Files: map[string]cachedFile{}, Vectors: map[string]vector{}}
		}
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}
	if st.Vectors == nil {
		st.Vectors = map[string]vector{}
	}
	for _, f := range st.Files {
		if f.Record != nil {
			if f.Record.validate() != nil {
				force = true
				break
			}
		} else if f.Error == "" {
			force = true
			break
		}
	}
	if force {
		st.Vectors = map[string]vector{}
		if e = s.root.Remove(".memory/vectors.gob"); e != nil && !errors.Is(e, fs.ErrNotExist) {
			return e
		}
	}
	if _, e = s.root.Stat(".memory/dirty"); e == nil {
		force = true
	} else if !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	tx := &session{s: s, state: st}
	defer func() {
		if tx.index != nil {
			tx.index.Close()
		}
	}()
	if e = tx.sync(ctx, force, false); e != nil {
		return e
	}
	if e = fn(tx); e != nil && len(tx.state.Diagnostics) > 0 {
		return &CollectionError{Err: e, Diagnostics: tx.state.Diagnostics}
	}
	return e
}
func (tx *session) persist() error {
	b, e := json.Marshal(tx.state)
	if e != nil {
		return e
	}
	if e = tx.s.atomic(".memory/state.json", b, nil, false); e != nil {
		return e
	}
	if e = tx.s.root.Remove(".memory/dirty"); e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	return tx.s.syncDir(".memory")
}

func (tx *session) sync(ctx context.Context, force, verify bool) error {
	s, st := tx.s, tx.state
	old := st.Files
	files := map[string]cachedFile{}
	report := SyncReport{}
	err := fs.WalkDir(s.root.FS(), ".", func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{p, "io", e.Error()})
			return nil
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if p == ".memory" {
			return fs.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{p, "symlink", "symlinks are excluded; replace with a regular file or directory"})
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		report.Files++
		info, e := s.root.Lstat(p)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{p, "file_type", "expected regular Markdown file"})
			return nil
		}
		fp := stamp(info)
		cached, exists := old[p]
		if exists && cached.Stamp == fp && !verify && !force {
			files[p] = cached
			return nil
		}
		b, e := s.root.ReadFile(p)
		if e != nil {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{p, "io", e.Error()})
			return nil
		}
		after, e := s.root.Lstat(p)
		if e != nil {
			return e
		}
		if stamp(after) != fp {
			return fmt.Errorf("file changed during synchronization: %s", p)
		}
		h := digest(b)
		if exists && cached.Hash == h && !force {
			cached.Stamp = fp
			files[p] = cached
			return nil
		}
		report.Parsed++
		f := cachedFile{Stamp: fp, Hash: h}
		r, e := Parse(b)
		if e != nil {
			f.Error = e.Error()
		} else {
			f.Record = &r
		}
		files[p] = f
		return nil
	})
	if err != nil {
		return err
	}
	docs := map[string]Document{}
	paths := map[string][]string{}
	for p, f := range files {
		if f.Error != "" {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{p, "invalid", f.Error})
		}
		if f.Record != nil {
			paths[f.Record.ID] = append(paths[f.Record.ID], p)
		}
	}
	for id, ps := range paths {
		sort.Strings(ps)
		if len(ps) > 1 {
			for _, p := range ps {
				report.Diagnostics = append(report.Diagnostics, Diagnostic{p, "duplicate_id", fmt.Sprintf("ID %s occurs in %v; all copies excluded", id, ps)})
			}
			continue
		}
		p := ps[0]
		docs[id] = Document{Record: *files[p].Record, Path: p, Hash: files[p].Hash}
	}
	for _, d := range docs {
		for _, l := range allLinks(d.Record) {
			if _, ok := docs[l.Target]; !ok {
				report.Diagnostics = append(report.Diagnostics, Diagnostic{d.Path, "broken_reference", fmt.Sprintf("%s -> %s is missing or invalid", l.Relation, l.Target)})
			}
		}
	}
	sort.Slice(report.Diagnostics, func(i, j int) bool {
		a, b := report.Diagnostics[i], report.Diagnostics[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
	for p := range old {
		if _, ok := files[p]; !ok {
			report.Removed++
		}
	}
	idxPath := filepath.Join(s.path, ".memory", "index")
	if tx.index == nil && !force {
		tx.index, err = bleve.Open(idxPath)
		if err != nil {
			tx.index = nil
			force = true
		}
	}
	if force {
		if tx.index != nil {
			tx.index.Close()
			tx.index = nil
		}
		if err = s.dirty(); err != nil {
			return err
		}
		if err = s.root.RemoveAll(".memory/index"); err != nil {
			return err
		}
		im, mappingErr := indexMapping()
		if mappingErr != nil {
			return mappingErr
		}
		tx.index, err = bleve.New(idxPath, im)
		if err != nil {
			tx.index = nil
			return err
		}
	}
	changed := force || report.Parsed > 0 || report.Removed > 0 || len(old) != len(files)
	if !changed {
		for p, f := range files {
			if f.Stamp != old[p].Stamp {
				changed = true
				break
			}
		}
	}
	if changed {
		if err = s.dirty(); err != nil {
			return err
		}
		batch := tx.index.NewBatch()
		if !force {
			for id := range oldDocuments(old) {
				if _, ok := docs[id]; !ok {
					batch.Delete(id)
				}
			}
		}
		previous := oldDocuments(old)
		for id, d := range docs {
			before, ok := previous[id]
			if force || !ok || before.Hash != d.Hash || before.Path != d.Path {
				if err = batch.Index(id, indexRecord(d.Record)); err != nil {
					return err
				}
				report.Indexed++
			}
		}
		if err = tx.index.Batch(batch); err != nil {
			return err
		}
	}
	st.Files = files
	st.Docs = docs
	st.Diagnostics = report.Diagnostics
	tx.report = report
	if changed {
		return tx.persist()
	}
	return nil
}
func oldDocuments(files map[string]cachedFile) map[string]Document {
	out := map[string]Document{}
	counts := map[string]int{}
	for p, f := range files {
		if f.Record != nil {
			counts[f.Record.ID]++
			out[f.Record.ID] = Document{Record: *f.Record, Path: p, Hash: f.Hash}
		}
	}
	for id, n := range counts {
		if n > 1 {
			delete(out, id)
		}
	}
	return out
}

// Sync reconciles external changes. verify additionally hashes unchanged files.
// Invalid files are returned as diagnostics, not silently repaired.
func (s *Store) Sync(ctx context.Context, verify bool) (SyncReport, error) {
	var out SyncReport
	e := s.with(ctx, false, func(tx *session) error {
		if verify {
			first := tx.report
			if e := tx.sync(ctx, false, true); e != nil {
				return e
			}
			tx.report.Parsed += first.Parsed
			tx.report.Indexed += first.Indexed
			tx.report.Removed += first.Removed
		}
		out = tx.report
		return nil
	})
	return out, e
}

// Rebuild reparses all Markdown and reconstructs indexes, discarding vectors.
func (s *Store) Rebuild(ctx context.Context) (SyncReport, error) {
	var out SyncReport
	e := s.with(ctx, true, func(tx *session) error { out = tx.report; return nil })
	return out, e
}

// Get reads a complete record and its current content hash.
func (s *Store) Get(ctx context.Context, id string) (Document, error) {
	var d Document
	e := s.with(ctx, false, func(tx *session) error {
		var e error
		d, e = tx.read(id)
		return e
	})
	return d, e
}

func (tx *session) read(id string) (Document, error) {
	d, ok := tx.state.Docs[id]
	if !ok {
		return d, ErrNotFound
	}
	if e := tx.s.safePath(d.Path, false); e != nil {
		return d, e
	}
	b, e := tx.s.root.ReadFile(d.Path)
	if e != nil {
		return d, e
	}
	if digest(b) != d.Hash {
		return d, fmt.Errorf("record changed during read: %w", ErrConflict)
	}
	d.Record, e = Parse(b)
	d.Diagnostics = tx.state.Diagnostics
	return d, e
}

type BatchResult struct {
	Items       []Document   `json:"items"`
	Missing     []string     `json:"missing"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// GetMany reads complete records in requested order and lists missing IDs.
func (s *Store) GetMany(ctx context.Context, ids []string) (BatchResult, error) {
	out := BatchResult{Items: []Document{}, Missing: []string{}}
	e := s.with(ctx, false, func(tx *session) error {
		for _, id := range ids {
			if _, ok := tx.state.Docs[id]; !ok {
				out.Missing = append(out.Missing, id)
			} else {
				d, e := tx.read(id)
				if e != nil {
					return e
				}
				d.Diagnostics = nil // Batch diagnostics are returned once, below.
				out.Items = append(out.Items, d)
			}
		}
		out.Diagnostics = tx.state.Diagnostics
		return nil
	})
	return out, e
}

func cloneRecord(r Record) (Record, error) {
	b, e := json.Marshal(r)
	if e != nil {
		return Record{}, e
	}
	var c Record
	e = decodeJSON(b, &c)
	return c, e
}

// Create publishes a new record, generating an ID and timestamps if absent.
// An existing ID is never overwritten. Inspect receipt warnings after success.
func (s *Store) Create(ctx context.Context, r Record) (Receipt, error) {
	return s.write(ctx, r, "", true)
}

// Update replaces a record after checking expectedHash from a previous read.
// Omitted metadata is removed; creation time is preserved. No history is retained.
func (s *Store) Update(ctx context.Context, r Record, expectedHash string) (Receipt, error) {
	return s.write(ctx, r, expectedHash, false)
}

// Patch applies a JSON Merge Patch to mutable fields. It lets a caller update
// metadata using a compact hit's hash without reading/transmitting the body.
// Objects merge, arrays replace, null removes. Identity/timestamps are immutable.
func (s *Store) Patch(ctx context.Context, id, expectedHash string, changes map[string]any) (Receipt, error) {
	for key := range changes {
		switch key {
		case "title", "kind", "taxonomies", "links", "sources", "extra", "body":
		default:
			return Receipt{}, fmt.Errorf("field %q cannot be patched", key)
		}
	}
	d, e := s.Get(ctx, id)
	if e != nil {
		return Receipt{}, e
	}
	if expectedHash == "" || d.Hash != expectedHash {
		return Receipt{}, ErrConflict
	}
	b, e := json.Marshal(d.Record)
	if e != nil {
		return Receipt{}, e
	}
	var object map[string]any
	if e = decodeJSON(b, &object); e != nil {
		return Receipt{}, e
	}
	// Clone/normalize Go input into ordinary JSON objects without mutating it.
	b, e = json.Marshal(changes)
	if e != nil {
		return Receipt{}, e
	}
	var patch map[string]any
	if e = decodeJSON(b, &patch); e != nil {
		return Receipt{}, e
	}
	mergeObject(object, patch)
	b, e = json.Marshal(object)
	if e != nil {
		return Receipt{}, e
	}
	var r Record
	if e = decodeJSON(b, &r); e != nil {
		return Receipt{}, e
	}
	return s.Update(ctx, r, expectedHash)
}
func mergeObject(dst, patch map[string]any) {
	for key, value := range patch {
		if value == nil {
			delete(dst, key)
			continue
		}
		if object, ok := value.(map[string]any); ok {
			current, ok := dst[key].(map[string]any)
			if !ok {
				current = map[string]any{}
			}
			mergeObject(current, object)
			dst[key] = current
		} else {
			dst[key] = value
		}
	}
}
func (s *Store) write(ctx context.Context, input Record, expected string, create bool) (Receipt, error) {
	var receipt Receipt
	if input.Format != 0 && input.Format != 1 {
		return receipt, errors.New("format must be 1")
	}
	r, e := cloneRecord(input)
	if e != nil {
		return receipt, e
	}
	e = s.with(ctx, false, func(tx *session) error {
		var current Document
		if create {
			if r.ID == "" {
				var e error
				r.ID, e = newID()
				if e != nil {
					return e
				}
			}
			for _, f := range tx.state.Files {
				if f.Record != nil && f.Record.ID == r.ID {
					return fmt.Errorf("ID already exists: %s", r.ID)
				}
			}
		} else {
			var ok bool
			current, ok = tx.state.Docs[r.ID]
			if !ok {
				return ErrNotFound
			}
			if expected == "" || expected != current.Hash {
				return ErrConflict
			}
			r.CreatedAt = current.Record.CreatedAt
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if r.CreatedAt == "" {
			r.CreatedAt = now
		}
		r.UpdatedAt = now
		r.Format = 1
		b, e := Encode(r)
		if e != nil {
			return e
		}
		if _, e = Parse(b); e != nil {
			return e
		}
		path := current.Path
		if create {
			if e = s.safePath("records", true); e != nil {
				return e
			}
			if e = s.root.MkdirAll("records", 0700); e != nil {
				return e
			}
			if e = s.syncDir("."); e != nil {
				return e
			}
			path = "records/" + r.ID + ".md"
		}
		if e = s.dirty(); e != nil {
			return e
		}
		var check func() error
		if !create {
			check = func() error {
				if e := s.safePath(path, false); e != nil {
					return e
				}
				old, e := s.root.ReadFile(path)
				if e != nil {
					return e
				}
				if digest(old) != expected {
					return ErrConflict
				}
				return nil
			}
		}
		if e = s.atomic(path, b, check, create); e != nil {
			return fmt.Errorf("publication of %s at %s failed; inspect this ID before retrying: %w", r.ID, path, e)
		}
		receipt = Receipt{ID: r.ID, Path: path, Hash: digest(b)}
		if e = tx.sync(ctx, false, false); e != nil {
			receipt.Warnings = append(receipt.Warnings, "record is durable; derived index needs repair: "+e.Error())
		}
		receipt.Diagnostics = tx.state.Diagnostics
		return nil
	})
	return receipt, e
}

// Delete removes a record only if its current content matches expected.
func (s *Store) Delete(ctx context.Context, id, expected string) (Receipt, error) {
	var receipt Receipt
	e := s.with(ctx, false, func(tx *session) error {
		d, ok := tx.state.Docs[id]
		if !ok {
			return ErrNotFound
		}
		if expected == "" || expected != d.Hash {
			return ErrConflict
		}
		if e := s.safePath(d.Path, false); e != nil {
			return e
		}
		b, e := s.root.ReadFile(d.Path)
		if e != nil {
			return e
		}
		if digest(b) != expected {
			return ErrConflict
		}
		if e = s.dirty(); e != nil {
			return e
		}
		if e = s.root.Remove(d.Path); e != nil {
			return e
		}
		if e = s.syncDir(filepath.Dir(d.Path)); e != nil {
			return e
		}
		receipt = Receipt{ID: id, Path: d.Path}
		if e = tx.sync(ctx, false, false); e != nil {
			receipt.Warnings = []string{"delete is durable; derived index needs repair: " + e.Error()}
		}
		receipt.Diagnostics = tx.state.Diagnostics
		return nil
	})
	return receipt, e
}

// Stats reports current collection counts and validation diagnostics.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	e := s.with(ctx, false, func(tx *session) error {
		names := taxonomyNames(tx.state.Docs)
		out = Stats{Records: len(tx.state.Docs), Files: len(tx.state.Files), Taxonomies: len(names), Diagnostics: tx.state.Diagnostics}
		for _, d := range tx.state.Docs {
			out.Links += len(allLinks(d.Record))
		}
		return nil
	})
	return out, e
}
