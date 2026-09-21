package server

// store.go implements blobstore's storage primitives: a flat directory of
// current files, a sibling directory of superseded versions, and a JSON
// index that ties the two together. It holds no HTTP or Trilium knowledge —
// the server in main.go and handlers.go is the only caller.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// blobRecord is the current version of one stored id, as index.json keeps it.
type blobRecord struct {
	Mime     string    `json:"mime"`
	Filename string    `json:"filename,omitempty"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	StoredAt time.Time `json:"storedAt"`
	// Key guards /files/<id>, which carries no Authorization header. The id is
	// the caller's own note id and easy to guess; this secret is not.
	Key string `json:"key"`
}

// historyEntry is one version an id held before it was overwritten or
// deleted out of "current". It survives both events, so an id's full history
// is only ever appended to, never rewritten.
type historyEntry struct {
	ArchivedAs string    `json:"archivedAs"`
	ArchivedAt time.Time `json:"archivedAt"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
}

// index is the on-disk index.json layout: current records keyed by id, plus
// each id's list of superseded versions. A deleted id disappears from Blobs
// but its History entries remain.
type index struct {
	Blobs   map[string]blobRecord     `json:"blobs"`
	History map[string][]historyEntry `json:"history"`
}

// Store owns one root directory's blobs/, blobs-old/ and index.json.
type Store struct {
	root string
	// mu serializes StoreBlob and DeleteBlob, which both read-modify-write
	// index.json; ReadBlob needs no lock since renames are atomic and it
	// only ever reads a state index.json already committed to.
	mu sync.Mutex
}

// openStore creates the two storage subdirectories if missing and returns a
// Store ready to use. It does not touch index.json — that is read lazily so
// a fresh root need not pre-create an empty one.
func openStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "blobs"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "blobs-old"), 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.root, "index.json") }
func (s *Store) blobsDir() string  { return filepath.Join(s.root, "blobs") }
func (s *Store) oldDir() string    { return filepath.Join(s.root, "blobs-old") }

// loadIndex reads index.json, returning an empty index when the file does
// not exist yet rather than an error.
func (s *Store) loadIndex() (*index, error) {
	data, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return &index{Blobs: map[string]blobRecord{}, History: map[string][]historyEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("index.json 不是合法 JSON:%w", err)
	}
	if idx.Blobs == nil {
		idx.Blobs = map[string]blobRecord{}
	}
	if idx.History == nil {
		idx.History = map[string][]historyEntry{}
	}
	return &idx, nil
}

// saveIndex writes index.json by way of a temp file plus rename, so a crash
// or a killed process mid-write never leaves a half-written index behind.
func (s *Store) saveIndex(idx *index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath())
}

// StoreResult is what a successful StoreBlob reports back to the caller.
type StoreResult struct {
	ID  string
	Rec blobRecord
	// New is false when the submitted bytes exactly match what this id
	// already held, so no new version was created.
	New bool
}

// StoreBlob saves data under id. A byte-identical resubmission is a no-op
// beyond confirming the id still exists. Anything else archives whatever the
// id currently holds into blobs-old/ (recorded in index.json's history)
// before the new bytes become current. The new bytes are written to a temp
// file and re-hashed before anything is archived or replaced, so a failed or
// corrupted write never disturbs the existing current file.
func (s *Store) StoreBlob(id, mime, filename string, data []byte) (StoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sum := sha256Hex(data)
	idx, err := s.loadIndex()
	if err != nil {
		return StoreResult{}, err
	}

	if existing, ok := idx.Blobs[id]; ok && existing.SHA256 == sum {
		return StoreResult{ID: id, Rec: existing, New: false}, nil
	}

	finalPath := filepath.Join(s.blobsDir(), id)
	tmpPath := finalPath + ".tmp-" + randomSuffix()
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return StoreResult{}, fmt.Errorf("写入失败:%w", err)
	}
	writtenSum, err := sha256File(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return StoreResult{}, fmt.Errorf("校验写入内容失败:%w", err)
	}
	if writtenSum != sum {
		_ = os.Remove(tmpPath)
		return StoreResult{}, errors.New("写入后 sha256 与预期不一致,已放弃,未留下半份文件")
	}

	if existing, ok := idx.Blobs[id]; ok {
		archiveName := id + "-" + time.Now().UTC().Format("20060102T150405.000000000")
		if err := os.Rename(finalPath, filepath.Join(s.oldDir(), archiveName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(tmpPath)
			return StoreResult{}, fmt.Errorf("归档旧版本失败:%w", err)
		}
		idx.History[id] = append(idx.History[id], historyEntry{
			ArchivedAs: archiveName,
			ArchivedAt: time.Now().UTC(),
			Size:       existing.Size,
			SHA256:     existing.SHA256,
		})
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return StoreResult{}, fmt.Errorf("改名为当前版本失败:%w", err)
	}

	rec := blobRecord{
		Mime:     mime,
		Filename: filename,
		Size:     int64(len(data)),
		SHA256:   sum,
		StoredAt: time.Now().UTC(),
		// A fresh key per version: superseding the content invalidates the URL
		// that pointed at the old one.
		Key: randomKey(),
	}
	idx.Blobs[id] = rec
	if err := s.saveIndex(idx); err != nil {
		return StoreResult{}, fmt.Errorf("写索引失败:%w", err)
	}
	return StoreResult{ID: id, Rec: rec, New: true}, nil
}

// ReadBlob returns one stored file's bytes and current-record metadata
// (chiefly its mime, for the Content-Type header). It reports os.ErrNotExist
// when the id has no current record.
func (s *Store) ReadBlob(id string) ([]byte, blobRecord, error) {
	idx, err := s.loadIndex()
	if err != nil {
		return nil, blobRecord{}, err
	}
	rec, ok := idx.Blobs[id]
	if !ok {
		return nil, blobRecord{}, os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(s.blobsDir(), id))
	if err != nil {
		return nil, blobRecord{}, err
	}
	return data, rec, nil
}

// listedBlob is one current record as ListBlobs reports it, with the number of
// versions its history holds.
type listedBlob struct {
	ID       string
	Rec      blobRecord
	Versions int
}

// ListBlobs returns every current record, newest first. Like ReadBlob it reads
// the index once and takes no lock.
func (s *Store) ListBlobs() ([]listedBlob, error) {
	idx, err := s.loadIndex()
	if err != nil {
		return nil, err
	}
	list := make([]listedBlob, 0, len(idx.Blobs))
	for id, rec := range idx.Blobs {
		list = append(list, listedBlob{ID: id, Rec: rec, Versions: len(idx.History[id])})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Rec.StoredAt.After(list[j].Rec.StoredAt) })
	return list, nil
}

// DeleteBlob drops id's current record and file. Its history entries under
// index.json's "history" key, and its archived copies under blobs-old/, are
// left untouched — retrieving a note should not erase the trail of versions
// it went through. It reports false when the id had no current record (a 404
// to the caller).
func (s *Store) DeleteBlob(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, err := s.loadIndex()
	if err != nil {
		return false, err
	}
	if _, ok := idx.Blobs[id]; !ok {
		return false, nil
	}
	delete(idx.Blobs, id)
	// The index is the source of truth for existence, so it is committed
	// before the file is removed: a crash between the two steps leaves an
	// orphaned file rather than a dangling index entry that GET would 500 on.
	if err := s.saveIndex(idx); err != nil {
		return false, err
	}
	if err := os.Remove(filepath.Join(s.blobsDir(), id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// randomSuffix returns 16 random hex characters, enough that two concurrent
// writes never pick the same temp file name.
func randomSuffix() string { return randomHex(8) }

// randomKey returns 32 random hex characters. It guards /files/<id>, which is
// reachable by every device on the network the service listens on, so it is
// longer than a name only this process ever compares.
func randomKey() string { return randomHex(16) }

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
