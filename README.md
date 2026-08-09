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
      socket. `cacheprog` always keeps its own local disk tier and, if a
      daemon is reachable, layers it in as a second tier (see
      [`internal/cas/tiered`](internal/cas/tiered)):
      - **Reads (`Get`):** local is checked first; the daemon is only
        consulted on a local miss, and a remote hit is written through to
        local so the next lookup for that action never leaves the machine.
      - **Writes (`Put`):** always written to local first — that's what the
        build actually depends on — then best-effort pushed to the daemon
        too, so the object is available to every other machine sharing it.
        A slow or unreachable daemon delays or skips that push but never
        fails the local write or the build.

      Net effect: repeat local builds get local-disk speed even with a
      daemon configured, and a machine that's never built a given action
      before gets it from the shared cache instead of recompiling it.
- [x] **LRU eviction** — `-max-size`/`-gc-interval` on `cacheserver`
      (background sweeps) or `-gc` on `cacheprog` (opt-in, direct mode).
- [x] **Metrics / hit-rate dashboard** — `cacheserver -metrics-addr` serves
      a small dashboard at `/` and Prometheus-format metrics at `/metrics`;
      `cacheprog -stats` prints a one-line summary for direct-mode builds.
      No new dependencies — see [`internal/metrics`](internal/metrics).

## Benchmarks

Measured against [caddyserver/caddy](https://github.com/caddyserver/caddy)
(47 packages), `go build ./...`:

| Scenario | Wall time |
|---|---|
| Cold, no cache at all (today's reality on a fresh CI runner) | 1m 15s |
| buildcache, fresh machine + a teammate's pre-warmed shared cache | 17.0s ± 3.3s |
| Go's own local `GOCACHE`, warm, same-machine rebuild (the ceiling) | 16.9s ± 2.3s |

(Warm numbers are mean ± stddev over 5 interleaved trials each — the two
are statistically tied, not a real ranking.) A machine that has *never
built this repo* lands within noise of a machine that's already fully
warmed up locally, just by fetching from the shared cache instead of
recompiling from scratch. That's the ~4.4x win in one row.

One thing worth knowing going in: `GOCACHE` and `GOCACHEPROG` are mutually
exclusive in the go command itself (`cmd/go/internal/cache/default.go`) —
setting `GOCACHEPROG` at all means Go never touches your local `GOCACHE`,
warm or not. `internal/cas/tiered` exists specifically to claw that local
speed back at the buildcache layer instead: once an action has been
fetched once, it's served straight from local disk on every subsequent
build, without a daemon round trip.

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
local-only mode with `-no-server`. `-dir` is always used as the local tier,
whether or not a daemon is also reachable.

**Metrics:**

```bash
./cacheserver -dir $HOME/.cache/buildcache -metrics-addr :9090 &
# http://localhost:9090/         human dashboard (hit rate, counts, latency)
# http://localhost:9090/metrics  Prometheus text format

GOCACHEPROG="$(pwd)/cacheprog -stats" go build ./...
# cacheprog: 42 gets (31 hits, 73.8% hit rate), 6 puts, 583291 bytes stored
```

`-stats` reports on `cacheprog`'s own view of the request (which tier
served it, or a daemon round trip that missed everywhere), for every build
regardless of whether a daemon is reachable. `cacheserver -metrics-addr`
reports only what actually reached the daemon, which will be less than the
whole team's request volume once local tiers are warm — that's the point,
not a discrepancy to chase down. See [`internal/metrics`](internal/metrics)
if you want to plug in your own backend (OTel, StatsD, ...) instead of the
built-in counters — `Recorder` is a four-method interface, no SDK required.

## Layout

```
cmd/
  cacheprog/     GOCACHEPROG entrypoint the go command invokes directly
  cacheserver/   shared daemon: one store, many connections
internal/
  protocol/       GOCACHEPROG wire types + request/response loop (Server) and a client (Client) for talking to another Server
  cas/             storage interface (Store) + protocol adapter (Handler)
  cas/disk/        disk-backed Store implementation
  cas/remote/       Store that talks to a remote cacheserver via protocol.Client
  cas/tiered/        Store composing a local + remote Store: local-first, remote fallback, write-through
  sockpath/        shared default socket path
  metrics/          Recorder hook + in-memory Counters, dashboard/Prometheus endpoints
```

## Testing

```bash
go test ./...          # full suite
go test -race ./...    # with the race detector -- run this before touching the protocol package
go vet ./...
gofmt -l .
```

Coverage is concentrated where the actual logic lives (`internal/protocol`,
`internal/cas/*`), not in `cmd/`'s thin flag-parsing wiring. Worth calling
out: `internal/protocol`'s tests replicate the real go command's wire
framing byte-for-byte (see `encodeLikeGoCommand` in `server_test.go`)
rather than a simplified stand-in for it. That distinction is what let two
real bugs — a blank-line framing mismatch and a zero-byte-body nil pointer,
both invisible to hand-written smoke tests using simplified framing — ship
silently before landing here as regression tests.

## Contributing

Early-stage, single-maintainer project — issues and PRs welcome, but expect
things to shift. The "Status" checklist above is the current roadmap; open
an issue if you're looking for where to start.

## License

Apache 2.0 — see [LICENSE](LICENSE).
