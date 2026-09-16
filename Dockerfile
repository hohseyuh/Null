# Build stage: static binaries, no CGO. No JS build step — there is no JS.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nullapi ./cmd/nullapi
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nullmcp ./cmd/nullmcp

# Runtime stage: alpine for ripgrep (both binaries' search shells out to
# it) and git (nullmcp's write tools commit through it — see
# internal/vault/write.go). One image, both binaries — compose.yaml picks
# which one a given service runs via entrypoint/command, so there is
# exactly one build to keep in sync, not two.
FROM alpine:3.20
RUN apk add --no-cache ripgrep git ca-certificates \
    && adduser -D -H -u 10001 null
COPY --from=build /out/nullapi /usr/local/bin/nullapi
COPY --from=build /out/nullmcp /usr/local/bin/nullmcp
USER null
EXPOSE 8080
# nullapi's vault mount must be read-only (see compose.yaml) — belt and
# braces with the O_RDONLY-only reads in its code path. nullmcp's is not:
# its create_note/write_note/delete_note write there directly and commit
# each change — see CLAUDE.md non-negotiable #2. Default entrypoint is
# nullapi; the nullmcp compose service overrides it.
ENV NULL_VAULT_PATH=/vault NULL_ADDR=:8080
ENTRYPOINT ["nullapi"]
