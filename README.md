# buildcache

A self-hostable remote build cache for Go, built on the `GOCACHEPROG`
protocol added in Go 1.24. No BUILD files, no config beyond one environment
variable — the go command already computes its own action graph and
content hashes; this just gives it somewhere shared to put the results.

```
GOCACHEPROG="/path/to/cacheprog" go build ./...
GOCACHEPROG="/path/to/cacheprog" go test ./...   # same cache, no extra setup
```

Requires Go **1.24+** for the invoking `go` command (that's what defines
`GOCACHEPROG`). `buildcache` itself builds with any reasonably modern Go
toolchain, since it has no dependencies outside the standard library.

## Why

Bazel/Buck2-style build systems get correctness from explicitness — every
input and output is declared, which is what makes their action cache
trustworthy. That explicitness is also most of the adoption cost. Go
sidesteps the problem: the compiler already knows its own action graph, so
an external cache only has to answer "have you seen this key?" — no BUILD
files, no graph inference.

Go is the starting point, not the only target: Gradle/JVM and Turborepo/Nx
both have their own remote-cache protocols that could sit on the same CAS
backend behind an HTTP frontend; Rust would need a `RUSTC_WRAPPER`-based
frontend instead, since Cargo has no equivalent extension point.

## Status

- [x] **Local disk cache** — `cacheprog` speaks the GOCACHEPROG wire
      protocol, backed by a local disk store with atomic writes.
- [x] **CAS interface extracted** — storage (`internal/cas`) is decoupled
      from the wire protocol (`internal/protocol`), so a new backend only
      has to implement four methods, not the whole protocol.
- [x] **Server mode** — `cacheserver` is a long-lived daemon holding one
      warm store, shared across many `cacheprog` invocations over a Unix
      socket. `cacheprog` tries the daemon first and falls back to a
      standalone disk store if none is running, so it works with zero
      setup either way.
- [x] **LRU eviction** — `-max-size`/`-gc-interval` on `cacheserver`
      (background sweeps) or `-gc` on `cacheprog` (opt-in, direct mode).
- [x] **Metrics / hit-rate dashboard** — `cacheserver -metrics-addr` serves
      a small dashboard at `/` and Prometheus-format metrics at `/metrics`;
      `cacheprog -stats` prints a one-line summary for direct-mode builds.
      No new dependencies — see [`internal/metrics`](internal/metrics).

## Usage

**Standalone (no daemon):**

```bash
go build -o cacheprog ./cmd/cacheprog
GOCACHEPROG="$(pwd)/cacheprog -dir $HOME/.cache/buildcache" go build ./...
```

**With the shared daemon:**

```bash
go build -o cacheserver ./cmd/cacheserver
go build -o cacheprog ./cmd/cacheprog

./cacheserver -dir $HOME/.cache/buildcache &   # start once, stays running

GOCACHEPROG="$(pwd)/cacheprog" go build ./...   # picks up the daemon automatically
GOCACHEPROG="$(pwd)/cacheprog" go test ./...
```

`cacheprog` looks for the daemon at a UID-namespaced path under the OS temp
dir by default (see `internal/sockpath`); override with `-socket`, or force
standalone mode with `-no-server`.

**Metrics:**

```bash
./cacheserver -dir $HOME/.cache/buildcache -metrics-addr :9090 &
# http://localhost:9090/         human dashboard (hit rate, counts, latency)
# http://localhost:9090/metrics  Prometheus text format

GOCACHEPROG="$(pwd)/cacheprog -stats" go build ./...
# cacheprog: 42 gets (31 hits, 73.8% hit rate), 6 puts, 583291 bytes stored
```

`-stats` only reports on direct-mode builds — when `cacheprog` reaches a
daemon it just relays bytes, so use the daemon's own `-metrics-addr`
instead. See [`internal/metrics`](internal/metrics) if you want to plug in
your own backend (OTel, StatsD, ...) instead of the built-in counters —
`Recorder` is a four-method interface, no SDK required.

## Layout

```
cmd/
  cacheprog/     GOCACHEPROG entrypoint the go command invokes directly
  cacheserver/   shared daemon: one store, many connections
internal/
  protocol/       GOCACHEPROG wire types + request/response loop
  cas/             storage interface (Store) + protocol adapter (Handler)
  cas/disk/        disk-backed Store implementation
  sockpath/        shared default socket path
  metrics/          Recorder hook + in-memory Counters, dashboard/Prometheus endpoints
```

## Contributing

Early-stage, single-maintainer project — issues and PRs welcome, but expect
things to shift. The "Status" checklist above is the current roadmap; open
an issue if you're looking for where to start.

## License

Apache 2.0 — see [LICENSE](LICENSE).
