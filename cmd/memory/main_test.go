package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestCLIEndToEnd(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "memory")
	build := exec.Command("go", "build", "-o", binary, ".")
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	root := t.TempDir()
	run := func(command string, input any, status int) map[string]any {
		t.Helper()
		b, _ := json.Marshal(input)
		cmd := exec.Command(binary, "-root", root, command)
		cmd.Stdin = bytes.NewReader(b)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		e := cmd.Run()
		actual := 0
		if e != nil {
			if x, ok := e.(*exec.ExitError); ok {
				actual = x.ExitCode()
			} else {
				t.Fatal(e)
			}
		}
		if actual != status {
			t.Fatalf("%s exit=%d want=%d stdout=%s stderr=%s", command, actual, status, stdout.String(), stderr.String())
		}
		var out map[string]any
		data := stdout.Bytes()
		if status != 0 {
			data = stderr.Bytes()
		}
		if e = json.Unmarshal(data, &out); e != nil {
			t.Fatalf("%s invalid output %q: %v", command, data, e)
		}
		return out
	}
	saved := run("create", map[string]any{"id": "a", "title": "Recording", "body": "Η Δήμητρα είπε hello.\n", "taxonomies": map[string]any{"people": []any{map[string]any{"type": "text", "text": "Dimitra"}}}}, 0)
	read := run("get", map[string]any{"id": "a"}, 0)
	record := read["record"].(map[string]any)
	if record["body"] != "Η Δήμητρα είπε hello.\n" {
		t.Fatal(read)
	}
	vocab := run("taxonomies", nil, 0)
	if len(vocab["names"].([]any)) != 1 {
		t.Fatal(vocab)
	}
	run("entries", map[string]any{"name": "people", "search": "dim", "limit": 1}, 0)
	run("query", map[string]any{"all": []any{map[string]any{"taxonomy": "people", "op": "eq", "value": map[string]any{"type": "text", "text": "Dimitra"}}}}, 0)
	run("search", map[string]any{"text": "hello", "mode": "hybrid"}, 0)
	run("get-many", map[string]any{"ids": []string{"a", "missing"}}, 0)
	run("links", map[string]any{"id": "a"}, 0)
	run("traverse", map[string]any{"id": "a", "depth": 2}, 0)
	for _, command := range []string{"stats", "sync", "check", "rebuild"} {
		run(command, nil, 0)
	}
	patched := run("patch", map[string]any{"id": "a", "expected_hash": saved["hash"], "changes": map[string]any{"title": "Patched"}}, 0)
	record["title"] = "Updated"
	updated := run("update", map[string]any{"record": record, "expected_hash": patched["hash"]}, 0)
	run("update", map[string]any{"record": record, "expected_hash": saved["hash"]}, 4)
	run("delete", map[string]any{"id": "a", "expected_hash": updated["hash"]}, 0)
	run("get", map[string]any{"id": "a"}, 3)
	run("query", map[string]any{"unknown": true}, 1)
	// Independent CLI processes coordinate through the collection lock.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(binary, "-root", root, "create")
			cmd.Stdin = bytes.NewBufferString(fmt.Sprintf(`{"id":"p%d","title":"Process"}`, i))
			b, e := cmd.CombinedOutput()
			if e != nil {
				errs <- fmt.Errorf("%w: %s", e, b)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	stats := run("stats", nil, 0)
	if stats["records"] != float64(4) {
		t.Fatal(stats)
	}
	if _, e := os.Stat(filepath.Join(root, "records", "p0.md")); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"--unknown"}, {}} {
		cmd := exec.Command(binary, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if e := cmd.Run(); e == nil {
			t.Fatal("invalid flags accepted")
		}
		var problem map[string]any
		if e := json.Unmarshal(stderr.Bytes(), &problem); e != nil {
			t.Fatal("flag error was not pure JSON", stderr.String(), e)
		}
	}
}
