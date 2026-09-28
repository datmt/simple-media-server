# Product Requirement Document (PRD)

**Product Name:** Self-Contained Media Streaming & Ingestion Engine (`mediad`)

**Document Status:** Ready for Review

**Target Release:** v1.0.0

---

## 1. Overview & Problem Statement

### 1.1 Context

Serving standard video files (`.mp4`) directly over basic HTTP servers leads to poor user experience:

* Lack of HTTP byte-range (`206 Partial Content`) support prevents scrubbing and seeking.
* Metadata placed at the end of the file (`moov atom`) forces complete downloads before playback starts.
* Traditional full-featured streaming stacks (e.g., Celery + Redis + FastAPI + Nginx + PostgreSQL) require high maintenance overhead, complex deployments, and excessive runtime dependencies.

### 1.2 Objective

Deliver an embeddable, single-binary media server that provides:

1. Resilient chunked video uploads.
2. An automated, background transcode pipeline producing HTTP Live Streaming (`HLS`) assets.
3. Embedded static file serving and persistent state storage with **zero external system dependencies** beyond `ffmpeg`.
4. A lightweight embedded UI for immediate browser-based playback and testing.

---

## 2. Personas & Core User Stories

| Persona | Role | Core Need |
| --- | --- | --- |
| **System Operator / Dev** | Deployer | Needs to run one static binary via CLI/systemd with directory path flags and have it work immediately. |
| **End User / Content Consumer** | Viewer | Needs instant playback, immediate timeline seeking, and zero buffering on mobile/desktop browsers. |
| **Uploader** | Contributor | Needs an interface or direct endpoint to drop video files of various codecs and receive clear status tracking. |

### User Stories

* *As a developer*, I want to pass `--upload-dir`, `--serving-dir`, and `--db-path` so that storage paths integrate cleanly with my local or mounted volume structure.
* *As an uploader*, I want to submit large video files and monitor their state (`pending` $\rightarrow$ `processing` $\rightarrow$ `ready` or `failed`) without holding an open network connection.
* *As a viewer*, I want to jump anywhere on the playback timeline without downloading preceding segments.

---

## 3. Scope & System Architecture

### 3.1 In-Scope (v1.0.0)

* Single executable binary compiled via Go (statically linked, CGO-free).
* Pure-Go embedded database (`modernc.org/sqlite`).
* Background worker pool utilizing local `ffmpeg` CLI.
* Embedded single-page application (HTML/CSS/Vanilla JS + `hls.js`).
* Graceful crash recovery (auto-resuming interrupted tasks on restart).

### 3.2 Out-of-Scope (Future Enhancements)

* Multi-bitrate / adaptive ladder transcoding (e.g., 1080p, 720p, 480p variants).
* User authentication and access control lists (RBAC/API keys).
* Distributed multi-node clustering (v1 is single-node/local disk).
* Webhook callbacks for external service notification.

---

## 4. Technical Architecture & Component Flow

```
                      +---------------------------------------+
                      |             Browser Client            |
                      |  - Embedded HTML UI                   |
                      |  - HLS.js Video Player                |
                      +-------+-----------------------^-------+
                              |                       |
               POST /api/upload                       | GET /stream/{id}/*
                              v                       |
+-----------------------------------------------------+------------------+
| mediad (Single Binary)                                                |
|                                                                        |
|  +--------------------+    +--------------------+    +---------------+ |
|  |   HTTP Endpoint    |--->| SQLite Store       |    | HTTP File     | |
|  |   /api/upload      |    | (media.db)         |    | Server        | |
|  +---------+----------+    +---------+----------+    +-------^-------+ |
|            |                         |                       |         |
|            | Write                   | State                 | Serve   |
|            v                         v                       |         |
|     [Raw Upload Dir]          [Task Queue (Chan)]            |         |
|            |                         |                       |         |
|            | Read                    v                       |         |
|            |               +--------------------+            |         |
|            +-------------->| Transcode Worker   |------------+         |
|                            | (Executes FFmpeg)  | Write HLS            |
|                            +--------------------+                      |
+------------------------------------------------------------------------+

```

---

## 5. Functional Requirements

### 5.1 CLI Specification & Configuration

The binary must accept configuration strictly via command-line flags with sensible defaults:

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--upload-dir` | string | `./uploads` | Destination for incoming raw video files. |
| `--serving-dir` | string | `./stream` | Target directory for generated `.m3u8` and `.ts` files. |
| `--db-path` | string | `./media.db` | Location of the SQLite database file. |
| `--port` | int | `8080` | Port for the HTTP server to bind to. |

*Requirement:* If the targeted directories do not exist on launch, the binary must create them recursively with `0755` permissions.

---

### 5.2 Storage & Database Schema

The database must use SQLite. Tables must be created on startup via automated migration:

```sql
CREATE TABLE IF NOT EXISTS videos (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    filename TEXT NOT NULL,
    status TEXT CHECK(status IN ('pending', 'processing', 'ready', 'failed')) DEFAULT 'pending',
    raw_path TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_videos_status ON videos(status);

```

---

### 5.3 Ingestion & API Endpoints

#### `POST /api/upload`

* Accepts `multipart/form-data` with key `file`.
* Streams payload directly to disk under `--upload-dir` as `{video_id}_{sanitized_filename}`.
* Max memory usage for parsing: $\le 32\text{ MB}$.
* Inserts DB row with status `pending`.
* Sends task ID to internal processing queue.
* **HTTP Response:** `202 Accepted`
```json
{
  "id": 1,
  "status": "pending"
}

```



#### `GET /api/videos`

* Returns an array of video items sorted by `id DESC`.
* **HTTP Response:** `200 OK`
```json
[
  {
    "id": 1,
    "filename": "presentation.mp4",
    "status": "ready",
    "created_at": "2026-09-28 05:45:00"
  }
]

```



#### `GET /stream/{id}/...`

* Serves `.m3u8` and `.ts` segments located in `{servingDir}/{id}/`.
* Must return `Accept-Ranges: bytes` and correct MIME types:
* `.m3u8` $\rightarrow$ `application/vnd.apple.mpegurl`
* `.ts` $\rightarrow$ `video/mp2t`



#### `GET /`

* Serves the embedded HTML/JavaScript user interface directly from binary memory.

---

### 5.4 Transcoding Pipeline (`ffmpeg`)

1. **Queue Management:** An in-memory buffered channel (depth: 100) feeds a background worker.
2. **State Updates:**
* Worker picks job: updates DB status to `processing`.
* On exit code `0`: updates status to `ready`.
* On non-zero exit code: updates status to `failed`.


3. **Execution Command:**
```bash
ffmpeg -y -i <raw_path> \
  -c:v libx264 -preset ultrafast -crf 23 \
  -c:a aac -b:a 128k \
  -hls_time 4 \
  -hls_playlist_type vod \
  -hls_segment_filename <serving_dir>/<id>/chunk_%03d.ts \
  <serving_dir>/<id>/playlist.m3u8

```



---

## 6. Non-Functional Requirements

### 6.1 Reliability & Crash Recovery

* **Reconciliation Loop:** On startup, the system must query:
```sql
SELECT id FROM videos WHERE status IN ('pending', 'processing');

```


Any matching jobs must be automatically re-enqueued to ensure no data loss on unexpected process termination.

### 6.2 Performance & Resource Management

* **Binary Footprint:** Compiled binary size must remain under $30\text{ MB}$.
* **Zero-Copy Serving:** Segment streaming must leverage kernel-level transfer (`io.Copy` / `http.FileServer`).
* **Memory Ceiling:** Memory overhead from the server itself (excluding active FFmpeg processes) must remain under $50\text{ MB}$ under regular operation.

### 6.3 Dependencies

* **Runtime:** `ffmpeg` must be available in system `$PATH`.
* **Build-Time:** No CGO requirement (`CGO_ENABLED=0`).

---

## 7. Verification & Acceptance Criteria

| ID | Test Scenario | Acceptance Criteria |
| --- | --- | --- |
| **AC-01** | Binary Execution | `./media --port=9000` launches without external SQLite shared libraries installed on the host. |
| **AC-02** | File Upload | A $1\text{ GB}$ file upload finishes without throwing Out-Of-Memory (OOM) errors. |
| **AC-03** | Status Transition | UI reflects `pending` $\rightarrow$ `processing` $\rightarrow$ `ready` within poll window without manual refresh. |
| **AC-04** | Scrubbing Latency | Clicking forward past the buffered window in the player resumes playback in $< 300\text{ ms}$. |
| **AC-05** | Interruption Recovery | Killing the process mid-transcode (`SIGKILL`) and restarting it re-triggers the transcode job to completion. |

---

## 8. Rollout & Future Roadmap

* **Phase 1 (v1.0 - Current Spec):** Core ingestion, SQLite queue, single-bitrate HLS, embedded UI.
* **Phase 2 (v1.1):** Hardware-accelerated transcode detection (`h264_nvenc`, `h264_videotoolbox`, `h264_qsv`).
* **Phase 3 (v1.2):** Basic auth layer and automatic disk-cleanup policy (purging raw inputs once HLS conversion succeeds).
