package server

// handlers.go implements the blobstore HTTP protocol: bearer-token-guarded
// /blobs endpoints for a caller that already owns some content (e.g. a
// Trilium note being relocated) or wants to see what is stored, plus an
// unauthenticated /files/<id>
// endpoint a browser can load directly — an iframe src cannot carry an
// Authorization header.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

// tenant is one group of files with its own token and store. The default group has an empty name;
// its files keep the /files/<id> addresses they had before groups existed.
type tenant struct {
	name  string
	key   string
	store *Store
}

// server routes the protocol's endpoints; the bearer token decides which tenant they act on.
type server struct {
	tenants    []*tenant
	origins    []string
	publicBase string
	mux        *http.ServeMux
}

func newServer(tenants []*tenant, origins []string, publicBase string) *server {
	s := &server{tenants: tenants, origins: origins, publicBase: publicBase}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/blobs", s.handleBlobs)
	s.mux.HandleFunc("/blobs/", s.handleBlobsID)
	s.mux.HandleFunc("/files/", s.handleFile)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// checkAuth applies the checks every /blobs endpoint needs — CORS origin,
// method, bearer token — and returns the tenant the token belongs to. It
// writes the response itself and returns nil when the caller should stop here
// (including a handled OPTIONS preflight).
func (s *server) checkAuth(w http.ResponseWriter, r *http.Request, method string) *tenant {
	origin := r.Header.Get("Origin")
	known := origin != "" && s.originAllowed(origin)
	if known {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Relocation-Note-Id, X-Relocation-Filename")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", method)
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	if origin != "" && !known {
		writeErr(w, http.StatusForbidden, "来源 "+origin+" 不在允许列表里")
		return nil
	}
	if r.Method != method {
		writeErr(w, http.StatusMethodNotAllowed, "这个接口只接受 "+method)
		return nil
	}
	// Every tenant is compared, so the time taken does not tell which one came close.
	token := []byte(bearerToken(r))
	var found *tenant
	for _, t := range s.tenants {
		if subtle.ConstantTimeCompare(token, []byte(t.key)) == 1 {
			found = t
		}
	}
	if found == nil {
		writeErr(w, http.StatusUnauthorized, "令牌不对")
	}
	return found
}

func (s *server) originAllowed(origin string) bool {
	for _, o := range s.origins {
		if o == origin {
			return true
		}
	}
	return false
}

// bearerToken reads the token out of "Authorization: Bearer <token>", or
// returns "" when the header is missing or malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// handleBlobs dispatches GET (list) and POST (store) on the bare /blobs path.
func (s *server) handleBlobs(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if method == http.MethodOptions {
		method = r.Header.Get("Access-Control-Request-Method")
	}
	if method == http.MethodGet {
		s.handleListBlobs(w, r)
		return
	}
	s.handlePostBlobs(w, r)
}

// handleListBlobs answers with every current record, newest first, and the
// totals over them. Each entry carries the same url POST /blobs handed back.
func (s *server) handleListBlobs(w http.ResponseWriter, r *http.Request) {
	t := s.checkAuth(w, r, http.MethodGet)
	if t == nil {
		return
	}
	list, err := t.store.ListBlobs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var total int64
	blobs := make([]map[string]any, 0, len(list))
	for _, b := range list {
		total += b.Rec.Size
		blobs = append(blobs, map[string]any{
			"id":       b.ID,
			"filename": b.Rec.Filename,
			"mime":     b.Rec.Mime,
			"size":     b.Rec.Size,
			"sha256":   b.Rec.SHA256,
			"storedAt": b.Rec.StoredAt,
			"versions": b.Versions,
			"url":      s.fileURL(t, b.ID, b.Rec.Key),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(blobs), "totalSize": total, "blobs": blobs})
}

// handlePostBlobs stores the request body under the caller-supplied id and
// answers with a URL the caller's UI can open directly.
func (s *server) handlePostBlobs(w http.ResponseWriter, r *http.Request) {
	t := s.checkAuth(w, r, http.MethodPost)
	if t == nil {
		return
	}
	id := strings.TrimSpace(r.Header.Get("X-Relocation-Note-Id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少请求头 X-Relocation-Note-Id")
		return
	}
	if err := validateID(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mime := r.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	// Percent-encoded by the caller: a header value carries Latin-1 only, and file names are not
	// bound by that. A name that was never encoded decodes to itself.
	filename := strings.TrimSpace(r.Header.Get("X-Relocation-Filename"))
	if decoded, err := url.PathUnescape(filename); err == nil {
		filename = decoded
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败:"+err.Error())
		return
	}

	res, err := t.store.StoreBlob(id, mime, filename, data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": res.ID,
		// The caller stores these two: the checksum is the only check spanning the
		// whole round trip, and the size is what it shows while the content is away.
		"sha256": res.Rec.SHA256,
		"size":   res.Rec.Size,
		"url":    s.fileURL(t, res.ID, res.Rec.Key),
	})
}

// fileURL is the address POST /blobs hands back: the tenant's name (none for
// the default tenant) and the id name the file, and the key authorizes reading
// it, since /files carries no Authorization header.
func (s *server) fileURL(t *tenant, id, key string) string {
	prefix := "/files/"
	if t.name != "" {
		prefix += t.name + "/"
	}
	return s.publicBase + prefix + url.PathEscape(id) + "?k=" + url.QueryEscape(key)
}

// handleBlobsID dispatches GET and DELETE under /blobs/<id>; POST has its
// own route at the bare /blobs path and never reaches here.
func (s *server) handleBlobsID(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if method == http.MethodOptions {
		// A CORS preflight names the method it is asking about separately;
		// without it there is nothing to route the OPTIONS request by.
		method = r.Header.Get("Access-Control-Request-Method")
	}
	if method == http.MethodDelete {
		s.handleDeleteBlob(w, r)
		return
	}
	s.handleGetBlob(w, r)
}

func (s *server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	t := s.checkAuth(w, r, http.MethodGet)
	if t == nil {
		return
	}
	id, err := pathArg(r.URL.Path, "/blobs/")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	data, rec, err := t.store.ReadBlob(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "没有这个 id:"+id)
		return
	}
	w.Header().Set("Content-Type", rec.Mime)
	// What this end recorded when the bytes arrived. A caller comparing it against both its own
	// record and the bytes it just received can tell a damaged transfer from a record of its own
	// that has been edited.
	w.Header().Set("X-Blob-Sha256", rec.SHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *server) handleDeleteBlob(w http.ResponseWriter, r *http.Request) {
	t := s.checkAuth(w, r, http.MethodDelete)
	if t == nil {
		return
	}
	id, err := pathArg(r.URL.Path, "/blobs/")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	existed, err := t.store.DeleteBlob(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !existed {
		writeErr(w, http.StatusNotFound, "没有这个 id:"+id)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFile serves one stored file's bytes to a browser opening the URL that
// POST /blobs handed back. An iframe src carries no bearer token, so the URL's
// own key stands in for one; the id alone is the caller's note id and would
// let anyone who reads a note fetch its content. It accepts only a bare id, so
// no path segment can walk it outside the blobs directory.
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "这个接口只接受 GET")
		return
	}
	// /files/<id> is the default tenant's, /files/<tenant>/<id> another's.
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/files/"), "/")
	t := s.tenants[0]
	if name, tail, ok := strings.Cut(rest, "/"); ok {
		t = s.tenantNamed(name)
		rest = tail
	}
	id, err := pathArg(rest, "")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if t == nil {
		writeErr(w, http.StatusNotFound, "没有这个 id:"+id)
		return
	}
	data, rec, err := t.store.ReadBlob(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "没有这个 id:"+id)
		return
	}
	// Answers a wrong key the same way as a missing id, so the URL cannot be
	// used to find out which ids exist.
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("k")), []byte(rec.Key)) != 1 {
		writeErr(w, http.StatusNotFound, "没有这个 id:"+id)
		return
	}
	w.Header().Set("Content-Type", rec.Mime)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// tenantNamed returns the non-default tenant with this name, or nil.
func (s *server) tenantNamed(name string) *tenant {
	for _, t := range s.tenants {
		if t.name != "" && t.name == name {
			return t
		}
	}
	return nil
}

// pathArg reads the single id a path carries after its prefix, rejecting
// anything that is not a bare filename.
func pathArg(urlPath, prefix string) (string, error) {
	id := strings.Trim(strings.TrimPrefix(urlPath, prefix), "/")
	if err := validateID(id); err != nil {
		return "", err
	}
	return id, nil
}

// validateID rejects anything that is not a plain, single-segment name — no
// path separators, no ".." or "." — so an id can never resolve outside the
// two flat storage directories it is joined into.
func validateID(id string) error {
	if id == "" {
		return errors.New("请求路径缺少 id")
	}
	if strings.ContainsAny(id, `/\`) {
		return errors.New("id 不能包含路径分隔符")
	}
	if id != filepath.Base(id) || id == "." || id == ".." {
		return errors.New("id 不合法")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
