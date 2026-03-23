# Conduit

A distributed terminal collaboration engine. Open a shell session, share the session ID, and anyone with the URL gets a live synchronized view — no agents, no screen recording, no latency surprises.

Built in Go from scratch, one layer at a time.

---

## What it actually does

When you connect, Conduit spawns a real PTY on the server, wraps it in a binary-framed WebSocket protocol, and fans the output to every subscriber through a broadcast hub with flow control. Sessions survive disconnects — reconnect within 30 seconds and you pick up exactly where you left off. If you're running multiple nodes, session ownership is published to etcd, so any node can redirect an incoming connection to the right host.

The frontend is a React app with xterm.js that reimplements the same binary protocol in JavaScript, byte for byte, so there's no translation layer between the browser and the server.

---

## Architecture

The codebase is split into 8 internal packages that form a strict dependency chain. Nothing in `internal/` is importable from outside the module — Go enforces this at compile time.

```
observability
    └── protocol
    └── pty
    └── session      ← owns the PTY, manages suspend/resume
    └── relay        ← broadcast hub, EMA flow control, LZ4 compression
    └── replay       ← append-only ring buffer for session replay
    └── consensus    ← etcd lease registry, session ownership
    └── transport    ← WebSocket adapter, ties everything together
```

**protocol** — Every message on the wire is a binary frame: 1-byte type, 2-byte session ID, 8-byte sequence number, 2-byte payload length, N-byte payload, 4-byte CRC32. The sequence number is the source of truth for replay and flow control.

**pty** — On Linux, new sessions get their own PID/mount/UTS/network namespaces with a seccomp allowlist. On macOS it falls back to `creack/pty` without namespaces (the Go runtime doesn't support clone syscalls in a way that plays nicely with `CLONE_NEWPID` on Darwin).

**relay** — The hub runs on a single goroutine. Subscribers get a buffered channel; the hub never touches locks on the hot path. Lag is tracked per-subscriber with an exponential moving average (α=0.1). If a client falls 500 frames behind it gets a sync notification. At 2000 frames it gets dropped.

**replay** — A ring buffer (default 4096 entries) with LZ4-compressed payloads. When a client reconnects and sends a `REPLAY_REQUEST` with a sequence number, the server replays every frame after that point. Sequence 65535 means "start from the beginning."

**consensus** — Each node holds a single etcd lease (15s TTL, renewed every 5s). Session ownership is published under that lease as a key. If a node crashes, the lease expires and the keys evaporate — no manual cleanup. On a handshake miss, the transport looks up the session in etcd and returns a `NACK` with the owning node's address.

---

## Wire protocol

```
 0       1       3       11      13      13+N    13+N+4
 +-------+-------+-------+-------+-------+-------+
 | type  | sess  |  seq  | len   | data  |  crc  |
 +-------+-------+-------+-------+-------+-------+
   1 B     2 B     8 B     2 B     N B     4 B
```

Eight frame types: `KEYSTROKE`, `OUTPUT`, `SYNC`, `ACK`, `NACK`, `HEARTBEAT`, `REPLAY_REQUEST`, `REPLAY_FRAME`. The JS client uses a 256-entry CRC32-IEEE lookup table and `DataView` for big-endian reads — same as the Go side.

---

## Getting started

**Local (single node, no etcd):**

```bash
git clone https://github.com/anshk/conduit
cd conduit
make dev
```

This starts the server on `:8080` and the frontend on `:3000`. Open `http://localhost:3000`, click Connect, and you get a shell.

**Local (multi-node with etcd):**

```bash
docker compose up
```

Server at `:8080`, frontend at `:3000`. Set `CONDUIT_ETCD_ENDPOINTS=localhost:2379` to enable distributed mode.

**Running the tests:**

```bash
export PATH="$HOME/go/bin:$PATH"
go test ./... -race
```

PTY and namespace tests require Linux. The macOS test suite uses Darwin stubs and passes on both platforms.

---

## Project structure

```
cmd/server/          entry point, wires all packages together
internal/
  consensus/         etcd lease registry
  observability/     zap logger, OTel tracer, Prometheus metrics
  protocol/          binary frame encoding/decoding
  pty/               PTY spawning, namespace isolation (Linux)
  relay/             broadcast hub, flow control
  replay/            append-only session log
  session/           session state machine
  transport/         WebSocket adapter, handshake FSM
web/                 React + xterm.js frontend
deploy/terraform/    AWS (ECS Fargate, ALB, etcd on EC2, CloudFront)
.github/workflows/   CI: test → build → push to GHCR on main
```

---

## Deployment

The Terraform in `deploy/terraform/` targets AWS. It provisions a VPC, an ALB with sticky sessions (client IP hash), ECS Fargate tasks for the server, an EC2 t3.small for etcd, and a CloudFront distribution backed by S3 for the frontend.

```bash
make tf-init
make tf-plan
make deploy
```

The `outputs.tf` prints a `deploy_commands` quickref with the exact `docker push` and ECS task commands.

Server image builds from the multi-stage `Dockerfile` (golang:1.24-alpine → alpine:3.21, static binary, non-root user). Frontend builds in Node and gets served by nginx with `envsubst` for runtime environment variables.

---

## Observability

Every request path is instrumented. The server exposes `/metrics` in Prometheus format. Spans are emitted on handshake and replay via the OpenTelemetry API (no-op by default; wire in an SDK exporter to send to Jaeger or OTLP). Structured logs come from zap — set `CONDUIT_DEV=1` for a human-readable console format.

Eight metrics: active sessions, active subscribers, frames sent/dropped, hub lag, replay length, handshake latency, session lifetime.

---

## Performance

The hub benchmark sits at **4–9M frames/sec** on an M-series Mac with 10 concurrent subscribers. The main costs are channel sends and LZ4 compression on large payloads — the hub goroutine itself has zero allocations per frame dispatch.

---

## Dependencies

Nothing was added without a specific reason:

| Library | Why |
|---|---|
| `gorilla/websocket` | WebSocket upgrade + binary message framing |
| `creack/pty` | PTY allocation on macOS; `StartWithSize` avoids Ctty wiring issues |
| `pierrec/lz4/v4` | Fast compression for relay payloads and replay entries |
| `go.uber.org/zap` | Structured logging with zero allocs on the hot path |
| `prometheus/client_golang` | `/metrics` endpoint for scraping |
| `go.opentelemetry.io/otel` | Trace API only (SDK wired in Layer 10) |
| `go.etcd.io/etcd/client/v3` | Distributed session ownership and node registry |

---

## License

MIT
