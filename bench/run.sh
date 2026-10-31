#!/usr/bin/env bash
# bench/run.sh -- buildcache benchmark harness.
#
# Usage: run.sh <repo_url> <ref> <label> [trials]
#
#   repo_url  git clone URL, e.g. https://github.com/caddyserver/caddy
#   ref       a tag or branch to check out (pinned, not "latest") for
#             reproducibility -- see bench/README.md for the exact tags
#             this project's own results were measured against
#   label     short slug used for the output file, e.g. "caddy-small"
#   trials    repetitions per scenario (default 5)
#
# Appends one CSV row per individual run (not just a summary) to
# bench/results/<label>.csv, so the raw data is always available to
# re-check or re-aggregate later -- never publish only a summary table
# without this file alongside it.
#
# Scenarios, run against `go build ./...` at the pinned commit:
#   1. cold_no_cache                    -- go clean -cache, cold build.
#      Today's reality on a fresh CI runner with nothing pre-warmed.
#   2. local_gocache_warm                -- Go's own local GOCACHE, warm
#      (repeat build, same machine). The speed ceiling: nothing beats
#      talking to your own local build cache in-process.
#   3. buildcache_fresh_machine_shared_warm -- a *fresh* local buildcache
#      dir (simulating a machine that's never built this repo) talking to
#      a cacheserver daemon that already has the repo's objects
#      (simulating a teammate's pre-warmed shared cache).
#   4. buildcache_local_tier_warm        -- buildcache with its own local
#      tier already warm (this machine has built this exact commit
#      before). Compared against scenario 2 to check whether the local
#      tier actually closes the gap to GOCACHE's speed, per
#      internal/cas/tiered's design intent.
#
# Scenarios 2 and 4 are interleaved trial-by-trial (2,4,2,4,...) rather
# than run as two separate blocks, so any machine drift (thermal
# throttling, background load) affects both equally instead of biasing
# whichever ran second.
set -euo pipefail

REPO_URL="$1"
REF="$2"
LABEL="$3"
TRIALS="${4:-5}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
RESULTS_DIR="$ROOT/bench/results"
mkdir -p "$RESULTS_DIR"
CSV="$RESULTS_DIR/${LABEL}.csv"

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }
cleanup() {
    [[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

echo "repo,commit,label,scenario,trial,wall_seconds,gets,hits,hit_rate_pct,puts,put_bytes" > "$CSV"

log "building cacheprog/cacheserver from $ROOT"
go build -o "$WORK/cacheprog" "$ROOT/cmd/cacheprog"
go build -o "$WORK/cacheserver" "$ROOT/cmd/cacheserver"

log "cloning $REPO_URL @ $REF (shallow)"
git clone --quiet --depth 1 --branch "$REF" "$REPO_URL" "$WORK/repo"
cd "$WORK/repo"
COMMIT="$(git rev-parse HEAD)"
log "pinned commit: $COMMIT"

log "pre-warming module cache (excluded from all timings below)"
go mod download 2>&1 | tail -5 || true

# wall_time runs "$@", discarding stdout, saving stderr to
# $WORK/last_stderr.txt (for -stats parsing), and prints elapsed
# wall-clock seconds to stdout.
wall_time() {
    local start end
    start=$(date +%s.%N)
    "$@" >/dev/null 2>"$WORK/last_stderr.txt"
    end=$(date +%s.%N)
    python3 -c "print(f'{$end - $start:.3f}')"
}

# record <scenario> <trial> <wall_seconds> [parse_stats]
# parse_stats=1 pulls hit-rate numbers out of $WORK/last_stderr.txt,
# which cacheprog -stats writes as:
#   "cacheprog: N gets (H hits, R% hit rate), P puts, B bytes stored"
record() {
    local scenario="$1" trial="$2" seconds="$3" parse_stats="${4:-0}"
    local gets=0 hits=0 hitrate=0 puts=0 putbytes=0
    if [[ "$parse_stats" == "1" && -f "$WORK/last_stderr.txt" ]] && grep -q "gets (" "$WORK/last_stderr.txt"; then
        gets=$(grep -oP '\d+(?= gets)' "$WORK/last_stderr.txt" | tail -1)
        hits=$(grep -oP '\d+(?= hits)' "$WORK/last_stderr.txt" | tail -1)
        hitrate=$(grep -oP '[\d.]+(?=% hit rate)' "$WORK/last_stderr.txt" | tail -1)
        puts=$(grep -oP '\d+(?= puts)' "$WORK/last_stderr.txt" | tail -1)
        putbytes=$(grep -oP '\d+(?= bytes stored)' "$WORK/last_stderr.txt" | tail -1)
    fi
    echo "$REPO_URL,$COMMIT,$LABEL,$scenario,$trial,$seconds,$gets,$hits,$hitrate,$puts,$putbytes" >> "$CSV"
    log "  $scenario trial $trial: ${seconds}s (hits=$hits/$gets)"
}

# --- Scenario 1: cold, no cache at all ---
log "scenario 1: cold_no_cache ($TRIALS trials)"
for i in $(seq 1 "$TRIALS"); do
    go clean -cache
    t=$(wall_time go build ./...)
    record "cold_no_cache" "$i" "$t"
done

# --- Start the shared daemon for scenarios 3 and 4 ---
SERVER_DIR="$WORK/server-cache"
SOCK="$WORK/cacheserver.sock"
"$WORK/cacheserver" -dir "$SERVER_DIR" -socket "$SOCK" -max-size 0 &
SERVER_PID=$!
sleep 0.3

log "warming the shared cache once (simulates a teammate's prior build)"
go clean -cache
GOCACHEPROG="$WORK/cacheprog -dir $WORK/warmer-local -socket $SOCK" go build ./... >/dev/null 2>&1

log "scenario 3: buildcache_fresh_machine_shared_warm ($TRIALS trials)"
for i in $(seq 1 "$TRIALS"); do
    go clean -cache
    FRESH_DIR="$WORK/fresh-$i"
    rm -rf "$FRESH_DIR"
    t=$(wall_time env GOCACHEPROG="$WORK/cacheprog -dir $FRESH_DIR -socket $SOCK -stats" go build ./...)
    record "buildcache_fresh_machine_shared_warm" "$i" "$t" 1
done

# --- Scenarios 2 and 4, interleaved trial-by-trial ---
log "scenarios 2+4 (interleaved): local_gocache_warm vs buildcache_local_tier_warm ($TRIALS trials each)"
go clean -cache
go build ./... >/dev/null 2>&1  # warm GOCACHE once, untimed

WARM_DIR="$WORK/warm-local-tier"
rm -rf "$WARM_DIR"
GOCACHEPROG="$WORK/cacheprog -dir $WARM_DIR -socket $SOCK" go build ./... >/dev/null 2>&1  # warm buildcache's local tier once, untimed

for i in $(seq 1 "$TRIALS"); do
    t=$(wall_time go build ./...)
    record "local_gocache_warm" "$i" "$t"

    t=$(wall_time env GOCACHEPROG="$WORK/cacheprog -dir $WARM_DIR -socket $SOCK -stats" go build ./...)
    record "buildcache_local_tier_warm" "$i" "$t" 1
done

log "done: $CSV"
