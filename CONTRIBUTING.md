# Contributing

Thanks for wanting to help make StreamMux better! Here's how to get set up and
what to keep in mind.

## Development setup

Prerequisites:

- **Go 1.25+**
- **Node.js 20+** and npm
- **FFmpeg** (with `ffprobe`) on your `PATH` — required for remuxing and probing

```bash
# 1. Install dependencies
make deps

# 2. Build the frontend (the Go binary embeds it)
make frontend

# 3. Build the Go binary
make backend

# 4. Run it
export SECRET_KEY=your-secret-key
./bin/streammux
```

Or run the server with the frontend dev server:

```bash
# Terminal 1 — frontend dev server
cd web && npm run dev

# Terminal 2 — Go server (proxies /api and /stream to :3001 in dev)
PORT=3001 go run ./cmd/streammux
```

## Project layout

```
cmd/streammux/        entrypoint
internal/
  application/        business logic (parser, collector, analyzer, muxer, ffmpeg, resolver)
  domain/             models, constants, ports
  infrastructure/     sqlite, crypto, store
  interface/http/     HTTP server and routes
web/                  React + Vite + Tailwind + UntitledUI frontend
```

The architecture is hexagonal / DDD: `domain` defines the core types and ports,
`application` implements the business logic, `infrastructure` provides concrete
implementations (SQLite, crypto, in-memory store), and `interface/http` is the
transport layer.

## Guidelines

- Run `gofmt -w .` and `go vet ./...` before committing.
- Run `go test ./...` and make sure everything passes.
- Keep the parser regexes and language table aligned with the upstream
  AIOStreams parser where behaviour should match.
- Write tests for new matching/parsing logic (see `internal/application/muxer`).

## Commit messages

Use clear, conventional commit messages, e.g.:

```
feat: add duration matching between video and audio sources
fix: correct audio track index for multiaudio files
```
