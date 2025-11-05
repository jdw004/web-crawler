package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"web-crawler/crawler"
)

func TestRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<title>CLI test</title>") }))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"-depth", "0", server.URL}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var result crawler.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Title != "CLI test" || result.Status != 200 || stderr.Len() != 0 {
		t.Fatalf("result=%+v stderr=%q", result, stderr.String())
	}
	path := filepath.Join(t.TempDir(), "results.jsonl")
	if err := run(t.Context(), []string{"-output", path, server.URL}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid(bytes.TrimSpace(data)) {
		t.Fatalf("invalid output: %s (%v)", data, err)
	}
	if err := run(t.Context(), []string{"-output", path, server.URL}, io.Discard, io.Discard); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output was not protected: %v", err)
	}
}

func TestArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"-bad-flag"}, {"-workers", "0", "https://example.com"}, {"not-a-url"}} {
		if err := run(t.Context(), args, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted invalid args: %v", args)
		}
	}
	if err := run(t.Context(), []string{"-help"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}
