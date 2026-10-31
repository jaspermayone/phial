# phial

A memory server for coding agents and chat agents. Each memory is one record, a phial: one
durable statement with a kind, a status and a source. The server keeps phials for every machine
and every agent, and serves them over MCP, HTTP and a browser UI.

phial replaces [lard](https://tangled.org/dunkirk.sh/lard). Its record model comes from
[entire-brain](https://github.com/entireio/entire-brain). See [docs/design.md](docs/design.md).

## Run it

```sh
export PHIAL_TOKEN=$(openssl rand -hex 32)   # or PHIAL_TOKEN_FILE=/run/agenix/phial-token
go run ./cmd/phial serve                     # listens on 127.0.0.1:7480
```

- MCP: `POST /mcp` with `Authorization: Bearer $PHIAL_TOKEN`.
- Browser UI: `http://127.0.0.1:7480/`. Sign in with the same token.
- Health: `GET /healthz` (no auth).

Add it to Claude Code:

```sh
claude mcp add --transport http phial https://phial.hogwarts.dev/mcp \
  --header "Authorization: Bearer $PHIAL_TOKEN"
```

## Commands

```text
phial serve                          run the server
phial import lard --dir DIR          import a lard memory dir or backup tenant (dry run)
phial import lard --url URL          import from a running lard server (dry run)
                  [--token-file F]   bearer token for --url (or LARD_TOKEN)
                  [--apply]          write the phials
phial rank-report [--limit N]        active phials with the lowest scores
phial purge ID --reason TEXT         remove one phial for good (writes an audit line)
```

## MCP tools

`get_context`, `recall`, `outline`, `read`, `remember`, `supersede`, `retract`, `confirm`.
There is no delete tool: phials are kept forever, and old or stale ones rank lower.

## Configuration

The server reads `$PHIAL_CONFIG`, or `~/.config/phial/config.toml`. Environment variables win.

| TOML key | env var | default | description |
| --- | --- | --- | --- |
| `addr` | `PHIAL_ADDR` | `127.0.0.1:7480` | listen address |
| `db` | `PHIAL_DB` | `$XDG_DATA_HOME/phial/phial.db` | SQLite file |
| `public_url` | `PHIAL_PUBLIC_URL` | | external URL; `https://` marks UI cookies Secure |
| `auth.mode` | `PHIAL_AUTH` | `token` | `token`, or `none` on a loopback address only |
| `auth.token` | `PHIAL_TOKEN` | | bearer token, 32 characters or more |
| `auth.token_file` | `PHIAL_TOKEN_FILE` | | file that holds the token |
| `ranking.*` | | see design 7.1 | half-lives, floors and trust values |

Ranking example:

```toml
[ranking.kinds.gotcha]
half_life = "90d"
floor = 0.2

[ranking.trust]
stale = 0.25
```

## Develop

```sh
gofmt -l . && go vet ./... && go test -race ./...
```

The UI fonts are IBM Plex Sans and IBM Plex Mono, under the SIL Open Font License
(`internal/web/static/fonts/OFL.txt`).
