// Package server is blobstore's storage service: POST a file under a
// caller-chosen id, GET or DELETE it back by that id, list what is stored, and
// hand the caller a URL a browser can open directly. It reads no Trilium
// credential or database of any kind; that is what lets it run on a machine
// the note-moving side does not trust.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const defaultAddr = "127.0.0.1:37862"

// Run starts the service from the BLOBSTORE_* environment and blocks until it fails.
func Run(version string) error {
	root := strings.TrimSpace(os.Getenv("BLOBSTORE_ROOT"))
	if root == "" {
		return errors.New("必须设置 BLOBSTORE_ROOT,指定存放目录(未设置时拒绝启动,不猜一个默认位置)")
	}
	addr := envOr("BLOBSTORE_ADDR", defaultAddr)

	key := os.Getenv("BLOBSTORE_KEY")
	if key == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		key = hex.EncodeToString(buf)
	}
	origins := splitCSV(envOr("BLOBSTORE_ORIGINS", "http://localhost:8080,http://127.0.0.1:8080"))
	publicBase := strings.TrimSuffix(envOr("BLOBSTORE_PUBLIC_BASE", "http://"+addr), "/")

	store, err := openStore(root)
	if err != nil {
		return fmt.Errorf("打开存放目录失败:%w", err)
	}
	extra, err := parseTenants(os.Getenv("BLOBSTORE_TENANTS"), root, key)
	if err != nil {
		return err
	}
	tenants := append([]*tenant{{key: key, store: store}}, extra...)
	srv := newServer(tenants, origins, publicBase, root, version)

	fmt.Printf("blobstore %s\n", version)
	fmt.Printf("  地址   http://%s\n", addr)
	fmt.Printf("  密钥   %s\n", key)
	fmt.Printf("  来源   %s\n", strings.Join(origins, " "))
	fmt.Printf("  存放   %s\n", root)
	fmt.Printf("  公开   %s/files/<id>\n", publicBase)
	for _, t := range extra {
		fmt.Printf("  分组   %s(%s,%s/files/%s/<id>)\n", t.name, t.store.root, publicBase, t.name)
	}
	fmt.Println("  接口:GET /status;POST /blobs;GET /blobs;GET /blobs/<id>;DELETE /blobs/<id>;GET /files/[<分组>/]<id>(无鉴权)")
	fmt.Println("  Ctrl+C 停止")

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpSrv.ListenAndServe()
}

// tenantName limits a group's name to what is safe both as a directory name and as a URL segment.
var tenantName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// parseTenants reads BLOBSTORE_TENANTS ("name:token,name:token") into groups stored under
// <root>/tenants/<name>. Names and tokens must be unique, and no token may equal the default key,
// since the token alone decides which group a request acts on.
func parseTenants(spec, root, defaultKey string) ([]*tenant, error) {
	var out []*tenant
	names := map[string]bool{}
	keys := map[string]bool{defaultKey: true}
	for _, entry := range splitCSV(spec) {
		name, token, ok := strings.Cut(entry, ":")
		name, token = strings.TrimSpace(name), strings.TrimSpace(token)
		if !ok || !tenantName.MatchString(name) {
			return nil, fmt.Errorf("BLOBSTORE_TENANTS 里的 %q 不合法:要写成 名字:令牌,名字只用小写字母、数字、- 与 _", entry)
		}
		if len(token) < 16 || strings.ContainsAny(token, " \t") {
			return nil, fmt.Errorf("分组 %s 的令牌太短或含空白(至少 16 个字符)", name)
		}
		if names[name] {
			return nil, fmt.Errorf("分组 %s 重复", name)
		}
		if keys[token] {
			return nil, fmt.Errorf("分组 %s 的令牌与默认令牌或其他分组相同", name)
		}
		names[name], keys[token] = true, true
		store, err := openStore(filepath.Join(root, "tenants", name))
		if err != nil {
			return nil, fmt.Errorf("打开分组 %s 的存放目录失败:%w", name, err)
		}
		out = append(out, &tenant{name: name, key: token, store: store})
	}
	return out, nil
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
