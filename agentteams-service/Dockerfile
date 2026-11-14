# syntax=docker/dockerfile:1

# Build stage: the module pins go 1.26.11; the base is pinned to the same
# patch level so the build never relies on GOTOOLCHAIN auto-fetch.
FROM golang:1.26.11 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/agentteams-service ./cmd/agentteams-service

# Runtime stage: static binary, non-root, no shell tooling.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 -h /nonexistent -s /sbin/nologin app
COPY --from=build /out/agentteams-service /usr/local/bin/agentteams-service
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/agentteams-service"]
