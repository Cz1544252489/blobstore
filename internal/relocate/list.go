package relocate

// list.go implements "blobstore list": a read-only report over the notes/
// blobs join in document.db, filtered and capped in Go once the rows are
// back, since SQLite's own filtering would need the same arithmetic anyway.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func parseListArgs(args []string) (minMB, maxMB float64, n int, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-min":
			i++
			if i >= len(args) {
				return 0, 0, 0, fmt.Errorf("-min 缺少值")
			}
			if minMB, err = strconv.ParseFloat(args[i], 64); err != nil {
				return 0, 0, 0, fmt.Errorf("-min 的值不是数字:%s", args[i])
			}
		case "-max":
			i++
			if i >= len(args) {
				return 0, 0, 0, fmt.Errorf("-max 缺少值")
			}
			if maxMB, err = strconv.ParseFloat(args[i], 64); err != nil {
				return 0, 0, 0, fmt.Errorf("-max 的值不是数字:%s", args[i])
			}
		case "-n":
			i++
			if i >= len(args) {
				return 0, 0, 0, fmt.Errorf("-n 缺少值")
			}
			if n, err = strconv.Atoi(args[i]); err != nil {
				return 0, 0, 0, fmt.Errorf("-n 的值不是整数:%s", args[i])
			}
		default:
			return 0, 0, 0, fmt.Errorf("list 不认识参数 %s", a)
		}
	}
	return minMB, maxMB, n, nil
}

func cmdList(args []string) error {
	minMB, maxMB, n, err := parseListArgs(args)
	if err != nil {
		return err
	}
	dbPath := envOr("RELOCATE_DB", defaultDBPath)
	pythonBin := envOr("RELOCATE_PYTHON", "python")
	notes, err := pdfNoteList(dbPath, pythonBin)
	if err != nil {
		return err
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].Size > notes[j].Size })

	var filtered []PDFNote
	for _, nt := range notes {
		if minMB > 0 && float64(nt.Size) < minMB*1048576 {
			continue
		}
		if maxMB > 0 && float64(nt.Size) > maxMB*1048576 {
			continue
		}
		filtered = append(filtered, nt)
	}
	if n > 0 && len(filtered) > n {
		filtered = filtered[:n]
	}

	fmt.Printf("%-13s %10s  %s\n", "noteId", "大小", "标题")
	fmt.Println(strings.Repeat("-", 90))
	var totalSize int64
	for _, nt := range filtered {
		mark := ""
		if nt.Protected {
			mark = "  [受保护,ETAPI 读不到内容]"
		}
		fmt.Printf("%-13s %10s  %s%s\n", nt.NoteID, mb(nt.Size), nt.Title, mark)
		totalSize += nt.Size
	}
	fmt.Printf("\n合计 %d 篇,共 %s\n", len(filtered), mb(totalSize))
	return nil
}
