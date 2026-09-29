# AGENTS.md

`mediad`: single static Go binary. Recursive library scan, on-demand `ffmpeg` HLS
transcode, upload, embedded UI, basic auth. Only runtime dependency is `ffmpeg` on `$PATH`.

## Layout

| Path | Purpose |
| --- | --- |
| `main.go` | Entire server (~430 lines): CLI, HTTP handlers, job queue, worker |
| `ui/index.html` | Upload/playback UI, embedded into the binary, served at `GET /` |
| `prd.md` | Original product requirements |
| `build.sh` | Local build for host OS/arch -> `./mediad`, warns if >30MB |
| `release.sh` | Cross-compiled tarballs -> `dist/<version>/` (linux+darwin, amd64+arm64) |
| `install.sh` | `curl \| bash` installer, fetches the `mediad-linux-amd64` release asset |
| `.github/workflows/build.yml` | CI: build + 30MB size check; on `v*` tag, attach `mediad-linux-amd64` to the release |
| `README.md` | User-facing install and usage docs |

## main.go map

- `main`: dispatches subcommands (`adduser`, `version`/`-v`/`--version`, `help`), else parses flags, opens the DB, migrates, then starts the worker and the HTTP server.
- Startup order: migrate -> `go worker` -> `go prober` -> `scan` -> `reconcile` -> listen. A ticker (`--scan-interval`, default 5m) then calls `scan(false)`.
- Routes (Go 1.22 `ServeMux` patterns):
  - `POST /api/upload` -> `library.uploadHandler` (saves to `<library>/uploads`)
  - `POST /api/scan` -> `library.scan`
  - `POST /api/optimize` -> `optimizeHandler` (`new|failed` -> `pending`, enqueue)
  - `GET /api/raw/{id}` -> `rawHandler` (original file, path looked up by id)
  - `GET /api/videos` -> `listHandler`
  - `GET /stream/` -> `streamHandler` (serves the HLS dir)
  - `GET /{$}` -> `indexHandler`
- `basicAuth` wraps the mux. `loadCreds` reads `creds.json` (bcrypt hashes); no creds means auth is disabled with a warning.
- Job pipeline: `optimizeHandler` marks selected rows `pending`, pushes the id to a buffered channel (`queue`, cap 100), and `worker` runs `processOne` serially. `processOne` runs `pending -> processing -> ready|failed` via `ffmpeg` (libx264/aac, mobile-first: 360p, crf 28, ultrafast, tunable via `--optimize-*` flags; 4s HLS segments) into `<serving-dir>/<id>/`.
- `reconcile`: re-enqueues rows left `pending`/`processing` by a killed run.
- `library.scan`: walks `--library-dir` recursively (skips serving dir), upserts by `raw_path`, resets `ready|failed` -> `new` if size/mtime changed, deletes rows (and HLS dir) for vanished files. Never enqueues. `scan(false)` (ticker) uses `TryLock` and skips if a scan is running; `scan(true)` (startup, `POST /api/scan`) waits. Skips entirely if the library dir is unreadable, so an unmounted drive doesn't wipe the DB.
- `library.prober`: one goroutine, woken by `upsert`, runs `ffprobe` on rows with `probed=0` and stores duration/width/height/vcodec/acodec. `upsert` resets `probed` when size/mtime change. `browserNative` is a container+codec guess behind the UI's "optimize to play" hint.

## Data

- SQLite (`modernc.org/sqlite`, pure Go, no CGO), one table `videos(id, filename, status, raw_path UNIQUE, size, mtime, probed, duration, width, height, vcodec, acodec, created_at, updated_at)`. `filename` is the path relative to the library. `status` is one of `new|pending|processing|ready|failed` (`new` = not optimized, `ready` = HLS exists). Old schema without `probed` is dropped on startup (with the HLS output).
- DSN sets `busy_timeout(5000)` and WAL. WAL adds `media.db-wal` and `media.db-shm` beside the DB. Keep the pragmas, since the worker, scans and the prober write concurrently.
- Files on disk: library in `--library-dir` (required), HLS output in `--serving-dir` (`./stream`), creds in `--creds-path` (`./creds.json`).

## Build, run, release

```sh
./build.sh                      # local binary
./mediad adduser <user> <pass>  # create login
./mediad --port=8080            # run
```

- Version is injected with `-ldflags "-X main.version=..."`. A plain `go build` reports `dev`.
- Release: push a `v*` tag. CI builds `mediad-linux-amd64` and attaches it to the GitHub release. The asset name must match what `install.sh` downloads. The workflow needs `permissions: contents: write`.
- `install.sh` runs `mediad version` on an existing install to skip a current one. It uses a `timeout` because binaries older than the `version` command start the server instead.

## Conventions

- Keep it one binary, no CGO, under 30MB.
- No test suite yet. Verify by building and running against a temp dir with a few video files in a nested library dir.
