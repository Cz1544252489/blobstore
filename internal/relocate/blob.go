package relocate

// blob.go is the blobstore HTTP client: the three authenticated endpoints
// (POST/GET/DELETE /blobs) that move bytes between Trilium and the
// credential-free storage service. The protocol is in the README.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var blobClient = &http.Client{Timeout: 10 * time.Minute}

// postBlob uploads data under noteID and returns the id, sha256 and URL the
// service reports back.
func postBlob(svc Service, noteID, filename string, data []byte, contentType string) (id, sha256Hex, blobURL string, err error) {
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(svc.URL, "/")+"/blobs", bytes.NewReader(data))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+svc.Token)
	req.Header.Set("X-Relocation-Note-Id", noteID)
	req.Header.Set("X-Relocation-Filename", encodeURIComponent(filename))
	req.Header.Set("Content-Type", contentType)
	resp, err := blobClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("连不上迁移服务(%s):%w", svc.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("POST /blobs 失败(%d):%s", resp.StatusCode, trim(body))
	}
	var out struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", "", fmt.Errorf("响应不是合法 JSON:%s", trim(body))
	}
	return out.ID, out.SHA256, out.URL, nil
}

// getBlob retrieves the bytes stored under id, along with the Content-Type
// the service recorded when they were stored.
func getBlob(svc Service, id string) (data []byte, contentType string, err error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(svc.URL, "/")+"/blobs/"+id, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+svc.Token)
	resp, err := blobClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("连不上迁移服务(%s):%w", svc.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET /blobs/%s 失败(%d):%s", id, resp.StatusCode, trim(body))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// deleteBlob removes id's current version. A 404 (id already gone) is not
// treated as an error — the caller only wants the space freed.
func deleteBlob(svc Service, id string) error {
	req, err := http.NewRequest(http.MethodDelete, strings.TrimSuffix(svc.URL, "/")+"/blobs/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+svc.Token)
	resp, err := blobClient.Do(req)
	if err != nil {
		return fmt.Errorf("连不上迁移服务(%s):%w", svc.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("DELETE /blobs/%s 失败(%d):%s", id, resp.StatusCode, trim(body))
	}
	return nil
}

// encodeURIComponent percent-encodes s the way JavaScript's
// encodeURIComponent does. blobstore's handler decodes this header with
// url.PathUnescape rather than url.QueryUnescape, because the latter turns
// "+" into a space, which encodeURIComponent never produces.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if isUnreservedURIComponent(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreservedURIComponent(c byte) bool {
	if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
		return true
	}
	switch c {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}
