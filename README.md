# github-core-clone-01

A standalone, one-process, no-UI GitHub-compatible git forge for
[the contest](https://make.fun/bounty/github-core-clone-01), built by
`{owner}` in the `{repo}` repository. It serves git smart HTTP
(upload-pack protocol v2, receive-pack v0), REST v3 under `/api/v3`,
GraphQL v4 at `/api/graphql` and MCP at `/mcp`, all from its own
in-memory store, with no git binary, no outside service and no model
calls.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips
TypeScript types on its own; the flag is harmless there and makes an older
local Node work too. There is nothing to install: the program imports
`node:http`, `node:crypto`, `node:zlib` and its own modules only.

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

- `main.ts`: the lifecycle. Kept as the starter ships it, plus one line.
- `app.ts`: the HTTP server and the git smart-HTTP handlers.
- `src/store.ts`: the in-memory store (repos, git objects, issues, pull
  requests), packfile read/write, and the squash-merge logic.
- `src/git.ts`: pkt-line framing, v2/v0 advertisements, ls-refs, fetch and
  receive-pack.
- `src/rest.ts`: the REST v3 routes.
- `src/graphql.ts`: a small GraphQL parser and executor for the checked
  documents.
- `src/mcp.ts`: the MCP endpoint and its six tools.
- `package.json`, `tsconfig.json`, `LICENSE` (MIT): the entry's metadata
  and licence.

## Determinism

All timestamps are the fixed trial instant, all ids are sequential counters
and all orderings are stable, so two runs on the same requests leave
byte-identical state. No clock reads and no randomness anywhere observable.
