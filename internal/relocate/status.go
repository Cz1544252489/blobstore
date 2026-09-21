package relocate

// status.go implements "blobstore status": list notes already relocated to
// some blobstore-like service, with an optional per-note byte check.

import (
	"fmt"
	"strings"
)

func cmdStatus(args []string) error {
	verify := false
	for _, a := range args {
		switch a {
		case "-verify", "--verify":
			verify = true
		default:
			return fmt.Errorf("status 不认识参数 %s", a)
		}
	}

	notes, err := searchNotes("note.type = webView #relocationTarget")
	if err != nil {
		return err
	}

	var services []Service
	if verify {
		cfg, err := readConfig()
		if err != nil {
			return err
		}
		services = cfg.Services
	}

	fmt.Printf("%-13s %10s  %s\n", "noteId", "大小", "标题")
	fmt.Println(strings.Repeat("-", 90))
	for i := range notes {
		n := &notes[i]
		size := ownAttrValue(n, "relocationSize")
		fmt.Printf("%-13s %10s  %s", n.NoteID, size, n.Title)
		if verify {
			ok, msg := verifyOne(services, n)
			if ok {
				fmt.Print("  一致")
			} else {
				fmt.Print("  不一致:" + msg)
			}
		}
		fmt.Println()
	}
	fmt.Printf("\n共 %d 篇已迁移\n", len(notes))
	return nil
}

func verifyOne(services []Service, n *Note) (bool, string) {
	target := ownAttrValue(n, "relocationTarget")
	hash := ownAttrValue(n, "relocationHash")
	if target == "" || hash == "" {
		return false, "缺少 relocationTarget 或 relocationHash 标签"
	}
	svc, err := selectServiceByID(services, target)
	if err != nil {
		return false, err.Error()
	}
	data, _, err := getBlob(svc, n.NoteID)
	if err != nil {
		return false, "取回失败:" + err.Error()
	}
	sum := sha256Hex(data)
	if sum[:16] != hash {
		return false, fmt.Sprintf("blobstore 内容 sha256 前 16 位是 %s,笔记标签是 %s", sum[:16], hash)
	}
	return true, ""
}
