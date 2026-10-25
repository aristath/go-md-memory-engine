// Command memory exposes the memory package through JSON stdin/stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	memory "github.com/aristath/go-md-memory-engine"
)

func main() { os.Exit(run()) }
func run() int {
	flags := flag.NewFlagSet("memory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "collection directory (required)")
	base := flags.String("embedding-base", os.Getenv("MEMORY_EMBEDDING_BASE"), "OpenAI-compatible base URL, including /v1")
	model := flags.String("embedding-model", os.Getenv("MEMORY_EMBEDDING_MODEL"), "embedding model")
	dims := flags.Int("embedding-dimensions", 0, "requested embedding dimensions; 0 uses provider default")
	batch := flags.Int("embedding-batch-size", 0, "inputs per embedding request; 0 uses 16")
	chunk := flags.Int("embedding-chunk-runes", 0, "embedding chunk size; 0 uses 2000")
	timeout := flags.Int("embedding-timeout", 0, "embedding HTTP timeout in seconds; 0 uses 60")
	usage := func() {
		flags.SetOutput(os.Stderr)
		fmt.Fprintln(os.Stderr, "Usage: memory -root DIRECTORY [embedding flags] COMMAND < input.json\nCommands: create get get-many update patch delete query search search-many embed taxonomies entries links traverse sync check rebuild stats\nAll successful output is JSON. See README.md for input shapes.")
		flags.PrintDefaults()
	}
	flags.Usage = func() {}
	if e := flags.Parse(os.Args[1:]); e != nil {
		if e == flag.ErrHelp {
			usage()
			return 0
		}
		return failure(e)
	}
	if *root == "" || flags.NArg() != 1 {
		return failure(errors.New("root and one command are required; use -help for usage"))
	}
	options := memory.Options{}
	if *base != "" || *model != "" {
		options.Embedding = &memory.EmbeddingConfig{BaseURL: *base, Model: *model, APIKey: os.Getenv("MEMORY_EMBEDDING_API_KEY"), Dimensions: *dims, BatchSize: *batch, ChunkRunes: *chunk, TimeoutSeconds: *timeout}
	}
	store, e := memory.Open(*root, options)
	if e != nil {
		return failure(e)
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	decode := func(v any) error {
		d := json.NewDecoder(os.Stdin)
		d.UseNumber()
		d.DisallowUnknownFields()
		if e := d.Decode(v); e != nil {
			return fmt.Errorf("input JSON: %w", e)
		}
		if e := d.Decode(new(any)); e != io.EOF {
			return errors.New("input must contain exactly one JSON object")
		}
		return nil
	}
	var result any
	switch flags.Arg(0) {
	case "create":
		var r memory.Record
		if e = decode(&r); e == nil {
			result, e = store.Create(ctx, r)
		}
	case "update":
		var r struct {
			Record       memory.Record `json:"record"`
			ExpectedHash string        `json:"expected_hash"`
		}
		if e = decode(&r); e == nil {
			result, e = store.Update(ctx, r.Record, r.ExpectedHash)
		}
	case "get", "delete":
		var r struct {
			ID           string `json:"id"`
			ExpectedHash string `json:"expected_hash,omitempty"`
		}
		if e = decode(&r); e == nil {
			if flags.Arg(0) == "get" {
				result, e = store.Get(ctx, r.ID)
			} else {
				result, e = store.Delete(ctx, r.ID, r.ExpectedHash)
			}
		}
	case "patch":
		var r struct {
			ID           string         `json:"id"`
			ExpectedHash string         `json:"expected_hash"`
			Changes      map[string]any `json:"changes"`
		}
		if e = decode(&r); e == nil {
			result, e = store.Patch(ctx, r.ID, r.ExpectedHash, r.Changes)
		}
	case "get-many":
		var r struct {
			IDs []string `json:"ids"`
		}
		if e = decode(&r); e == nil {
			result, e = store.GetMany(ctx, r.IDs)
		}
	case "query":
		var r memory.Query
		if e = decode(&r); e == nil {
			result, e = store.Query(ctx, r)
		}
	case "search":
		var r memory.Recall
		if e = decode(&r); e == nil {
			result, e = store.Search(ctx, r)
		}
	case "search-many", "embed":
		var r memory.SearchBatch
		if e = decode(&r); e == nil {
			result, e = store.SearchMany(ctx, r)
		}
	case "taxonomies":
		result, e = store.Taxonomies(ctx)
	case "entries":
		var r struct {
			Name   string `json:"name"`
			Search string `json:"search,omitempty"`
			memory.Page
		}
		if e = decode(&r); e == nil {
			result, e = store.Entries(ctx, r.Name, r.Search, r.Page)
		}
	case "links", "traverse":
		var r struct {
			ID string `json:"id"`
			memory.GraphOptions
		}
		if e = decode(&r); e == nil {
			if flags.Arg(0) == "links" {
				result, e = store.Links(ctx, r.ID, r.GraphOptions)
			} else {
				result, e = store.Traverse(ctx, r.ID, r.GraphOptions)
			}
		}
	case "sync":
		result, e = store.Sync(ctx, false)
	case "check":
		result, e = store.Sync(ctx, true)
	case "rebuild":
		result, e = store.Rebuild(ctx)
	case "stats":
		result, e = store.Stats(ctx)
	default:
		e = errors.New("unknown command; use -help")
	}
	if e != nil {
		return failure(e)
	}
	if e = json.NewEncoder(os.Stdout).Encode(result); e != nil {
		return failure(e)
	}
	return 0
}
func failure(e error) int {
	code := "error"
	status := 1
	if errors.Is(e, memory.ErrNotFound) {
		code = "not_found"
		status = 3
	}
	if errors.Is(e, memory.ErrConflict) {
		code = "conflict"
		status = 4
	}
	output := map[string]any{"error": code, "message": e.Error()}
	var collection *memory.CollectionError
	if errors.As(e, &collection) {
		output["diagnostics"] = collection.Diagnostics
	}
	_ = json.NewEncoder(os.Stderr).Encode(output)
	return status
}
