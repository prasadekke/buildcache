package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prasadekke/buildcache/internal/metrics"
)

func TestDefaultDir(t *testing.T) {
	got := defaultDir()
	if got == "" {
		t.Fatal("defaultDir() returned an empty string")
	}
	if cacheDir, err := os.UserCacheDir(); err == nil {
		want := filepath.Join(cacheDir, "buildcache")
		if got != want {
			t.Errorf("defaultDir() = %q, want %q", got, want)
		}
	}
}

func TestPrintStats(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	printStats(metrics.Snapshot{Gets: 10, Hits: 8, Puts: 3, PutBytes: 4096})

	w.Close()
	os.Stderr = orig

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"10 gets", "8 hits", "80.0% hit rate", "3 puts", "4096 bytes stored"} {
		if !strings.Contains(got, want) {
			t.Errorf("printStats output missing %q; got:\n%s", want, got)
		}
	}
}
