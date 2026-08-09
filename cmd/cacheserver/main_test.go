package main

import (
	"os"
	"path/filepath"
	"testing"
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
