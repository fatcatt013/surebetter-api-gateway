package main

import (
	"errors"
	"io"
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

var shotIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// shotPath is where userID's screenshot id is stored: <shotsDir>/<user id>/<id>.jpg.
func (b *botHub) shotPath(userID int64, id string) string {
	return filepath.Join(b.shotsDir, strconv.FormatInt(userID, 10), id+".jpg")
}

// writeShot writes data to path through a temporary file, so a reader never sees half a JPEG.
func writeShot(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// handlePostShot stores a JPEG (raw body, ≤ maxShotBytes, starting FF D8 FF) for the agent's user
// and answers its id. ?window= and ?order= are only logged.
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
	id := newOrderID() // 16 random hex characters
	if err := writeShot(b.shotPath(p.UserID, id), data); err != nil {
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
