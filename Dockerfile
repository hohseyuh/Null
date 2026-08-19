# Build stage: static binary, no CGO. No JS build step — there is no JS.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /nullapi ./cmd/nullapi

# Runtime stage: alpine for ripgrep, which /search shells out to.
FROM alpine:3.20
RUN apk add --no-cache ripgrep ca-certificates \
    && adduser -D -H -u 10001 null
COPY --from=build /nullapi /usr/local/bin/nullapi
USER null
EXPOSE 8080
# The vault must be mounted read-only at /vault (see compose.yaml) —
# belt and braces with the O_RDONLY-only reads in the code.
ENV NULL_VAULT_PATH=/vault NULL_ADDR=:8080
ENTRYPOINT ["nullapi"]
