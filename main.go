package main

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed ui/index.html
var uiFS embed.FS

type cred struct {
	Username string `json:"username"`
	Password string `json:"password"` // bcrypt hash
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z"
// (build.sh / release.sh do this); "dev" for a plain `go build`.
var version = "dev"

const rootUsage = `mediad - self-contained media streaming & ingestion engine

Usage:
  mediad [flags]                     run the server (default command)
  mediad adduser <user> <pass> [creds-path]
                                      hash a password and add/update a user in creds.json
  mediad version                     print the version
  mediad help                        show this help

Flags for "mediad [flags]":
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "adduser" {
		adduser(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-v" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, rootUsage)
		flag.PrintDefaults()
	}
	libDir := flag.String("library-dir", "", "media library root, scanned recursively (required); uploads land in <library-dir>/uploads")
	servingDir := flag.String("serving-dir", "./stream", "target directory for generated HLS assets")
	dbPath := flag.String("db-path", "./media.db", "path to the sqlite database file")
	credsPath := flag.String("creds-path", "./creds.json", "path to the basic-auth credentials file")
	port := flag.Int("port", 8080, "http listen port")
	optHeight := flag.Int("optimize-height", 360, "optimized video height in px (never upscales)")
	optCRF := flag.Int("optimize-crf", 28, "x264 quality, 18 (best) to 35 (smallest)")
	optPreset := flag.String("optimize-preset", "ultrafast", "x264 speed preset (ultrafast, veryfast, medium, ...)")
	scanEvery := flag.Duration("scan-interval", 5*time.Minute, "how often to rescan the library (0 disables)")

	if len(os.Args) > 1 && os.Args[1] == "help" {
		flag.Usage()
		return
	}
	flag.Parse()

	if *libDir == "" {
		log.Fatal("--library-dir is required")
	}
	for _, d := range []string{*libDir, *servingDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("mkdir %s: %v", d, err)
		}
	}

	db, err := sql.Open("sqlite", *dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	// Older schemas lack the probe columns: drop the table and its stale HLS output.
	if _, err := db.Exec(`SELECT probed FROM videos LIMIT 0`); err != nil {
		db.Exec(`DROP TABLE IF EXISTS videos`)
		if ents, err := os.ReadDir(*servingDir); err == nil {
			for _, e := range ents {
				os.RemoveAll(filepath.Join(*servingDir, e.Name()))
			}
		}
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS videos (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			filename TEXT NOT NULL,
			status TEXT CHECK(status IN ('new', 'pending', 'processing', 'ready', 'failed')) DEFAULT 'new',
			raw_path TEXT NOT NULL UNIQUE,
			size INTEGER NOT NULL DEFAULT 0,
			mtime INTEGER NOT NULL DEFAULT 0,
			probed INTEGER NOT NULL DEFAULT 0,
			duration REAL NOT NULL DEFAULT 0,
			width INTEGER NOT NULL DEFAULT 0,
			height INTEGER NOT NULL DEFAULT 0,
			vcodec TEXT NOT NULL DEFAULT '',
			acodec TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_videos_status ON videos(status);
	`); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	queue := make(chan int64, 100)
	go worker(db, *servingDir, encodeOpts{*optHeight, *optCRF, *optPreset}, queue)
	lib := &library{db: db, root: *libDir, servingDir: *servingDir, wake: make(chan struct{}, 1)}
	go lib.prober()
	lib.scan(true)
	reconcile(db, queue)
	if *scanEvery > 0 {
		go func() {
			for range time.Tick(*scanEvery) {
				lib.scan(false)
			}
		}()
	}

	creds := loadCreds(*credsPath)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/upload", lib.uploadHandler)
	mux.HandleFunc("POST /api/scan", func(w http.ResponseWriter, r *http.Request) { lib.scan(true); w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/optimize", optimizeHandler(db, queue))
	mux.HandleFunc("GET /api/raw/{id}", rawHandler(db))
	mux.HandleFunc("GET /api/videos", listHandler(db))
	mux.Handle("GET /stream/", http.StripPrefix("/stream/", streamHandler(*servingDir)))
	mux.HandleFunc("GET /{$}", indexHandler)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("mediad listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, basicAuth(creds, mux)))
}

// basicAuth wraps a handler requiring HTTP basic auth against the given
// credential list. Skipped entirely when the creds file has no entries,
// so a fresh checkout with an empty creds.json still runs.
func basicAuth(creds []cred, next http.Handler) http.Handler {
	if len(creds) == 0 {
		log.Println("warning: no credentials loaded, basic auth disabled")
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok {
			for _, c := range creds {
				if subtle.ConstantTimeCompare([]byte(user), []byte(c.Username)) == 1 &&
					bcrypt.CompareHashAndPassword([]byte(c.Password), []byte(pass)) == nil {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="mediad"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

func loadCreds(path string) []cred {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("no creds file at %s (run '%s adduser <user> <pass>' to create one)", path, os.Args[0])
			return nil
		}
		log.Fatalf("read creds: %v", err)
	}
	var creds []cred
	if err := json.Unmarshal(data, &creds); err != nil {
		log.Fatalf("parse creds: %v", err)
	}
	return creds
}

// adduser hashes the given password and upserts {username, hash} into
// creds.json (created if missing). Usage: mediad adduser <user> <pass> [creds-path]
const adduserUsage = `usage: mediad adduser <username> <password> [creds-path]

Hashes <password> with bcrypt and writes/updates the {username, password}
entry in creds.json (default ./creds.json, or [creds-path] if given).
`

func adduser(args []string) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Print(adduserUsage)
		return
	}
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, adduserUsage)
		os.Exit(1)
	}
	username, password := args[0], args[1]
	path := "./creds.json"
	if len(args) > 2 {
		path = args[2]
	}

	var creds []cred
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &creds); err != nil {
			log.Fatalf("parse existing creds: %v", err)
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}

	replaced := false
	for i, c := range creds {
		if c.Username == username {
			creds[i].Password = string(hash)
			replaced = true
			break
		}
	}
	if !replaced {
		creds = append(creds, cred{Username: username, Password: string(hash)})
	}

	out, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		log.Fatalf("marshal creds: %v", err)
	}
	if err := os.WriteFile(path, out, 0600); err != nil {
		log.Fatalf("write creds: %v", err)
	}
	fmt.Printf("user %q written to %s\n", username, path)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	data, _ := uiFS.ReadFile("ui/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

var videoExts = map[string]bool{".mp4": true, ".mkv": true, ".webm": true, ".mov": true, ".avi": true, ".m4v": true}

type library struct {
	db         *sql.DB
	root       string
	servingDir string
	mu         sync.Mutex    // one scan at a time
	wake       chan struct{} // nudges the prober when rows need probing
}

// upsert records a file; a changed size/mtime resets ready/failed to new
// and queues a re-probe.
func (l *library) upsert(path string, info fs.FileInfo) error {
	err := l.upsertRow(path, info)
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return err
}

func (l *library) upsertRow(path string, info fs.FileInfo) error {
	rel, err := filepath.Rel(l.root, path)
	if err != nil {
		return err
	}
	_, err = l.db.Exec(`
		INSERT INTO videos (filename, raw_path, size, mtime) VALUES (?, ?, ?, ?)
		ON CONFLICT(raw_path) DO UPDATE SET
			filename = excluded.filename,
			status = CASE WHEN (size != excluded.size OR mtime != excluded.mtime)
				AND status IN ('ready', 'failed') THEN 'new' ELSE status END,
			probed = CASE WHEN size != excluded.size OR mtime != excluded.mtime THEN 0 ELSE probed END,
			size = excluded.size, mtime = excluded.mtime`,
		rel, path, info.Size(), info.ModTime().Unix())
	return err
}

// scan walks the library, upserts every video, and drops rows whose file is gone.
// block=false (the periodic scan) skips if another scan is still running;
// block=true (startup, manual) waits for it.
func (l *library) scan(block bool) {
	if block {
		l.mu.Lock()
	} else if !l.mu.TryLock() {
		log.Println("scan: previous scan still running, skipping")
		return
	}
	defer l.mu.Unlock()
	// Unmounted/missing library must not look like "every file was deleted".
	if _, err := os.ReadDir(l.root); err != nil {
		log.Printf("scan: library unreadable, skipping: %v", err)
		return
	}
	seen := map[string]bool{}
	filepath.WalkDir(l.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if sameDir(path, l.servingDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !videoExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if err := l.upsert(path, info); err != nil {
			log.Printf("scan: upsert %s: %v", path, err)
			return nil
		}
		seen[path] = true
		return nil
	})

	rows, err := l.db.Query(`SELECT id, raw_path FROM videos`)
	if err != nil {
		log.Printf("scan: query: %v", err)
		return
	}
	var gone []int64
	for rows.Next() {
		var id int64
		var p string
		if rows.Scan(&id, &p) == nil && !seen[p] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		l.db.Exec(`DELETE FROM videos WHERE id = ?`, id)
		os.RemoveAll(filepath.Join(l.servingDir, strconv.FormatInt(id, 10)))
	}
	log.Printf("scan: %d videos, %d removed", len(seen), len(gone))
}

func sameDir(a, b string) bool {
	x, err1 := os.Stat(a)
	y, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(x, y)
}

// uploadHandler stores the file under <library>/uploads and registers it as new.
func (l *library) uploadHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad multipart form", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	name := unsafeName.ReplaceAllString(filepath.Base(header.Filename), "_")
	if !videoExts[strings.ToLower(filepath.Ext(name))] {
		http.Error(w, "unsupported file type", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(l.root, "uploads")
	if err := os.MkdirAll(dir, 0755); err != nil {
		http.Error(w, "cannot create dir", http.StatusInternalServerError)
		return
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		path = filepath.Join(dir, fmt.Sprintf("%d_%s", time.Now().Unix(), name))
	}

	tmp := path + ".part" // not a video ext, so a concurrent scan skips it
	dst, err := os.Create(tmp)
	if err != nil {
		http.Error(w, "cannot create file", http.StatusInternalServerError)
		return
	}
	_, err = io.Copy(dst, file)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(path)
	if err == nil {
		err = l.upsert(path, info)
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// optimizeHandler queues the selected new/failed videos for HLS transcode.
func optimizeHandler(db *sql.DB, queue chan<- int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			IDs []int64 `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		var queued []int64
		for _, id := range req.IDs {
			res, err := db.Exec(`UPDATE videos SET status = 'pending', updated_at = CURRENT_TIMESTAMP
				WHERE id = ? AND status IN ('new', 'failed')`, id)
			if err != nil {
				continue
			}
			if n, _ := res.RowsAffected(); n > 0 {
				queued = append(queued, id)
			}
		}
		go func() { // queue is bounded; don't block the request
			for _, id := range queued {
				queue <- id
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"queued": len(queued)})
	}
}

// rawHandler serves the original file (range requests) for unoptimized playback.
// The path comes from the DB by id, never from the client.
func rawHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p string
		if err := db.QueryRow(`SELECT raw_path FROM videos WHERE id = ?`, r.PathValue("id")).Scan(&p); err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, p)
	}
}

type meta struct {
	Duration       float64
	Width, Height  int
	VCodec, ACodec string
}

// probe reads duration/resolution/codecs with ffprobe; zero meta on failure.
func probe(path string) meta {
	var m meta
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration:stream=codec_type,codec_name,width,height",
		"-of", "json", path).Output()
	if err != nil {
		log.Printf("probe %s: %v", path, err)
		return m
	}
	var p struct {
		Format  struct{ Duration string }
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int
			Height    int
		}
	}
	if json.Unmarshal(out, &p) != nil {
		return m
	}
	m.Duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	for _, s := range p.Streams {
		switch {
		case s.CodecType == "video" && m.VCodec == "":
			m.VCodec, m.Width, m.Height = s.CodecName, s.Width, s.Height
		case s.CodecType == "audio" && m.ACodec == "":
			m.ACodec = s.CodecName
		}
	}
	return m
}

// prober fills in metadata for rows with probed=0, one file at a time, so
// scans stay fast. A file that changes mid-probe is left for the next pass.
func (l *library) prober() {
	for range l.wake {
		for {
			var id, size, mtime int64
			var path string
			if l.db.QueryRow(`SELECT id, raw_path, size, mtime FROM videos WHERE probed = 0 LIMIT 1`).
				Scan(&id, &path, &size, &mtime) != nil {
				break
			}
			m := probe(path)
			if _, err := l.db.Exec(`UPDATE videos SET duration=?, width=?, height=?, vcodec=?, acodec=?, probed=1
				WHERE id=? AND size=? AND mtime=?`,
				m.Duration, m.Width, m.Height, m.VCodec, m.ACodec, id, size, mtime); err != nil {
				log.Printf("probe %d: %v", id, err)
				break
			}
		}
	}
}

// browserNative guesses whether a browser can play the original: known
// container + known codec. Unprobed or unprobeable files count as not native.
func browserNative(path, vcodec string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".webm":
		switch vcodec {
		case "h264", "vp8", "vp9", "av1":
			return true
		}
	}
	return false
}

// listHandler supports ?q= (path substring), ?status=, ?codec=, ?min_height=,
// ?sort=name|mtime|size|added|duration|resolution, ?order=asc|desc.
func listHandler(db *sql.DB) http.HandlerFunc {
	sorts := map[string]string{"name": "filename COLLATE NOCASE", "mtime": "mtime", "size": "size", "added": "id",
		"duration": "duration", "resolution": "height"}
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		where, args := []string{"1=1"}, []any{}
		if s := q.Get("q"); s != "" {
			where = append(where, "filename LIKE ?")
			args = append(args, "%"+s+"%")
		}
		if s := q.Get("status"); s != "" {
			where = append(where, "status = ?")
			args = append(args, s)
		}
		if s := q.Get("codec"); s != "" {
			where = append(where, "vcodec = ?")
			args = append(args, s)
		}
		if h, err := strconv.Atoi(q.Get("min_height")); err == nil {
			where = append(where, "height >= ?")
			args = append(args, h)
		}
		col, ok := sorts[q.Get("sort")]
		if !ok {
			col = "id"
		}
		dir := "DESC"
		if q.Get("order") == "asc" {
			dir = "ASC"
		}
		rows, err := db.Query(`SELECT id, filename, status, size, mtime, duration, width, height, vcodec, acodec, created_at FROM videos WHERE `+
			strings.Join(where, " AND ")+` ORDER BY `+col+` `+dir, args...)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type video struct {
			ID        int64   `json:"id"`
			Title     string  `json:"title"`
			Path      string  `json:"path"`
			Status    string  `json:"status"`
			Size      int64   `json:"size"`
			MTime     int64   `json:"mtime"`
			Duration  float64 `json:"duration"`
			Width     int     `json:"width"`
			Height    int     `json:"height"`
			VCodec    string  `json:"vcodec"`
			ACodec    string  `json:"acodec"`
			Native    bool    `json:"native"`
			CreatedAt string  `json:"created_at"`
		}
		items := []video{}
		for rows.Next() {
			var v video
			if err := rows.Scan(&v.ID, &v.Path, &v.Status, &v.Size, &v.MTime, &v.Duration, &v.Width, &v.Height,
				&v.VCodec, &v.ACodec, &v.CreatedAt); err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			v.Native = browserNative(v.Path, v.VCodec)
			base := filepath.Base(v.Path)
			v.Title = strings.TrimSuffix(base, filepath.Ext(base))
			items = append(items, v)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	}
}

func streamHandler(servingDir string) http.Handler {
	fs := http.FileServer(http.Dir(servingDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Ext(r.URL.Path) {
		case ".m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		case ".ts":
			w.Header().Set("Content-Type", "video/mp2t")
		}
		w.Header().Set("Accept-Ranges", "bytes")
		fs.ServeHTTP(w, r)
	})
}

// reconcile re-enqueues any job left in pending/processing from a prior
// run that was killed mid-flight, so no upload is silently dropped.
func reconcile(db *sql.DB, queue chan<- int64) {
	rows, err := db.Query(`SELECT id FROM videos WHERE status IN ('pending', 'processing')`)
	if err != nil {
		log.Fatalf("reconcile query: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			log.Fatalf("reconcile scan: %v", err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		log.Printf("re-enqueuing interrupted job %d", id)
		queue <- id
	}
}

type encodeOpts struct {
	height, crf int
	preset      string
}

func worker(db *sql.DB, servingDir string, opts encodeOpts, queue <-chan int64) {
	for id := range queue {
		processOne(db, servingDir, opts, id)
	}
}

func processOne(db *sql.DB, servingDir string, opts encodeOpts, id int64) {
	var rawPath string
	if err := db.QueryRow(`SELECT raw_path FROM videos WHERE id = ?`, id).Scan(&rawPath); err != nil {
		log.Printf("job %d: lookup failed: %v", id, err)
		return
	}

	if _, err := db.Exec(`UPDATE videos SET status = 'processing', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		log.Printf("job %d: status update failed: %v", id, err)
		return
	}

	outDir := filepath.Join(servingDir, strconv.FormatInt(id, 10))
	if err := os.MkdirAll(outDir, 0755); err != nil {
		log.Printf("job %d: mkdir failed: %v", id, err)
		fail(db, id)
		return
	}

	cmd := exec.Command("ffmpeg", "-y", "-i", rawPath,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
		"-vf", fmt.Sprintf("scale=-2:'min(%d,ih)'", opts.height),
		"-c:v", "libx264", "-preset", opts.preset, "-crf", strconv.Itoa(opts.crf),
		"-profile:v", "main", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "96k", "-ac", "2",
		"-hls_time", "4",
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", filepath.Join(outDir, "chunk_%03d.ts"),
		filepath.Join(outDir, "playlist.m3u8"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("job %d: ffmpeg failed: %v\n%s", id, err, out)
		fail(db, id)
		return
	}

	if _, err := db.Exec(`UPDATE videos SET status = 'ready', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		log.Printf("job %d: status update failed: %v", id, err)
	}
}

func fail(db *sql.DB, id int64) {
	if _, err := db.Exec(`UPDATE videos SET status = 'failed', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		log.Printf("job %d: fail-status update failed: %v", id, err)
	}
}
