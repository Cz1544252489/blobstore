// Package relocate is blobstore's command-line client: it moves a Trilium
// file note's content to or from a blobstore service over ETAPI, doing the
// same bookkeeping as the Trilium widget so a hundred-odd notes can be
// processed at once. It holds the ETAPI token, so the server package never
// imports it.
package relocate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Commands are the subcommands Run handles; main dispatches these here and nothing else.
var Commands = map[string]bool{"list": true, "move": true, "back": true, "status": true, "label": true}

// Run executes one subcommand with its arguments.
func Run(cmd string, args []string) error {
	// list is a pure database read and needs no ETAPI token; every other
	// command reads or writes notes.
	if cmd != "list" {
		if err := loadToken(); err != nil {
			return err
		}
	}
	switch cmd {
	case "list":
		return cmdList(args)
	case "move":
		return cmdMove(args)
	case "back":
		return cmdBack(args)
	case "status":
		return cmdStatus(args)
	case "label":
		return cmdLabel(args)
	}
	return fmt.Errorf("未知命令:%s", cmd)
}

// Usage is the help text for the subcommands, printed as part of blobstore's.
const Usage = `  blobstore list [-min <MB>] [-max <MB>] [-n <篇数>]
      按体积降序列出还在 Trilium 库里的 PDF 笔记(只读数据库,不要令牌)

  blobstore move [-min <MB>] [-n <篇数>] [-service <名字>] [-dry-run] [<noteId>...]
      把选中的笔记搬出;给了 noteId 就只处理这几篇,否则按 -min/-n 选;
      不给 -service 时用配置笔记里的 default

  blobstore back <noteId>...
      取回到 Trilium 库

  blobstore status [-verify]
      列出已经迁移出去的笔记;-verify 逐篇核对服务上的字节

  blobstore label [-dry-run]
      按笔记现在的位置写位置标签(配置里的 locationLabel):在库里的写 true,已迁移的写 false
`

// envOr reads a setting from the environment, falling back when it is unset or blank.
func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// mb formats a byte count for progress and listing output. Note that this is
// binary megabytes, unrelated to the KiB/MiB string relocationSize carries.
func mb(n int64) string { return fmt.Sprintf("%.1fMB", float64(n)/1048576.0) }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// formatRelocationSize reproduces Trilium's own display format for a
// relocated note's relocationSize label: k = max(round(bytes/1024), 1);
// under 1024 it prints "<k> KiB", otherwise "<round(k/102.4)/10> MiB".
func formatRelocationSize(n int64) string {
	k := int64(math.Round(float64(n) / 1024))
	if k < 1 {
		k = 1
	}
	if k < 1024 {
		return fmt.Sprintf("%d KiB", k)
	}
	m := int64(math.Round(float64(k) / 102.4))
	if m%10 == 0 {
		return fmt.Sprintf("%d MiB", m/10)
	}
	return fmt.Sprintf("%s MiB", strconv.FormatFloat(float64(m)/10, 'f', 1, 64))
}
