package relocate

// etapi.go is the Trilium ETAPI client: reading and writing one note's type,
// content and labels. Notes carry an IsProtected field because relocate must
// refuse to touch a protected note's content — ETAPI cannot read it while the
// protected session is closed.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Trilium mangles the bytes unless this exact content type is used.
	binaryCT = "application/octet-stream"
)

// etapiBase points at the Trilium instance to work against.
var etapiBase = envOr("RELOCATE_ETAPI", "http://127.0.0.1:37840/etapi")

var httpClient = &http.Client{Timeout: 10 * time.Minute}
var etapiToken string

type Attribute struct {
	AttributeID string `json:"attributeId"`
	NoteID      string `json:"noteId"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Value       string `json:"value"`
}

type Note struct {
	NoteID      string      `json:"noteId"`
	Title       string      `json:"title"`
	Type        string      `json:"type"`
	Mime        string      `json:"mime"`
	IsProtected bool        `json:"isProtected"`
	Attributes  []Attribute `json:"attributes"`
}

// loadToken digs the ETAPI token out of the Trilium MCP server entry in
// ~/.claude.json, so the token lives in one place on this machine.
func loadToken() error {
	// An explicit token wins: it is how a non-default instance gets addressed.
	if tok := strings.TrimSpace(os.Getenv("RELOCATE_ETAPI_TOKEN")); tok != "" {
		etapiToken = tok
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return fmt.Errorf("读不到 ~/.claude.json:%w", err)
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("解析 ~/.claude.json 失败:%w", err)
	}
	tok := findToken(root)
	if tok == "" {
		return fmt.Errorf("在 ~/.claude.json 里找不到 trilium 的 ETAPI 令牌")
	}
	etapiToken = tok
	return nil
}

func findToken(node any) string {
	m, ok := node.(map[string]any)
	if !ok {
		return ""
	}
	if servers, ok := m["mcpServers"].(map[string]any); ok {
		if tri, ok := servers["trilium"].(map[string]any); ok {
			if h, ok := tri["headers"].(map[string]any); ok {
				if a, ok := h["Authorization"].(string); ok {
					return a
				}
			}
		}
	}
	for _, v := range m {
		if got := findToken(v); got != "" {
			return got
		}
	}
	return ""
}

func etapi(method, path string, body []byte, ctype string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, etapiBase+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", etapiToken)
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("连不上 Trilium(%s),它在运行吗?%w", etapiBase, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

func etapiJSON(method, path string, payload any) (int, []byte, error) {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return 0, nil, err
		}
	}
	return etapi(method, path, body, "application/json")
}

func getNote(noteID string) (*Note, error) {
	st, body, err := etapi("GET", "/notes/"+noteID, nil, "")
	if err != nil {
		return nil, err
	}
	if st >= 400 {
		return nil, fmt.Errorf("读取笔记 %s 失败(%d):%s", noteID, st, trim(body))
	}
	var n Note
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// searchNotes runs a Trilium search and returns the matching notes with their attributes.
func searchNotes(query string) ([]Note, error) {
	q := url.Values{"search": {query}, "fastSearch": {"true"}, "limit": {"100000"}}
	st, body, err := etapi("GET", "/notes?"+q.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	if st >= 400 {
		return nil, fmt.Errorf("搜索 %q 失败(%d):%s", query, st, trim(body))
	}
	var res struct {
		Results []Note `json:"results"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return res.Results, nil
}

func getContent(noteID string) ([]byte, error) {
	st, body, err := etapi("GET", "/notes/"+noteID+"/content", nil, "")
	if err != nil {
		return nil, err
	}
	if st >= 400 {
		return nil, fmt.Errorf("读取内容失败(%d):%s", st, trim(body))
	}
	return body, nil
}

func putContent(noteID string, data []byte, ctype string) error {
	st, body, err := etapi("PUT", "/notes/"+noteID+"/content", data, ctype)
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("写入内容失败(%d):%s", st, trim(body))
	}
	return nil
}

func setNoteType(noteID, typ, mime string) error {
	st, body, err := etapiJSON("PATCH", "/notes/"+noteID, map[string]string{"type": typ, "mime": mime})
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("改笔记类型失败(%d):%s", st, trim(body))
	}
	return nil
}

// ownAttr returns the note's own attribute with this name, ignoring inherited ones.
func ownAttr(n *Note, name string) *Attribute {
	for i := range n.Attributes {
		if n.Attributes[i].Name == name && n.Attributes[i].NoteID == n.NoteID {
			return &n.Attributes[i]
		}
	}
	return nil
}

// ownAttrValue is ownAttr plus the "" default for a missing attribute.
func ownAttrValue(n *Note, name string) string {
	if a := ownAttr(n, name); a != nil {
		return a.Value
	}
	return ""
}

func setLabel(n *Note, name, value string) error {
	if a := ownAttr(n, name); a != nil {
		if a.Value == value {
			return nil
		}
		st, body, err := etapiJSON("PATCH", "/attributes/"+a.AttributeID, map[string]string{"value": value})
		if err != nil {
			return err
		}
		if st >= 400 {
			return fmt.Errorf("更新属性 %s 失败(%d):%s", name, st, trim(body))
		}
		return nil
	}
	st, body, err := etapiJSON("POST", "/attributes", map[string]any{
		"noteId": n.NoteID, "type": "label", "name": name,
		"value": value, "isInheritable": false,
	})
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("新建属性 %s 失败(%d):%s", name, st, trim(body))
	}
	return nil
}

func deleteLabel(n *Note, name string) error {
	a := ownAttr(n, name)
	if a == nil {
		return nil
	}
	st, body, err := etapi("DELETE", "/attributes/"+a.AttributeID, nil, "")
	if err != nil {
		return err
	}
	if st >= 400 {
		return fmt.Errorf("删除属性 %s 失败(%d):%s", name, st, trim(body))
	}
	return nil
}

func trim(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
