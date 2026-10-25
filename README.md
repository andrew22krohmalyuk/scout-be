# scout-be

Backend service for Scout. A standard-library HTTP server in Go — no third-party runtime dependencies.

---

## Requirements

| Tool | Version | Required |
|---|---|---|
| [Go](https://go.dev/dl/) | 1.26+ | yes |
| [golangci-lint](https://golangci-lint.run/welcome/install/) | 2.x | optional (linting) |
| [air](https://github.com/air-verse/air) | latest | optional (live reload) |

Check your Go version:

```bash
go version
```

---

## Quick start

```bash
git clone git@github.com-pet:andrew22krohmalyuk/scout-be.git
cd scout-be
go run ./cmd/server
```

The server starts on `http://localhost:8080`. Verify it:

```bash
curl localhost:8080/healthz
# {"status":"ok"}
```

Stop it with `Ctrl+C` — the server drains in-flight requests before exiting (10s max).

---

## Running

### Development (compile and run in one step)

```bash
go run ./cmd/server
```

On a custom port:

```bash
PORT=3000 go run ./cmd/server
```

### From a compiled binary

```bash
go build -o bin/server ./cmd/server
./bin/server
```

`bin/` is git-ignored.

### Live reload (optional)

`air` rebuilds and restarts the server on every file save. It is **not installed** by default:

```bash
go install github.com/air-verse/air@latest   # one-time install
air                                          # then, from the repo root
```

If `air` is not found after installing, add Go's bin directory to your `PATH`:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

---

## Building

| Command | What it does |
|---|---|
| `go build ./...` | Compiles every package, writes no binary. Fastest correctness check. |
| `go build -o bin/server ./cmd/server` | Builds the runnable binary into `bin/server`. |
| `go clean -cache` | Clears the build cache when a rebuild behaves oddly. |

### Rebuilding

Go caches builds, so a plain rebuild is just the same command again:

```bash
go build -o bin/server ./cmd/server
```

Force a full rebuild, ignoring the cache:

```bash
go build -a -o bin/server ./cmd/server
```

### Cross-compiling

Build a Linux binary from macOS (useful for Docker or a server deploy):

```bash
GOOS=linux GOARCH=amd64 go build -o bin/server-linux ./cmd/server
```

Strip debug info for a smaller binary:

```bash
go build -ldflags="-s -w" -o bin/server ./cmd/server
```

---

## Testing

```bash
go test ./...              # run all tests
go test -v ./...           # verbose, shows each test case
go test -race ./...        # with the race detector (slower, catches data races)
go test -cover ./...       # with a coverage summary
```

Run a single test:

```bash
go test -run TestRoutes ./internal/server
```

HTML coverage report:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

---

## Code quality

```bash
gofmt -l .          # lists unformatted files; silence means everything is formatted
gofmt -w .          # formats them in place
go vet ./...        # standard-library static analysis
golangci-lint run   # full linter suite (optional tool)
```

---

## Configuration

Configuration comes from environment variables. Locally they are loaded from a
gitignored `.env` in the repo root — copy the template and fill it in:

```bash
cp .env.example .env
```

`.env` is read at startup relative to the working directory, so start the server
from the repo root (`make run` does). Deployed environments inject the same keys
directly and ship no `.env`; a missing file logs a warning and is not fatal.

| Variable | Default | Description |
|---|---|---|
| `GOOGLE_CLIENT_ID` | *(empty)* | Google OIDC client ID. `/v1/auth/login` returns `500` without it. |
| `GOOGLE_CLIENT_SECRET` | *(empty)* | Google OIDC client secret. Never commit this. |
| `PORT` | `8080` | Port the server binds to. |
| `HOST` | *(empty)* | Interface to bind to. Empty means all interfaces. Set to `127.0.0.1` to accept local connections only. |

Example:

```bash
HOST=127.0.0.1 PORT=3000 go run ./cmd/server
```

---

## API

| Method | Path | Response |
|---|---|---|
| `GET` | `/healthz` | `{"status":"ok"}` — liveness probe |
| `GET` | `/api/v1/ping` | `{"message":"pong"}` |

Routes are method-aware: an unmatched path returns `404`, and a wrong method on a valid path returns `405`.

```bash
curl -i localhost:8080/healthz
curl -i localhost:8080/api/v1/ping
```

---

## Project layout

```
cmd/server/main.go              entrypoint — config, startup, graceful shutdown
internal/server/server.go       routes, handlers, JSON helper, logging middleware
internal/server/server_test.go  route tests
go.mod                          module definition (github.com/andrew22krohmalyuk/scout-be)
```

- **`cmd/server`** — process concerns only: reading env vars, wiring the `http.Server`, handling signals. Add nothing business-related here.
- **`internal/server`** — the HTTP layer. `Server` is the dependency holder; add a database handle, config struct, or API clients as fields on it and they become available to every handler.
- **`internal/`** — packages here cannot be imported by other modules. That is intentional; it keeps the service's internals private.

### Adding a route

1. Register it in `Routes()` in `internal/server/server.go`, using the `"METHOD /path"` pattern.
2. Write the handler as a method on `*Server` so it can reach shared dependencies.
3. Return the response through `s.writeJSON(...)` for consistent content-type and error logging.

---

## Logging

Logs are structured JSON on stdout via `log/slog`, one line per request:

```json
{"time":"2026-08-03T00:14:32+03:00","level":"INFO","msg":"request","method":"GET","path":"/healthz","status":200,"duration":"97.993µs"}
```

Pipe through [`jq`](https://jqlang.github.io/jq/) for readability during development:

```bash
go run ./cmd/server | jq
```

---

## Server behaviour

- **Graceful shutdown.** On `SIGINT` (`Ctrl+C`) or `SIGTERM`, the server stops accepting new connections and gives in-flight requests up to 10 seconds to finish before exiting.
- **Timeouts.** Read, write, idle, and read-header timeouts are set explicitly rather than left unbounded, so a slow or stalled client cannot hold a connection open indefinitely.
- **Exit codes.** `0` on a clean shutdown, `1` on a startup or shutdown failure (the reason is logged at `ERROR`).

---

## Troubleshooting

**`bind: address already in use`** — something is already on the port:

```bash
lsof -i :8080          # find the process
kill <PID>             # stop it
PORT=8081 go run ./cmd/server   # or just use another port
```

**`no required module provides package ...`** — sync the module files:

```bash
go mod tidy
```

**Stale build or unexplained failure** — clear the caches and rebuild:

```bash
go clean -cache -testcache
go build ./...
```

**Import path errors after moving files** — imports are absolute from the module root, so they always start with `github.com/andrew22krohmalyuk/scout-be/`.


## docker compose up --build