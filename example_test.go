package memory_test

import (
	"context"
	"fmt"
	"os"

	memory "markdownmemory"
)

func ExampleStore() {
	root, err := os.MkdirTemp("", "memory-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)
	store, err := memory.Open(root, memory.Options{})
	if err != nil {
		panic(err)
	}
	defer store.Close()
	ctx := context.Background()
	_, err = store.Create(ctx, memory.Record{ID: "session", Title: "Recording session", Body: "We recorded a song together.", Taxonomies: map[string][]memory.Value{"people": {memory.Text("Dimitra")}}})
	if err != nil {
		panic(err)
	}
	// Include only these names in model context. Entries are an explicit lookup.
	vocabulary, err := store.Taxonomies(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(vocabulary.Names)
	value := memory.Text("Dimitra")
	results, err := store.Query(ctx, memory.Query{All: []memory.Filter{{Taxonomy: "people", Op: "eq", Value: &value}}})
	if err != nil {
		panic(err)
	}
	fmt.Println(results.Total, results.Items[0].ID)
	// Output:
	// [people]
	// 1 session
}
