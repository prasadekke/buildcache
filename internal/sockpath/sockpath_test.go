package sockpath

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestDefault_IncludesUID(t *testing.T) {
	got := Default()
	want := strconv.Itoa(os.Getuid())
	if !strings.Contains(got, want) {
		t.Errorf("Default() = %q, want it to contain uid %q", got, want)
	}
}

func TestDefault_Deterministic(t *testing.T) {
	a := Default()
	b := Default()
	if a != b {
		t.Errorf("Default() returned different paths on two calls: %q vs %q", a, b)
	}
}

func TestDefault_UnderTempDir(t *testing.T) {
	got := Default()
	if !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("Default() = %q, want a path under %q", got, os.TempDir())
	}
}
