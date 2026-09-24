# Security

## Reporting a vulnerability

Please **do not open a public issue** for a security problem. Use GitHub's
private vulnerability reporting ("Security" tab → "Report a vulnerability") on
this repository, or contact the maintainer directly. Include what you did, what
you expected, and what happened. You will get an acknowledgement, and a fix or
an explanation, as promptly as one person can manage.

## What this software is trusting, and what it is not

Null gives a language model read and write access to a directory of notes. It
is built on the assumption that **the model, and any text it writes, is not
trusted**:

- The model can never raise a note's curation tier, and has no push or commit
  tool. These are enforced in server code (`internal/vault`), not by
  instructions to the model.
- Note bodies can contain arbitrary HTML and are rendered by the web reader.
  Every page is served with a strict Content-Security-Policy; write forms carry
  a CSRF token and a same-origin check; and first-open promotion only counts a
  genuine, user-initiated page load.
- Every path from a request goes through one function (`vault.SafeRequestPath`)
  that rejects traversal, absolute paths, hidden files and symlink escapes.
  The setup page's folder picker is confined to `NULL_BROWSE_ROOT` the same way.

## What you must do when deploying

- **Use two different tokens** — `NULL_TOKEN` (API) and `NULL_UI_TOKEN` (browser,
  Al-Mina, setup) — whenever a program or model holds the API token. With one
  shared token, that program can log in to the UI and approve its own tier
  proposals. The server warns at boot when they are the same.
- **Put TLS in front.** The binaries speak plain HTTP and bind where you tell
  them; terminate TLS in a reverse proxy. The login cookie is marked `Secure`
  only when it sees HTTPS (directly or via `X-Forwarded-Proto`).
- **Treat `NULL_BROWSE_ROOT` as the boundary** of what a browser session can
  point the server at. Do not set it to `/`.
- The vault is mounted read-write on purpose (every write is a git commit). Keep
  it a git repository, back it up, and push it yourself — the server never
  pushes.

## Known limits

Tokens are static bearer secrets with no rotation or per-user identity. There is
no rate limiting. This is a single-user tool.
