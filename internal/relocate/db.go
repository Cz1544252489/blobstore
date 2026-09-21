package relocate

// db.go reads Trilium's document.db read-only through a short Python
// script. blobstore uses only the standard library and add no
// third-party sqlite driver, and Python's own sqlite3 module already covers
// this without one. The connection is always opened with the sqlite3 URI
// "mode=ro", so a mistaken write in a query string fails instead of
// touching the live database.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const defaultDBPath = `D:\TriliumNotes-data\trilium-data\document.db`

// runPython feeds a script to the configured Python interpreter over stdin,
// which keeps the script out of the command line and its length limits.
func runPython(pythonBin, script string) (string, error) {
	cmd := exec.Command(pythonBin, "-")
	cmd.Stdin = strings.NewReader(script)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String() + " " + out.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("本机 Python 失败:%s", msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// queryDB runs one read-only SELECT against dbPath and returns its rows.
func queryDB(dbPath, pythonBin, sql string, args []any) ([]map[string]any, error) {
	if args == nil {
		args = []any{}
	}
	payload, err := json.Marshal(map[string]any{"db": dbPath, "sql": sql, "args": args})
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(`
import sqlite3, json, base64, pathlib
req = json.loads(base64.b64decode("%s").decode("utf-8"))
uri = pathlib.Path(req["db"]).resolve().as_uri() + "?mode=ro"
con = sqlite3.connect(uri, uri=True)
con.row_factory = sqlite3.Row
cur = con.execute(req["sql"], req.get("args") or [])
rows = [dict(r) for r in cur.fetchall()] if cur.description else []
print(json.dumps(rows, ensure_ascii=False))
`, base64.StdEncoding.EncodeToString(payload))

	out, err := runPython(pythonBin, script)
	if err != nil {
		return nil, err
	}
	// The script prints exactly one JSON line; ignore anything before it.
	if i := strings.LastIndex(out, "\n"); i >= 0 {
		out = out[i+1:]
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("数据库查询返回的不是 JSON:%s", out)
	}
	return rows, nil
}

// Service is one entry of the config note's services list.
type Service struct {
	ID    string
	Name  string
	URL   string
	Token string
}

// Config is what the note labelled #relocationConfig holds, the same note the
// Trilium widget reads. LocationLabel is empty when none is configured or the
// configured name is not one a label can carry.
type Config struct {
	Services      []Service
	DefaultID     string
	LocationLabel string
}

var attributeName = regexp.MustCompile(`^[\p{L}\p{N}_:]+$`)

// readConfig finds the #relocationConfig note over ETAPI and parses it. The
// tokens never get printed by any caller of this function.
func readConfig() (Config, error) {
	notes, err := searchNotes("#relocationConfig")
	if err != nil {
		return Config{}, err
	}
	if len(notes) == 0 {
		return Config{}, fmt.Errorf("找不到带 #relocationConfig 标签的配置笔记;先在 Trilium 里打开一篇文件笔记,插件会建一篇")
	}
	raw, err := getContent(notes[0].NoteID)
	if err != nil {
		return Config{}, fmt.Errorf("读配置笔记 %s 失败:%w", notes[0].NoteID, err)
	}
	var parsed struct {
		Default       string `json:"default"`
		LocationLabel string `json:"locationLabel"`
		Services      []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			URL   string `json:"url"`
			Token string `json:"token"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Config{}, fmt.Errorf("配置笔记 %s 不是合法 JSON:%w", notes[0].NoteID, err)
	}
	cfg := Config{DefaultID: parsed.Default}
	if label := strings.TrimSpace(parsed.LocationLabel); attributeName.MatchString(label) {
		cfg.LocationLabel = label
	}
	for _, p := range parsed.Services {
		if p.ID == "" {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		u := strings.TrimSpace(p.URL)
		if !strings.HasSuffix(u, "/") {
			u += "/"
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return Config{}, fmt.Errorf("服务 %s 的地址没有填写或不是 http(s) 地址", name)
		}
		if p.Token == "" {
			return Config{}, fmt.Errorf("服务 %s 的令牌没有填写", name)
		}
		cfg.Services = append(cfg.Services, Service{ID: p.ID, Name: name, URL: u, Token: p.Token})
	}
	return cfg, nil
}

// selectService picks the service move should use: the one named (by name or
// id), otherwise the config's default, otherwise the first.
func selectService(cfg Config, name string) (Service, error) {
	if len(cfg.Services) == 0 {
		return Service{}, fmt.Errorf("配置笔记里没有任何服务")
	}
	if name != "" {
		for _, s := range cfg.Services {
			if s.Name == name || s.ID == name {
				return s, nil
			}
		}
		return Service{}, fmt.Errorf("没有名为 %q 的服务", name)
	}
	for _, s := range cfg.Services {
		if s.ID == cfg.DefaultID {
			return s, nil
		}
	}
	return cfg.Services[0], nil
}

// selectServiceByID looks up the service a relocated note's relocationTarget
// label points at, for back and status -verify.
func selectServiceByID(services []Service, id string) (Service, error) {
	for _, s := range services {
		if s.ID == id {
			return s, nil
		}
	}
	return Service{}, fmt.Errorf("relocationTarget=%s 不在配置笔记列出的服务里", id)
}

// PDFNote is one PDF file note's size record, read from the notes/blobs join.
type PDFNote struct {
	NoteID    string
	Title     string
	Size      int64
	Protected bool
}

// pdfNoteList lists every PDF file note still stored inside Trilium.
// Whether a note is protected (unreadable through ETAPI) is reported per
// row so callers can warn or skip it without a second round trip.
func pdfNoteList(dbPath, pythonBin string) ([]PDFNote, error) {
	rows, err := queryDB(dbPath, pythonBin, `
SELECT n.noteId AS noteId, n.title AS title, n.isProtected AS isProtected,
       length(b.content) AS size
FROM notes n JOIN blobs b ON n.blobId = b.blobId
WHERE n.type='file' AND n.mime='application/pdf' AND n.isDeleted=0
`, nil)
	if err != nil {
		return nil, err
	}
	notes := make([]PDFNote, 0, len(rows))
	for _, r := range rows {
		notes = append(notes, PDFNote{
			NoteID:    strVal(r["noteId"]),
			Title:     strVal(r["title"]),
			Size:      intVal(r["size"]),
			Protected: truthy(r["isProtected"]),
		})
	}
	return notes, nil
}

func strVal(v any) string {
	s, _ := v.(string)
	return s
}

func intVal(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

func truthy(v any) bool {
	switch t := v.(type) {
	case float64:
		return t != 0
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	case bool:
		return t
	}
	return false
}
