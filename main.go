package main

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"

	_ "modernc.org/sqlite"
	"golang.org/x/crypto/bcrypt"
)

//go:embed ui/index.html
var uiFS embed.FS

type cred struct {
	Username string `json:"username"`
	Password string `json:"password"` // bcrypt hash
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

const rootUsage = `mediad - self-contained media streaming & ingestion engine

Usage:
  mediad [flags]                     run the server (default command)
  mediad adduser <user> <pass> [creds-path]
                                      hash a password and add/update a user in creds.json
  mediad help                        show this help

Flags for "mediad [flags]":
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == "adduser" {
		adduser(os.Args[2:])
		return
	}

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, rootUsage)
		flag.PrintDefaults()
	}
	uploadDir := flag.String("upload-dir", "./uploads", "destination for incoming raw video files")
	servingDir := flag.String("serving-dir", "./stream", "target directory for generated HLS assets")
	dbPath := flag.String("db-path", "./media.db", "path to the sqlite database file")
	credsPath := flag.String("creds-path", "./creds.json", "path to the basic-auth credentials file")
	port := flag.Int("port", 8080, "http listen port")

	if len(os.Args) > 1 && os.Args[1] == "help" {
		flag.Usage()
		return
	}
	flag.Parse()

	for _, d := range []string{*uploadDir, *servingDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("mkdir %s: %v", d, err)
		}
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS videos (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			filename TEXT NOT NULL,
			status TEXT CHECK(status IN ('pending', 'processing', 'ready', 'failed')) DEFAULT 'pending',
			raw_path TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_videos_status ON videos(status);
	`); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	queue := make(chan int64, 100)
	go worker(db, *servingDir, queue)
	reconcile(db, queue)

	creds := loadCreds(*credsPath)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/upload", uploadHandler(db, *uploadDir, queue))
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

func uploadHandler(db *sql.DB, uploadDir string, queue chan<- int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		res, err := db.Exec(`INSERT INTO videos (filename, raw_path) VALUES (?, ?)`, header.Filename, "")
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		id, _ := res.LastInsertId()

		safeName := unsafeName.ReplaceAllString(header.Filename, "_")
		rawPath := filepath.Join(uploadDir, fmt.Sprintf("%d_%s", id, safeName))

		dst, err := os.Create(rawPath)
		if err != nil {
			http.Error(w, "cannot create file", http.StatusInternalServerError)
			return
		}
		defer dst.Close()
		if _, err := io.Copy(dst, file); err != nil {
			http.Error(w, "write failed", http.StatusInternalServerError)
			return
		}

		if _, err := db.Exec(`UPDATE videos SET raw_path = ? WHERE id = ?`, rawPath, id); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}

		queue <- id

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"id": id, "status": "pending"})
	}
}

func listHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`SELECT id, filename, status, created_at FROM videos ORDER BY id DESC`)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type video struct {
			ID        int64  `json:"id"`
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
		}
		items := []video{}
		for rows.Next() {
			var v video
			if err := rows.Scan(&v.ID, &v.Filename, &v.Status, &v.CreatedAt); err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
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

func worker(db *sql.DB, servingDir string, queue <-chan int64) {
	for id := range queue {
		processOne(db, servingDir, id)
	}
}

func processOne(db *sql.DB, servingDir string, id int64) {
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
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "23",
		"-c:a", "aac", "-b:a", "128k",
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
