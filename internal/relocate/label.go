package relocate

// label.go implements "blobstore label": write the location label on every PDF note
// according to where its bytes actually are — true when they sit in
// Trilium's own database, false when they live on a blobstore service.
// The "论文列表" table binds a column to this label, so it has to stay
// accurate after every move and back.

import (
	"fmt"
)

func cmdLabel(args []string) error {
	dryRun := false
	for _, a := range args {
		switch a {
		case "-dry-run", "--dry-run":
			dryRun = true
		default:
			return fmt.Errorf("label 不认识参数 %s", a)
		}
	}

	cfg, err := readConfig()
	if err != nil {
		return err
	}
	if cfg.LocationLabel == "" {
		return fmt.Errorf("配置笔记里没有 locationLabel,不知道该写哪个标签")
	}
	label := cfg.LocationLabel

	// A search whose results include protected notes comes back 401 while the
	// protected session is closed, so they are excluded here; their bytes
	// cannot be read through ETAPI anyway and they never leave the database.
	local, err := searchNotes("note.type = file AND note.mime = application/pdf AND note.isProtected = false")
	if err != nil {
		return err
	}
	relocated, err := searchNotes("note.type = webView #relocationTarget")
	if err != nil {
		return err
	}

	type target struct {
		note *Note
		want string
	}
	var todo []target
	for i := range local {
		if ownAttrValue(&local[i], label) != "true" {
			todo = append(todo, target{&local[i], "true"})
		}
	}
	for i := range relocated {
		if ownAttrValue(&relocated[i], label) != "false" {
			todo = append(todo, target{&relocated[i], "false"})
		}
	}

	fmt.Printf("在 Trilium 数据库:%d 篇;已迁移到 blobstore:%d 篇;要改 %d 篇\n",
		len(local), len(relocated), len(todo))
	if dryRun {
		for _, t := range todo {
			fmt.Printf("  %s  #%s=%s  %s\n", t.note.NoteID, label, t.want, t.note.Title)
		}
		fmt.Println("(-dry-run,未写入)")
		return nil
	}

	var okCount, failCount int
	for _, t := range todo {
		if err := setLabel(t.note, label, t.want); err != nil {
			failCount++
			fmt.Printf("  %s 写 #%s 失败:%v\n", t.note.NoteID, label, err)
			continue
		}
		okCount++
	}
	fmt.Printf("已改 %d 篇,失败 %d 篇\n", okCount, failCount)
	return nil
}
