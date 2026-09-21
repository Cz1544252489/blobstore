package relocate

// move.go implements "blobstore move": upload selected PDF notes' bytes to
// blobstore and turn each note into a webView pointing at the returned URL.
// moveOne follows a fixed, one-way step order — once the upload is verified
// and the note's type is changed, a later failure is reported as a note
// left in a mid-migration state rather than rolled back; see the module's
// CLAUDE.md for why an automatic rollback past that point is not attempted.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func parseMoveArgs(args []string) (minMB float64, n int, service string, dryRun bool, noteIDs []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-dry-run", "--dry-run":
			dryRun = true
		case "-min":
			i++
			if i >= len(args) {
				return 0, 0, "", false, nil, fmt.Errorf("-min 缺少值")
			}
			if minMB, err = strconv.ParseFloat(args[i], 64); err != nil {
				return 0, 0, "", false, nil, fmt.Errorf("-min 的值不是数字:%s", args[i])
			}
		case "-n":
			i++
			if i >= len(args) {
				return 0, 0, "", false, nil, fmt.Errorf("-n 缺少值")
			}
			if n, err = strconv.Atoi(args[i]); err != nil {
				return 0, 0, "", false, nil, fmt.Errorf("-n 的值不是整数:%s", args[i])
			}
		case "-service":
			i++
			if i >= len(args) {
				return 0, 0, "", false, nil, fmt.Errorf("-service 缺少值")
			}
			service = args[i]
		default:
			if strings.HasPrefix(a, "-") {
				return 0, 0, "", false, nil, fmt.Errorf("move 不认识参数 %s", a)
			}
			noteIDs = append(noteIDs, a)
		}
	}
	return minMB, n, service, dryRun, noteIDs, nil
}

func cmdMove(args []string) error {
	minMB, n, serviceName, dryRun, noteIDs, err := parseMoveArgs(args)
	if err != nil {
		return err
	}
	dbPath := envOr("RELOCATE_DB", defaultDBPath)
	pythonBin := envOr("RELOCATE_PYTHON", "python")

	cfg, err := readConfig()
	if err != nil {
		return err
	}
	svc, err := selectService(cfg, serviceName)
	if err != nil {
		return err
	}

	targets := noteIDs
	if len(targets) == 0 {
		notes, err := pdfNoteList(dbPath, pythonBin)
		if err != nil {
			return err
		}
		sort.Slice(notes, func(i, j int) bool { return notes[i].Size > notes[j].Size })
		var protected int
		for _, nt := range notes {
			if minMB > 0 && float64(nt.Size) < minMB*1048576 {
				continue
			}
			// Protected notes cannot be read through ETAPI, so they never make
			// it past moveOne's check; leaving them out here keeps them from
			// taking up slots in every batch.
			if nt.Protected {
				protected++
				continue
			}
			targets = append(targets, nt.NoteID)
			if n > 0 && len(targets) >= n {
				break
			}
		}
		if protected > 0 {
			fmt.Printf("排除 %d 篇受保护的笔记(ETAPI 读不到内容)\n", protected)
		}
	}
	if len(targets) == 0 {
		fmt.Println("没有符合条件的笔记")
		return nil
	}

	fmt.Printf("共 %d 篇待处理,使用服务 %s(%s)\n", len(targets), svc.Name, svc.URL)
	for _, id := range targets {
		fmt.Println("  " + id)
	}
	if dryRun {
		fmt.Println("(-dry-run,未发送任何请求)")
		return nil
	}

	var okCount, skipCount, failCount int
	for _, id := range targets {
		fmt.Println()
		status, merr := moveOne(svc, cfg.LocationLabel, id)
		switch status {
		case moveSkipped:
			skipCount++
			fmt.Println("  跳过:" + merr.Error())
		case moveFailed:
			failCount++
			fmt.Println("  失败:" + merr.Error())
		default:
			okCount++
			fmt.Println("  已搬到 blobstore")
		}
	}
	fmt.Printf("\n完成:成功 %d 篇,跳过 %d 篇,失败 %d 篇\n", okCount, skipCount, failCount)
	return nil
}

type moveStatus int

const (
	moveOK moveStatus = iota
	moveSkipped
	moveFailed
)

// moveOne carries one note through the eight recorded steps. Steps 1-4 (read
// the note, read its content, upload, verify the upload) have not touched
// the note yet, so a failure there is a clean skip or fail with nothing to
// undo beyond deleting the blob just uploaded. From step 5 on the note's
// type has already changed, so a failure is reported as a mid-state note
// rather than rolled back.
func moveOne(svc Service, locationLabel, noteID string) (moveStatus, error) {
	n, err := getNote(noteID)
	if err != nil {
		return moveFailed, err
	}
	fmt.Printf("%s  %s", noteID, n.Title)

	if n.Type != "file" || n.Mime != "application/pdf" {
		fmt.Println()
		return moveSkipped, fmt.Errorf("类型是 %s/%s,不是 PDF 文件笔记", n.Type, n.Mime)
	}
	if n.IsProtected {
		fmt.Println()
		return moveSkipped, fmt.Errorf("受保护的笔记,ETAPI 读不到内容")
	}

	data, err := getContent(noteID)
	if err != nil {
		fmt.Println()
		return moveFailed, err
	}
	sum := sha256Hex(data)
	fmt.Printf("  %s\n", mb(int64(len(data))))

	filename := ownAttrValue(n, "originalFileName")
	if filename == "" {
		filename = n.Title
	}

	id, respSum, blobURL, err := postBlob(svc, noteID, filename, data, "application/pdf")
	if err != nil {
		return moveFailed, fmt.Errorf("上传失败,笔记未改动:%w", err)
	}
	if id != noteID || respSum != sum {
		if derr := deleteBlob(svc, id); derr != nil {
			return moveFailed, fmt.Errorf("上传后核对不一致(id=%s sha256=%s),撤回失败(%v),笔记未改动", id, respSum, derr)
		}
		return moveFailed, fmt.Errorf("上传后核对不一致(id=%s sha256=%s),已撤回,笔记未改动", id, respSum)
	}

	// The mime stays on the note, as the widget leaves it: Trilium ignores it on a webView note.
	if err := setNoteType(noteID, "webView", n.Mime); err != nil {
		return moveFailed, fmt.Errorf("笔记 %s 处在中间状态,只做到第 4 步(已上传,类型未改):%w", noteID, err)
	}
	if err := putContent(noteID, []byte(" "), "text/plain"); err != nil {
		return moveFailed, fmt.Errorf("笔记 %s 处在中间状态,只做到第 5 步(类型已改,内容未写):%w", noteID, err)
	}
	n, err = getNote(noteID)
	if err != nil {
		return moveFailed, fmt.Errorf("笔记 %s 处在中间状态,只做到第 6 步(内容已写,标签未写),重新读取笔记失败:%w", noteID, err)
	}
	labelOrder := []string{"relocationHash", "relocationTarget", "relocationSize", "webViewSrc"}
	labelValue := map[string]string{
		"relocationHash":   sum[:16],
		"relocationTarget": svc.ID,
		"relocationSize":   formatRelocationSize(int64(len(data))),
		"webViewSrc":       blobURL,
	}
	for i, name := range labelOrder {
		if err := setLabel(n, name, labelValue[name]); err != nil {
			return moveFailed, fmt.Errorf("笔记 %s 处在中间状态,第 7 步写标签时在第 %d/%d 个(%s)失败:%w",
				noteID, i+1, len(labelOrder), name, err)
		}
	}
	// The location label says where the bytes are now: true in Trilium's own
	// database, false anywhere else. A table column can be bound to it, so the
	// label is kept and updated rather than removed. #ToLocal (where the note
	// should be) is only touched when the note already carries it.
	if locationLabel != "" {
		if err := setLabel(n, locationLabel, "false"); err != nil {
			return moveFailed, fmt.Errorf("笔记 %s 已迁移,但第 8 步写 #%s 失败:%w", noteID, locationLabel, err)
		}
	}
	if ownAttr(n, "ToLocal") != nil {
		if err := setLabel(n, "ToLocal", "false"); err != nil {
			return moveFailed, fmt.Errorf("笔记 %s 已迁移,但第 8 步写 #ToLocal 失败:%w", noteID, err)
		}
	}
	return moveOK, nil
}
