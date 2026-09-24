# Contributing

Thanks for looking. This is a small Go service; the bar is "boring, tested,
and hard to misuse."

## Set up

```sh
go build ./... && go vet ./... && go test ./... -race
```

You need Go (version in `go.mod`), `ripgrep`, and `git`. Tests never touch a
real vault: they use `testdata/vault/` (read-only fixture) or copy it into a
temp git repository.

## Before you change anything

Read [`CLAUDE.md`](CLAUDE.md). Its **non-negotiables** are architectural
commitments, not preferences — for example: the model can never raise a note's
tier, the JSON API never writes, no MCP tool exposes git push/commit, and the
renderer treats every note body as hostile. A change that would violate one
should start as an issue, not a pull request. [`spec/tiers.md`](spec/tiers.md)
is the design of the tier system.

## Conventions

- Handlers do HTTP only; logic lives in `internal/vault` and `internal/search`
  and is testable without a server.
- Errors are values, wrapped with `%w`. No panics outside `main`.
- Every exported function has a doc comment saying what it does **and what it
  assumes**.
- Table-driven tests. New behaviour needs a test; a fixed bug needs the test
  that would have caught it.
- Anything touching a path from a request goes through `vault.SafeRequestPath`
  (notes) or `setup.Browser.Resolve` (the setup page's folder picker) — never a
  hand-rolled check. Keep their adversarial tests passing.
- No new dependency without saying why in the pull request.
- Concise commits, imperative mood, no emoji.

## Security-sensitive areas

Changes to any of these get extra scrutiny, and should say in the description
how they were tested: `internal/vault/write.go`, `human.go`, `index.go`
(the `asil` lock), `internal/session`, `internal/setup/browser.go`,
`internal/render` (CSP, CSRF, promotion rules), and `internal/mcp/oauth.go`.
Please report vulnerabilities privately — see [`SECURITY.md`](SECURITY.md).
