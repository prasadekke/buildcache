package metrics

import (
	"fmt"
	"net/http"
)

// ServeDashboard renders a small human-readable hit-rate summary. Mounted
// at "/" by cmd/cacheserver when -metrics-addr is set.
func (c *Counters) ServeDashboard(w http.ResponseWriter, r *http.Request) {
	s := c.Snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, dashboardHTML,
		s.HitRate()*100,
		s.Gets, s.Hits, s.GetErrors, s.MeanGetLatency(),
		s.Puts, s.PutErrors, s.PutBytes, s.MeanPutLatency(),
	)
}

// ServePrometheus renders c in Prometheus text exposition format. Mounted
// at "/metrics" by cmd/cacheserver when -metrics-addr is set — this is the
// hook for OpenTelemetry Collector's Prometheus receiver and similar
// scrape-based consumers, without buildcache depending on any of their
// client libraries.
func (c *Counters) ServePrometheus(w http.ResponseWriter, r *http.Request) {
	s := c.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, prometheusFormat,
		s.Gets, s.Hits, s.GetErrors, s.Puts, s.PutErrors, s.PutBytes,
		s.GetLatency.Seconds(), s.PutLatency.Seconds(),
	)
}

const dashboardHTML = `<!doctype html>
<html>
<head>
<title>buildcache</title>
<meta http-equiv="refresh" content="5">
<style>
body { font-family: system-ui, sans-serif; max-width: 640px; margin: 2rem auto; color: #222; }
table { border-collapse: collapse; width: 100%%; }
td, th { padding: .35rem .75rem; text-align: left; border-bottom: 1px solid #ddd; }
.rate { font-size: 2.5rem; font-weight: 600; margin: 0 0 1rem; }
</style>
</head>
<body>
<h1>buildcache</h1>
<p class="rate">%.1f%% hit rate</p>
<table>
<tr><th>gets</th><td>%d</td></tr>
<tr><th>hits</th><td>%d</td></tr>
<tr><th>get errors</th><td>%d</td></tr>
<tr><th>mean get latency</th><td>%s</td></tr>
<tr><th>puts</th><td>%d</td></tr>
<tr><th>put errors</th><td>%d</td></tr>
<tr><th>bytes stored</th><td>%d</td></tr>
<tr><th>mean put latency</th><td>%s</td></tr>
</table>
<p><a href="/metrics">/metrics</a> (Prometheus text format)</p>
</body>
</html>
`

const prometheusFormat = `# HELP buildcache_gets_total Total "get" requests served.
# TYPE buildcache_gets_total counter
buildcache_gets_total %d
# HELP buildcache_hits_total Total "get" requests that were cache hits.
# TYPE buildcache_hits_total counter
buildcache_hits_total %d
# HELP buildcache_get_errors_total Total "get" requests that returned an error.
# TYPE buildcache_get_errors_total counter
buildcache_get_errors_total %d
# HELP buildcache_puts_total Total "put" requests served.
# TYPE buildcache_puts_total counter
buildcache_puts_total %d
# HELP buildcache_put_errors_total Total "put" requests that returned an error.
# TYPE buildcache_put_errors_total counter
buildcache_put_errors_total %d
# HELP buildcache_put_bytes_total Total bytes stored across all "put" requests.
# TYPE buildcache_put_bytes_total counter
buildcache_put_bytes_total %d
# HELP buildcache_get_seconds_total Total time spent inside Store.Get, in seconds.
# TYPE buildcache_get_seconds_total counter
buildcache_get_seconds_total %f
# HELP buildcache_put_seconds_total Total time spent inside Store.Put, in seconds.
# TYPE buildcache_put_seconds_total counter
buildcache_put_seconds_total %f
`
