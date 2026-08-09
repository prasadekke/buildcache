package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeDashboard(t *testing.T) {
	c := &Counters{}
	c.Get(true, 10*time.Millisecond)
	c.Get(false, 10*time.Millisecond)
	c.Put(1024, 5*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c.ServeDashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"50.0%", "1024"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard body missing %q; body:\n%s", want, body)
		}
	}
}

func TestServePrometheus(t *testing.T) {
	c := &Counters{}
	c.Get(true, 10*time.Millisecond)
	c.Put(2048, 5*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	c.ServePrometheus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"buildcache_gets_total 1",
		"buildcache_hits_total 1",
		"buildcache_puts_total 1",
		"buildcache_put_bytes_total 2048",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prometheus body missing %q; body:\n%s", want, body)
		}
	}
}
