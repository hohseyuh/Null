# Build stage: static binaries, no CGO. No JS build step — the two scripts
# in internal/render/static are hand-written and embedded.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nullapi ./cmd/nullapi
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nullmcp ./cmd/nullmcp

# Runtime stage: alpine for ripgrep (search shells out to it) and git
# (every write is a commit made through it — see internal/vault/write.go).
# One image, both binaries — compose picks which one a service runs via
# entrypoint, so there is exactly one build to keep in sync.
FROM alpine:3.20
RUN apk add --no-cache ripgrep git ca-certificates \
    && adduser -D -H -u 10001 null \
    && mkdir /config && chown null /config
COPY --from=build /out/nullapi /usr/local/bin/nullapi
COPY --from=build /out/nullmcp /usr/local/bin/nullmcp
USER null
EXPOSE 8080
# The vault mount is read-write for both services: nullmcp writes the
# model's notes, and nullapi writes the two human tier actions (Al-Mina,
# first-open promotion) — each as one git commit. NULL_VAULT_PATH is NOT
# baked in: set it to fix the vault, or leave it unset and choose one at
# /setup (saved under /config, which should be a volume).
ENV NULL_ADDR=:8080 NULL_CONFIG_PATH=/config/config.json NULL_BROWSE_ROOT=/vaults
ENTRYPOINT ["nullapi"]
