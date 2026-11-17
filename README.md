# makefun-github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/rezakopet1/makefun-github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/rezakopet1/makefun-github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Build a standalone GitHub-compatible git forge: no UI, one process that serves git smart HTTP, the REST v3 API, GraphQL and MCP from its own store; a sealed suite drives it as a client while four timed workloads measure how fast it answers.

## Run it

```sh
node --experimental-strip-types main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=a2zdf556&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

## The lifecycle (main.ts)

The env is a server the harness starts beside yours. Its origin is in
`MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** `server.listen(0)` lets the kernel pick one;
   read it with `server.address().port`. Never bind 8080: that is the env's
   port.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with
   `{"port": <the port you bound>}`, as JSON.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with
   `{"done":false}` while the suite runs and `{"done":true}` once its export
   is written. Poll it with a short backoff; never exit before it says `true`.
4. **Exit 0** when it does.

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips
TypeScript types on its own; the flag is harmless there and makes an older
local Node work too. `main.ts` is a thin launcher: it spawns the compiled
forge binary `./forge` (Go, stdlib only, built from `go/`), forwards signals
to it, and exits with its code. There is nothing to install: the binary is
committed in the repository, and the launcher imports `node:child_process`
only.

## The surface

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | Model Context Protocol over streamable HTTP: `server/discover`, `initialize`, `tools/list`, `tools/call` |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

Every API call arrives with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`;
`/healthz` takes none. A request without a token is 401.

## Layout

- `main.ts`: the lifecycle launcher. Spawns `./forge`, forwards signals,
  exits with its code.
- `forge`: the compiled static Go binary (CGO_ENABLED=0, stripped, 6.4 MiB),
  built from `go/` — see `DESIGN.md` for the build command.
- `go/main.go`: the lifecycle (bind port 0, register, poll `/done`, exit 0)
  and the HTTP server boot.
- `go/http.go`: routing, auth, request ids, JSON senders, git smart-HTTP
  dispatch.
- `go/store.go`: the in-memory store (repos, git objects, issues, pull
  requests), packfile read/write, and the squash-merge logic.
- `go/git.go`: pkt-line framing, v2/v0 advertisements, ls-refs, fetch and
  receive-pack.
- `go/rest.go`: the REST v3 routes.
- `go/graphql.go`: a small GraphQL parser and executor for the checked
  documents.
- `go/mcp.go`: the MCP endpoint and its six tools.
- `DESIGN.md`: the design note this port was written from.
- `package.json`, `tsconfig.json`, `LICENSE` (MIT): the entry's metadata
  and licence.

## Determinism

All timestamps are the fixed trial instant, all ids are sequential counters
and all orderings are stable, so two runs on the same requests leave
byte-identical state. No clock reads and no randomness anywhere observable.
