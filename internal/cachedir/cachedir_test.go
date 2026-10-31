package cachedir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	got := Default()
	if got == "" {
		t.Fatal("Default() returned an empty string")
	}
	if cacheDir, err := os.UserCacheDir(); err == nil {
		want := filepath.Join(cacheDir, "buildcache")
		if got != want {
			t.Errorf("Default() = %q, want %q", got, want)
		}
	}
}
