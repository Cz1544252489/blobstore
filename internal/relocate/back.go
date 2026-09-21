package relocate

// back.go implements "blobstore back": pull one or more relocated notes'
// content out of blobstore and turn the note back into a plain file note.

import (
	"fmt"
	"strings"
)

func cmdBack(args []string) error {
	var noteIDs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("back 不认识参数 %s", a)
		}
		noteIDs = append(noteIDs, a)
	}
	if len(noteIDs) == 0 {
		return fmt.Errorf("用法:blobstore back <noteId>...")
	}

	cfg, err := readConfig()
	if err != nil {
		return err
	}

	var okCount, failCount int
	for _, id := range noteIDs {
		fmt.Println()
		if err := backOne(cfg, id); err != nil {
			failCount++
			fmt.Println("  失败:" + err.Error())
		} else {
			okCount++
		}
	}
	fmt.Printf("\n完成:成功 %d 篇,失败 %d 篇\n", okCount, failCount)
	return nil
}

// backOne follows the module's fixed back-migration steps. Step 3 (the
// sha256 check against relocationHash) is the last point at which the note
// itself is still untouched; a mismatch there aborts with nothing changed.
// From step 4 on the note's type has already changed, so a later failure is
// reported as a mid-state note rather than rolled back.
func backOne(cfg Config, noteID string) error {
	n, err := getNote(noteID)
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s\n", noteID, n.Title)
	if n.Type != "webView" {
		return fmt.Errorf("类型是 %s,不是网页视图笔记", n.Type)
	}
	target := ownAttrValue(n, "relocationTarget")
	if target == "" {
		return fmt.Errorf("没有 relocationTarget 标签,这篇不是经这条路搬走的")
	}
	hash := ownAttrValue(n, "relocationHash")
	if hash == "" {
		return fmt.Errorf("没有 relocationHash 标签")
	}
	svc, err := selectServiceByID(cfg.Services, target)
	if err != nil {
		return err
	}

	data, contentType, err := getBlob(svc, noteID)
	if err != nil {
		return fmt.Errorf("从 blobstore 取回失败,笔记未改动:%w", err)
	}
	sum := sha256Hex(data)
	if sum[:16] != hash {
		return fmt.Errorf("取回内容的 sha256 前 16 位是 %s,笔记的 relocationHash 是 %s,不一致,已中止,笔记未改动", sum[:16], hash)
	}

	// The widget keeps the mime on the webView note; notes moved out with it cleared take the service's.
	mime := n.Mime
	if mime == "" {
		mime = contentType
	}
	if err := setNoteType(noteID, "file", mime); err != nil {
		return fmt.Errorf("笔记 %s 处在中间状态,只做到第 3 步(内容已核对,类型未改):%w", noteID, err)
	}
	if err := putContent(noteID, data, binaryCT); err != nil {
		return fmt.Errorf("笔记 %s 处在中间状态,只做到第 4 步(类型已改,内容未写):%w", noteID, err)
	}
	back, err := getContent(noteID)
	if err != nil {
		return fmt.Errorf("笔记 %s 处在中间状态,内容已写但回读校验失败:%w", noteID, err)
	}
	if sha256Hex(back) != sum {
		return fmt.Errorf("笔记 %s 写入后回读校验不一致,请检查这篇笔记", noteID)
	}

	n, err = getNote(noteID)
	if err != nil {
		return fmt.Errorf("笔记 %s 内容已放回,但重新读取笔记失败,relocation 标签未清理:%w", noteID, err)
	}
	for _, name := range []string{"relocationHash", "relocationTarget", "relocationSize", "webViewSrc"} {
		if err := deleteLabel(n, name); err != nil {
			return fmt.Errorf("笔记 %s 内容已放回,但删除标签 %s 失败:%w", noteID, name, err)
		}
	}
	// The bytes are back in Trilium's own database; see moveOne on why the
	// location label is updated instead of removed.
	if cfg.LocationLabel != "" {
		if err := setLabel(n, cfg.LocationLabel, "true"); err != nil {
			return fmt.Errorf("笔记 %s 内容已放回,但写 #%s 失败:%w", noteID, cfg.LocationLabel, err)
		}
	}
	if ownAttr(n, "ToLocal") != nil {
		if err := setLabel(n, "ToLocal", "true"); err != nil {
			return fmt.Errorf("笔记 %s 内容已放回,但写 #ToLocal 失败:%w", noteID, err)
		}
	}

	if err := deleteBlob(svc, noteID); err != nil {
		fmt.Println("  提醒:blobstore 上删除失败,可能需要手动清理:" + err.Error())
	}
	fmt.Println("  已放回 Trilium 数据库")
	return nil
}
