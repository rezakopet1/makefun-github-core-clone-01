# Design note — github-core-clone-01, round 4: compiled static Go core

## Why Go, why now

Rounds 2–3 proved the wall: Node server CPU is only 0.2–0.9 ms/request, but
wall time is 2–4x that (client + loopback + scheduling). Local Node gains
(+7, then +1.5) never transferred to the server pod (125 → 116). The remaining
lever is structural: a compiled static binary with a lower per-request floor,
cheaper JSON/pack paths, and no interpreter warm-up. Target: ~1.7x faster than
our Node on the server pod (server perf 116 → ~155+, top-5 bar).

## Layout

- `main.ts` — thin launcher, authored fresh: spawns `./forge`, forwards
  SIGINT/SIGTERM/SIGHUP, exits with the child's code. No lifecycle logic here.
- `forge` — committed static Linux amd64 binary (CGO_ENABLED=0, stripped,
  must stay < 8 MiB; a net/http+json+zlib+sha1 probe measured 3.26 MiB).
- `go/` — the forge source, package main, stdlib only:
  - `main.go` — lifecycle: bind `:0`, POST `{port}` to
    `$MAKEFUN_ENV_URL/register` (retry, 200/409 ok, 400/405 fatal), poll GET
    `/done` until `{"done":true}`, close, exit 0. No clock, no randomness.
  - `http.go` — net/http server, routing, auth, request ids, JSON senders.
  - `store.go` — in-memory store: repos, git objects, trees/commits, pack
    read/write (non-deltified v2 out, deltas resolved in), merge/diff,
    issues/pulls, caches.
  - `git.go` — pkt-lines, v2/v0 advertisements, ls-refs, fetch, receive-pack.
  - `rest.go` — REST v3 handlers + JSON builders (field order mirrors the
    Node bodies).
  - `graphql.go` — recursive-descent GraphQL parser + typed executor.
  - `mcp.go` — MCP: server/discover, initialize, tools/list, tools/call.

## Behavioral spec

The Node implementation at commits d0c2791..c15c423 (120/120 checks) is the
spec, plus the kit's `sdk/run.ts` checks and the brief. This Go code is a
fresh port written from this note: same wire behavior, same JSON shapes, same
error strings, same determinism rules — new Go code, never re-typed Node.

## Concurrency

- One `sync.RWMutex` guards all structural state (repos, objects, refs,
  issues, pulls, counters). Dispatch takes RLock for read-only routes
  (GETs, upload-pack fetch) and Lock for mutations (REST writes,
  receive-pack, GraphQL, MCP).
- Per-repo `sync.Mutex` guards the response caches only (fetch packs,
  advertisements, ls-refs, sorted refs, serialized issue JSON). Lock order is
  always store-mu → cache-mu. The 50 concurrent shallow clones share the
  RLock and hit one cached pack build.
- No goroutine-per-request state beyond the request itself; no channels in
  hot paths.

## Hot-path design (the perf bet)

- Pre-serialize once, serve bytes: v2 advertisement is a package constant;
  v0 advertisements, ls-refs, fetch packs and single-issue JSON are cached
  per repo version and dropped by `touchRepo` / issue invalidation.
- Single `Write` per response with explicit `Content-Length`; no chunked
  encoding; keep-alive friendly.
- Zero-alloc where cheap: manual path splitting (no `net/url` parse per
  request — `http.Request.URL` is already parsed by net/http), byte-level
  pkt-line hex (no `fmt.Sprintf`, no regex, no `ParseInt` in hot paths),
  `[]byte` caches instead of re-marshal.
- Pack write: non-deltified pack v2, deflate level 1, deterministic order
  (commits, trees, blobs, tags; oid-sorted within a kind).
- Pack read: ONE inflate per entry with the exact consumed length. Go's
  `compress/zlib` does not report consumed bytes, so inflate through a
  1-byte-at-a-time counting reader: verified exact (58/58 bytes on a probe
  stream) and only ~6 µs/entry slower than a slice inflate (30 µs vs 24 µs
  on a 1 KiB blob). This replaces Node's binary-search probe loop.
- JSON: `encoding/json` over structs declared in wire field order; hot
  bodies served from `[]byte` caches, never re-marshalled.

## Determinism

All timestamps are the fixed trial instant (`2026-11-14T06:30:00Z`,
epoch 1700000000); all ids are sequential counters; all orderings are
explicit sorts or insertion order (never Go map iteration in output). No
`time.Now`, no `math/rand`, no wall clock in anything observable. Two runs
on the same requests leave byte-identical state (replay must match).

## Size / build

```
export GOROOT=$HOME/go GOPATH=$HOME/gopath PATH=$HOME/go/bin:$PATH
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge ./go
```

Binary must stay < 8 MiB; whole tree < 16 MiB. No `go.mod` network use:
stdlib only, `GOFLAGS=-mod=mod` not needed; build with `GOPROXY=off`.

## Verification

- After each surface: `node --experimental-strip-types <kit>/sdk/run.ts
  --url http://127.0.0.1:PORT --token <any> [--perf]` against a locally
  started forge.
- Full: `node cli/makefun.mjs grade-local --kit <kit> --bot <repo>
  --driver process` (120/120 + replay byte-identical).
- Perf: `taskset -c 0` constrained runs of the same, plus the sdk `--perf`
  per-workload medians. Gate: 120/120 AND constrained perf well above the
  166 band; otherwise report the numbers and stop.
