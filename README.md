# phial

A memory server for coding agents and chat agents. Each memory is one record, a phial: one
durable statement with a kind, a status and a source. The server keeps phials for every machine
and every agent, and serves them over MCP and HTTP.

phial replaces [lard](https://tangled.org/dunkirk.sh/lard). Its record model comes from
[entire-brain](https://github.com/entireio/entire-brain).

Status: design. See [docs/design.md](docs/design.md).
