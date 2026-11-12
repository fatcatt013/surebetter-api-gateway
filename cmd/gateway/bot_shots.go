package main

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/surebetter/api-gateway/internal/accounts"
)

// maxShotBytes is the largest screenshot an agent may upload.
const maxShotBytes = 1_500_000

// shotsPerMinute is how many screenshots a user's agents may upload per minute, all codes together.
const shotsPerMinute = 30

// A user's screenshot directory holds at most maxShotFiles files and maxShotDirBytes bytes (the data
// volume also holds accounts.db). Vars so a test can lower them.
var (
	maxShotFiles          = 500
	maxShotDirBytes int64 = 300_000_000
)

var errShotsFull = errors.New("shots_full")

var shotIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// shotPath is where userID's screenshot id is stored: <shotsDir>/<user id>/<id>.jpg.
func (b *botHub) shotPath(userID int64, id string) string {
	return filepath.Join(b.shotsDir, strconv.FormatInt(userID, 10), id+".jpg")
}

// writeShot writes data to path through a temporary file, so a reader never sees half a JPEG. On
// failure the temporary file is removed (one a crash leaves behind is removed by prune).
func writeShot(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	err := os.WriteFile(tmp, data, 0o600)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// dirUsage counts the regular files directly in dir and their bytes. A missing dir is empty.
func dirUsage(dir string) (files int, bytes int64, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		files++
		bytes += info.Size()
	}
	return files, bytes, nil
}

// storeShot writes userID's screenshot id unless that would put the user's directory over
// maxShotFiles files or maxShotDirBytes bytes (errShotsFull). At most 500 files: a scan is cheap.
func (b *botHub) storeShot(userID int64, id string, data []byte) error {
	path := b.shotPath(userID, id)
	b.shotsMu.Lock()
	defer b.shotsMu.Unlock()
	files, bytes, err := dirUsage(filepath.Dir(path))
	if err != nil {
		return err
	}
	if files+1 > maxShotFiles || bytes+int64(len(data)) > maxShotDirBytes {
		return errShotsFull
	}
	return writeShot(path, data)
}

// handlePostShot stores a JPEG (raw body, ≤ maxShotBytes, starting FF D8 FF) for the agent's user
// and answers its id: at most shotsPerMinute per user (a failed write is not counted; a full
// directory is), within the directory caps. ?window= and ?order= are only logged.
func (b *botHub) handlePostShot(w http.ResponseWriter, r *http.Request) {
	if b.shotsDir == "" {
		log.Error().Msg("bot screenshot upload without a screenshots directory")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxShotBytes))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		accounts.WriteError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	case err != nil || len(data) < 3 || data[0] != 0xFF || data[1] != 0xD8 || data[2] != 0xFF:
		accounts.WriteError(w, http.StatusBadRequest, "bad_image")
		return
	}
	p := principalOf(r)
	window, ok := b.shots.allow(p.UserID, 1, b.now())
	if !ok {
		accounts.WriteError(w, http.StatusTooManyRequests, "too_many_shots")
		return
	}
	id := newOrderID() // 16 random hex characters
	switch err := b.storeShot(p.UserID, id, data); {
	case errors.Is(err, errShotsFull):
		accounts.WriteError(w, http.StatusTooManyRequests, "shots_full")
		return
	case err != nil:
		b.shots.refund(p.UserID, 1, window) // not stored: not counted
		log.Error().Err(err).Msg("bot screenshot write failed")
		accounts.WriteError(w, http.StatusInternalServerError, "internal")
		return
	}
	log.Debug().Int64("user", p.UserID).Str("shot", id).Str("window", r.URL.Query().Get("window")).
		Str("order", r.URL.Query().Get("order")).Int("bytes", len(data)).Msg("bot screenshot stored")
	accounts.WriteJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// handleGetShot serves one of the session user's screenshots; anything else is 404.
func (b *botHub) handleGetShot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if b.shotsDir == "" || !shotIDPattern.MatchString(id) {
		accounts.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	f, err := os.Open(b.shotPath(principalOf(r).UserID, id))
	if err != nil {
		accounts.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		accounts.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeContent(w, r, "", st.ModTime(), f)
}
