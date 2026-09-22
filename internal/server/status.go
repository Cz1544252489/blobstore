package server

// status.go answers GET /status: what the caller's group holds, how much room
// is left on the disk under the storage root, and how the last backup went.
// The backup runs outside this service (deploy/blobstore-backup, as root) and
// leaves its result in <root>/backup-status.json; this side only reads it.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// backupStatusFile is written by deploy/blobstore-backup at the top of the
// storage root, which the backup covers as a whole, tenants included.
const backupStatusFile = "backup-status.json"

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	t := s.checkAuth(w, r, http.MethodGet)
	if t == nil {
		return
	}
	st, err := t.store.Stats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	group := map[string]any{
		"name":      t.name,
		"count":     st.Count,
		"totalSize": st.TotalSize,
		"versions":  st.History,
		"oldFiles":  st.OldFiles,
		"oldSize":   st.OldSize,
	}
	if !st.LastStored.IsZero() {
		group["lastStoredAt"] = st.LastStored
	}

	out := map[string]any{
		"version":   s.version,
		"startedAt": s.startedAt,
		"now":       time.Now().UTC(),
		"group":     group,
		"backup":    nil,
	}
	if total, free, ok := diskUsage(s.root); ok {
		out["disk"] = map[string]any{"total": total, "free": free}
	}
	backup, err := readBackupStatus(s.root)
	switch {
	case err == nil:
		out["backup"] = backup
	case !errors.Is(err, os.ErrNotExist):
		// A status file that cannot be read is itself worth showing, not a reason to fail.
		out["backup"] = map[string]any{"error": err.Error()}
	}
	writeJSON(w, http.StatusOK, out)
}

// readBackupStatus returns the backup script's status file as it wrote it, so
// fields it adds later pass through without a change here.
func readBackupStatus(root string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(root, backupStatusFile))
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, errors.New(backupStatusFile + " 不是合法 JSON:" + err.Error())
	}
	return v, nil
}
