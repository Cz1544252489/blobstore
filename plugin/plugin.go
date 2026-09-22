// Package plugin holds the Trilium widget, overview page and backend script
// that talk to a blobstore service, and writes them out as a Trilium zip
// export. It imports as two trees: the widget with the backend module beneath
// it, and a render note whose page script has a clone of that same module
// beneath it — a backend call resolves modules among the calling script's
// children, so both scripts need the module there.
package plugin

import (
	"archive/zip"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

//go:embed content-relocation.jsx
var widgetSource []byte

//go:embed relocationBackend.js
var backendSource []byte

//go:embed relocation-overview.jsx
var overviewSource []byte

// cloneStub is what a clone entry's data file holds; import only adds a branch for it.
var cloneStub = []byte("<p>This is a clone of relocationBackend.</p>")

// appVersion is the Trilium release the scripts were written against; import only shows it.
const appVersion = "0.105.0"

type attribute struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	Value         string `json:"value"`
	IsInheritable bool   `json:"isInheritable"`
	Position      int    `json:"position"`
}

type noteMeta struct {
	IsClone      bool        `json:"isClone"`
	NoteID       string      `json:"noteId"`
	NotePath     []string    `json:"notePath"`
	Title        string      `json:"title"`
	NotePosition int         `json:"notePosition"`
	Prefix       *string     `json:"prefix"`
	IsExpanded   bool        `json:"isExpanded"`
	Type         string      `json:"type"`
	Mime         string      `json:"mime"`
	Attributes   []attribute `json:"attributes"`
	Attachments  []struct{}  `json:"attachments"`
	Format       string      `json:"format,omitempty"`
	DataFileName string      `json:"dataFileName"`
	DirFileName  string      `json:"dirFileName,omitempty"`
	Children     []noteMeta  `json:"children,omitempty"`
}

// Write puts blobstore-plugin-<version>.zip into dir and returns its path.
func Write(dir, version string) (string, error) {
	// Import remaps these ids, so they only have to be unique within the archive. The backend
	// note's title is the name the widget calls it by and must stay relocationBackend.
	backend := noteMeta{
		NoteID: "crlBackend01", NotePath: []string{"crlWidget001", "crlBackend01"}, Title: "relocationBackend",
		NotePosition: 10, Type: "code", Mime: "application/javascript;env=backend",
		Attributes: []attribute{}, Attachments: []struct{}{}, DataFileName: "relocationBackend.js",
	}
	widget := noteMeta{
		NoteID: "crlWidget001", NotePath: []string{"crlWidget001"}, Title: "内容迁移",
		NotePosition: 10, Type: "code", Mime: "text/jsx",
		Attributes:  []attribute{{Type: "label", Name: "widget", Position: 10}},
		Attachments: []struct{}{}, DataFileName: "content-relocation.jsx", DirFileName: "content-relocation",
		Children: []noteMeta{backend},
	}
	// The render note shows the page script named by its renderNote relation; the script's own
	// child is a clone of the backend note, so there is still a single copy of that code.
	backendClone := noteMeta{
		IsClone: true, NoteID: "crlBackend01", NotePath: []string{"crlOverview1", "crlOvPage001", "crlBackend01"},
		Title: "relocationBackend", NotePosition: 10, Type: "text", Mime: "text/html", Format: "html",
		Attributes: []attribute{}, Attachments: []struct{}{}, DataFileName: "relocationBackend.clone.html",
	}
	page := noteMeta{
		NoteID: "crlOvPage001", NotePath: []string{"crlOverview1", "crlOvPage001"}, Title: "内容迁移总览页面",
		NotePosition: 10, Type: "code", Mime: "text/jsx",
		Attributes: []attribute{}, Attachments: []struct{}{},
		DataFileName: "relocation-overview.jsx", DirFileName: "relocation-overview", Children: []noteMeta{backendClone},
	}
	overview := noteMeta{
		NoteID: "crlOverview1", NotePath: []string{"crlOverview1"}, Title: "内容迁移总览",
		NotePosition: 20, Type: "render", Mime: "",
		Attributes: []attribute{
			{Type: "relation", Name: "renderNote", Value: "crlOvPage001", Position: 10},
			{Type: "label", Name: "iconClass", Value: "bx bx-bar-chart-alt-2", Position: 20},
			// The widget finds the page by this label to link to it.
			{Type: "label", Name: "relocationOverview", Position: 30},
		},
		Attachments: []struct{}{}, DataFileName: "relocation-overview-note.html", DirFileName: "relocation-overview-note", Children: []noteMeta{page},
	}
	meta, err := json.MarshalIndent(map[string]any{
		"formatVersion": 2, "appVersion": appVersion, "files": []noteMeta{widget, overview},
	}, "", "  ")
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, "blobstore-plugin-"+version+".zip")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(f)
	now := time.Now()
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{"!!!meta.json", meta},
		{"content-relocation.jsx", widgetSource},
		{"content-relocation/relocationBackend.js", backendSource},
		{"relocation-overview-note.html", []byte{}},
		{"relocation-overview-note/relocation-overview.jsx", overviewSource},
		{"relocation-overview-note/relocation-overview/relocationBackend.clone.html", cloneStub},
	} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: entry.name, Method: zip.Deflate, Modified: now})
		if err == nil {
			_, err = w.Write(entry.data)
		}
		if err != nil {
			_ = f.Close()
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}
