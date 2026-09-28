# mediad

Self-contained media streaming & ingestion engine. Single static Go binary:
chunked upload, background `ffmpeg` HLS transcode, embedded UI, basic auth.
No external dependencies beyond `ffmpeg` on `$PATH`.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/datmt/simple-media-server/master/install.sh | bash
```

Downloads the latest linux/amd64 release binary to `/usr/local/bin/mediad`
(uses `sudo` if that directory isn't writable). Override the install
location with `INSTALL_DIR`:

```sh
curl -fsSL https://raw.githubusercontent.com/datmt/simple-media-server/master/install.sh | INSTALL_DIR=$HOME/bin bash
```

Requires `ffmpeg` installed separately (`apt install ffmpeg`, `brew install ffmpeg`, ...).

## Usage

```sh
mediad help                       # show all flags
mediad adduser <user> <pass>      # create/update a basic-auth login in creds.json
mediad --port=8080                # run the server
```

Flags (all optional):

| Flag | Default | Description |
| --- | --- | --- |
| `--upload-dir` | `./uploads` | raw uploaded files |
| `--serving-dir` | `./stream` | generated HLS output |
| `--db-path` | `./media.db` | sqlite database |
| `--creds-path` | `./creds.json` | basic-auth credentials |
| `--port` | `8080` | HTTP listen port |

If `creds.json` doesn't exist or is empty, basic auth is disabled and a
warning is logged on startup. Run `mediad adduser <user> <pass>` first to
lock it down.

Open `http://localhost:8080` for the built-in upload/playback UI, or use
the API directly:

- `POST /api/upload` — multipart `file` field, returns `{"id", "status"}`
- `GET /api/videos` — list of videos with status
- `GET /stream/{id}/playlist.m3u8` — HLS playback (byte-range aware)

## Build from source

```sh
git clone https://github.com/datmt/simple-media-server.git
cd simple-media-server
./build.sh              # static binary for the host OS/arch -> ./mediad
./release.sh [version]  # cross-compiled tarballs -> ./dist/<version>/
```
