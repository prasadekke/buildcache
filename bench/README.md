# buildcache benchmarks

Reproducible measurements of `buildcache` against real repos, run on a
dedicated (not shared-tenant) cloud VM so results are comparable across
runs and citable elsewhere.

**Status: harness built and validated against a synthetic zero-dependency
repo (see `results/synth-validation.csv`), not yet run against the real
target repos.** That validation run is the first time in this project that
an actual `go build` with `GOCACHEPROG` set was confirmed to engage
`cacheprog`/`cacheserver` end-to-end — everything before it only tested the
wire protocol directly via piped JSON, bypassing the real go command
entirely. Results: 175/176 cache hits and a ~26x speedup for a fresh local
dir talking to a pre-warmed shared cache vs. a cold build, with the local
tier landing within ~10% of Go's own `GOCACHE` speed. That's real signal
that the mechanism works, but it's a 3-package synthetic repo, not
evidence about Caddy/Prometheus/Kubernetes-scale behavior — hence still
needing the real runs below.

The `README.md` at the repo root currently has a benchmark table (Caddy,
"5 interleaved trials") with no script or raw data behind it anywhere in
the repo. Treat it as unverified until it's replaced by a run through this
harness with its CSV committed alongside.

## Why a dedicated VM

A laptop or a shared/burstable cloud instance introduces noise (thermal
throttling, other tenants' CPU steal) that shows up as variance in the
numbers, especially for the tight "warm" comparisons where the two
scenarios being compared are expected to be close. A dedicated-vCPU
instance removes the noisy-neighbor variable and gives a machine spec
that's meaningful to publish alongside the numbers.

**Recommended**: Hetzner Cloud `CCX33` (8 dedicated vCPU / 32 GB RAM /
NVMe), Ubuntu 24.04 LTS image, any region. Cheap enough that a full
benchmark session costs well under $1 at hourly billing, and dedicated
vCPUs are the reproducibility-relevant property, not the specific
provider. An AWS `c6i.2xlarge` (8 vCPU, on-demand) in `us-east-1` is a
reasonable alternative if you'd rather publish AWS-branded specs; either
should be noted explicitly in the results writeup, since "vCPU count"
alone doesn't fully capture reproducibility (Hetzner's dedicated vCPU line
maps to genuinely reserved cores; AWS's do too for that instance family,
but that's worth double-checking for whatever instance type you end up
picking).

VM setup, once provisioned:

```bash
sudo apt-get update -qq
sudo apt-get install -y -qq golang-1.24-go git python3
export PATH=/usr/lib/go-1.24/bin:$PATH
go version   # confirm 1.24+ -- GOCACHEPROG doesn't exist before that

git clone git@github.com:<you>/buildcache.git
cd buildcache
```

(If the repo isn't pushed anywhere yet, `scp`/`rsync` it to the VM
instead.)

## Pinned targets

Real, current, tagged releases as of when this was written (2026-09-07) --
re-verify these still resolve before a real run; don't blindly reuse an
old tag months later without checking whether it still makes sense as
"current."

| Label | Repo | Tag | Size class |
|---|---|---|---|
| `caddy-small` | https://github.com/caddyserver/caddy | `v2.11.4` | small |
| `prometheus-medium` | https://github.com/prometheus/prometheus | `v3.14.0` | medium |
| `kubernetes-large` | https://github.com/kubernetes/kubernetes | `v1.36.4` | large |

Kubernetes in particular: `go build ./...` here is a simplification, not
a reproduction of Kubernetes' own multi-stage build system (which uses
`make`, cross-compilation, and code generation steps this harness doesn't
run). That's a deliberate scope decision to keep the comparison
apples-to-apples across all three repos using one mechanism
(`GOCACHEPROG`) rather than three different build systems -- state this
plainly in any writeup, since a Kubernetes-familiar reader would otherwise
reasonably ask why `make quick-release` numbers aren't here.

## Running

```bash
./bench/run.sh https://github.com/caddyserver/caddy v2.11.4 caddy-small 5
./bench/run.sh https://github.com/prometheus/prometheus v3.14.0 prometheus-medium 5
./bench/run.sh https://github.com/kubernetes/kubernetes v1.36.4 kubernetes-large 5
```

Each writes raw per-run data to `bench/results/<label>.csv` -- commit
these alongside any summary numbers published anywhere, so the underlying
measurements are always auditable, not just a table someone has to trust.

Kubernetes' `cold_no_cache` scenario alone will likely take a long while
(it's a genuinely large build); run it with `nohup`/`tmux` if the VM
session might disconnect, e.g.:

```bash
tmux new -s bench
./bench/run.sh https://github.com/kubernetes/kubernetes v1.36.4 kubernetes-large 5
# Ctrl-b d to detach, tmux attach -t bench to check back in
```

## Summarizing

```bash
python3 bench/summarize.py bench/results/*.csv
```

Prints a markdown table (mean, population stddev, mean hit rate per
scenario) to stdout, and the pinned commit for each label to stderr for
the methodology section of a writeup. It only reads the CSVs -- rerun
freely, and keep the CSVs as the source of truth if the summary and a
manual recheck of the raw numbers ever disagree.

## What "done" looks like

Before publishing anywhere:

- [ ] All three real repos run, `bench/results/*.csv` committed
- [ ] VM specs (provider, instance type, region, OS image, Go version)
      recorded in the writeup, not just "a cloud VM"
- [ ] Root `README.md`'s existing unverified benchmark table replaced (not
      just appended to) with real numbers plus a link to this directory
- [ ] Any claim in prose ("Nx faster", "within noise of") checked against
      the actual stddev in the CSV, not eyeballed from the mean alone
