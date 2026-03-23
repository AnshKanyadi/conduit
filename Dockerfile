# syntax=docker/dockerfile:1
# ---- Stage 1: Build --------------------------------------------------------
FROM golang:1.24-alpine AS builder

# Install build tools for CGO_ENABLED=0 static binary.
RUN apk --no-cache add git ca-certificates

WORKDIR /build

# Layer the dependency download separately so module cache is reused when
# only application code changes.
COPY go.mod go.sum ./
RUN go mod download -x

COPY . .

# Build a fully-static binary:
#   -trimpath  removes local filesystem paths from the binary (reproducible)
#   -w -s      strip DWARF and symbol table (smaller binary)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-w -s" \
      -o conduit \
      ./cmd/server

# ---- Stage 2: Runtime -------------------------------------------------------
# Alpine is chosen over distroless because the Conduit server execs /bin/sh
# as a PTY child process — a shell must exist in the final image.
# Alpine weighs ~8 MB and has a minimal attack surface.
FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata && \
    # Create a non-root user; PTY allocation works for unprivileged users on
    # Linux (devpts grants permission on open, not on ownership).
    addgroup -S conduit && \
    adduser  -S -G conduit conduit

COPY --from=builder /build/conduit /conduit

# 8080 is the HTTP/WebSocket port (see cmd/server/main.go).
EXPOSE 8080

USER conduit

ENTRYPOINT ["/conduit"]
