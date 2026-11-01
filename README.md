# phial

A memory server for coding agents and chat agents. Each memory is one record, a phial: one
durable statement with a kind, a status, a source and the person who wrote it. The server keeps
a store for each user and shared pools for teams, and serves them over MCP, HTTP and a browser
UI. Every change is in a ledger, so a phial, a subject or a pool shows its changes over time as
diffs.

phial replaces [lard](https://tangled.org/dunkirk.sh/lard). Its record model comes from
[entire-brain](https://github.com/entireio/entire-brain). See [docs/design.md](docs/design.md).

## Run it

```sh
export PHIAL_TOKEN=$(openssl rand -hex 32)   # or PHIAL_TOKEN_FILE=/run/agenix/phial-token
go run ./cmd/phial serve                     # listens on 127.0.0.1:7480
```

- MCP: `POST /mcp` with `Authorization: Bearer <token>`.
- Browser UI: `http://127.0.0.1:7480/`. Sign in with the same token.
- Health: `GET /healthz` (no auth).

With `auth.mode = "oauth"` the bearer token is an OAuth access token from your authorization
server, each user gets their own store, and only `auth.allowed_users` get in. Add it to Claude
Code:

```sh
claude mcp add --scope user --transport http --client-id <client> --client-secret \
  phial https://phial.hogwarts.dev/mcp
```

## The client

`phial-client` runs on each machine. It logs in with the device-code grant, uploads Claude Code
sessions for extraction, links them to the commits made during the session (with their
`Entire-Checkpoint` trailers), sends entire-brain memories one way into phial, checks code
anchors, and serves `brief` and `verify` to a local agent over MCP.

```sh
export PHIAL_CLIENT_ID=<client> PHIAL_CLIENT_SECRET=<secret>
phial-client login --url https://phial.hogwarts.dev --root ~/dev
phial-client sync                 # new sessions since the last sync
phial-client service install      # sync every 5 minutes (launchd or systemd --user)
phial-client verify ~/dev/phial   # check this repo's code anchors
phial-client brief "add a login page"
claude mcp add --scope user phial-local -- phial-client mcp
```

## Commands

```text
phial serve                          run the server
phial import lard --dir DIR          import a lard memory dir or backup tenant (dry run)
phial import lard --url URL          import from a running lard server (dry run)
                  [--user ID] [--token-file F] [--apply]
phial extract [--user ID]            run one extraction pass now
phial users                          list identities with a store, and the pools
phial rank-report [--user ID] [--limit N]
phial purge ID --reason TEXT [--user ID]   remove one phial for good (writes an audit line)
```

## MCP tools

Server: `get_context`, `recall`, `outline`, `read`, `remember`, `supersede`, `retract`,
`confirm`, `changes`, `review`. Client: `brief`, `verify`. There is no delete tool: phials are
kept forever, and old or stale ones rank lower.

## Configuration

The server reads `$PHIAL_CONFIG`, or `~/.config/phial/config.toml`. Environment variables win.

| TOML key | env var | default | description |
| --- | --- | --- | --- |
| `addr` | `PHIAL_ADDR` | `127.0.0.1:7480` | listen address |
| `data_dir` | `PHIAL_DATA_DIR` | `$XDG_DATA_HOME/phial` | `users/<id>/phial.db` and `pools/<name>/phial.db` |
| `public_url` | `PHIAL_PUBLIC_URL` | | external URL; `https://` marks UI cookies Secure |
| `primary_user` | `PHIAL_PRIMARY_USER` | `local` | identity for token sign-ins and the CLI |
| `auth.mode` | `PHIAL_AUTH` | `token` | `token`, `oauth`, or `none` on loopback only |
| `auth.token` | `PHIAL_TOKEN` | | bearer token (mode token) and the UI password, 32+ chars |
| `auth.token_file` | `PHIAL_TOKEN_FILE` | | file that holds the token |
| `auth.allowed_users` | `PHIAL_ALLOWED_USERS` | everyone | identities (OAuth `sub`) that may sign up |
| `auth.issuer` | `PHIAL_OAUTH_ISSUER` | | OIDC issuer (mode oauth) |
| `auth.introspection_url` | `PHIAL_OAUTH_INTROSPECTION_URL` | discovery | RFC 7662 endpoint |
| `auth.client_id` | `PHIAL_OAUTH_CLIENT_ID` | | client for introspection and the UI sign-in |
| `auth.client_secret` | `PHIAL_OAUTH_CLIENT_SECRET` | | its secret (or `client_secret_file`) |
| `auth.accepted_audiences` | `PHIAL_ACCEPTED_AUDIENCES` | | audiences besides `<public_url>/mcp` |
| `auth.ui_login` | `PHIAL_UI_LOGIN` | `token` | `token` or `oauth` (needs `<public_url>/auth/callback` registered) |
| `llm.provider` | `PHIAL_LLM_PROVIDER` | `anthropic` | `anthropic` (the Claude API) or `openai` (any OpenAI-compatible endpoint) |
| `llm.model` | `PHIAL_LLM_MODEL` | | extraction model, e.g. `claude-haiku-5-5`; empty keeps extraction off |
| `llm.base_url` | `PHIAL_LLM_BASE_URL` | OpenRouter for `openai` | endpoint; optional override for `anthropic` |
| `llm.api_key` | `PHIAL_LLM_API_KEY` | | its key (or `api_key_file`) |
| `extract.interval`, `extract.quiet` | | `1m`, `5m` | worker cadence and the quiet period |
| `ranking.*` | | design 7.1 | half-lives, floors and trust values |

A shared pool:

```toml
[[pools]]
name = "crane"
members = ["jsp", "teammate"]      # read and write
readers = ["guest"]            # read only
subjects = ["areas/crane"]     # and its children
```

## Develop

```sh
gofmt -l . && go vet ./... && go test -race ./...
nix build          # the package, with phial and phial-client
```

The UI fonts are IBM Plex Sans and IBM Plex Mono, under the SIL Open Font License
(`internal/web/static/fonts/OFL.txt`).
