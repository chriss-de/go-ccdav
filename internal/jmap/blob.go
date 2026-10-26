package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// blobTTL is how long an uploaded blob is kept. Nothing references
// uploads for long yet: they are inputs to /set or import methods. RFC
// 8620 asks for at least an hour.
const blobTTL = 24 * time.Hour

// blobStore keeps uploads as users/<owner>/.jmap-blobs/<sha256>. The blob
// id is "G" + the hex digest, so equal uploads share one file.
type blobStore struct {
	root string
}

func (b blobStore) dir(owner string) string {
	return filepath.Join(b.root, "users", owner, ".jmap-blobs")
}

func blobPath(dir, id string) (string, bool) {
	h, ok := strings.CutPrefix(id, "G")
	if !ok || len(h) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", false
	}
	return filepath.Join(dir, h), true
}

// put stores r and returns the blob id and size. Expired blobs of the
// same owner are removed on the way.
func (b blobStore) put(owner string, r io.Reader) (string, int64, error) {
	dir := b.dir(owner)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", 0, err
	}
	b.expire(dir)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	id := "G" + hex.EncodeToString(sum.Sum(nil))
	p, _ := blobPath(dir, id)
	if err := os.Rename(tmp, p); err != nil {
		return "", 0, err
	}
	now := time.Now()
	os.Chtimes(p, now, now) // a re-upload restarts the clock
	return id, n, nil
}

func (b blobStore) open(owner, id string) (*os.File, error) {
	p, ok := blobPath(b.dir(owner), id)
	if !ok {
		return nil, fs.ErrNotExist
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if time.Since(fi.ModTime()) > blobTTL {
		return nil, fs.ErrNotExist
	}
	return os.Open(p)
}

func (b blobStore) expire(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > blobTTL {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

type uploadResponse struct {
	AccountID string `json:"accountId"`
	BlobID    string `json:"blobId"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
}

// upload is the upload endpoint (RFC 8620 §6.1). Uploading needs write
// access to something in the account.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, user, accountID string) {
	a, ok := h.account(user, accountID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if a.readOnly {
		http.Error(w, "the account is read-only for you", http.StatusForbidden)
		return
	}
	release, ok := slot(&h.uploads, user, maxConcurrentUpload)
	if !ok {
		writeProblem(w, problem{Type: probLimit, Limit: "maxConcurrentUpload", Detail: "too many concurrent uploads"})
		return
	}
	defer release()
	id, size, err := h.blobs.put(a.owner, http.MaxBytesReader(w, r.Body, maxSizeUpload))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeProblem(w, problem{Type: probLimit, Limit: "maxSizeUpload", Detail: "upload too large", Status: http.StatusRequestEntityTooLarge})
			return
		}
		h.log.Error("jmap upload", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	typ := r.Header.Get("Content-Type")
	if typ == "" {
		typ = "application/octet-stream"
	}
	writeJSON(w, http.StatusCreated, uploadResponse{AccountID: accountID, BlobID: id, Type: typ, Size: size})
}

// download is the download endpoint (RFC 8620 §6.2). The client picks the
// content type; the response is an attachment in a CSP sandbox, so even
// text/html can't run in the server's origin.
func (h *Handler) download(w http.ResponseWriter, r *http.Request, user, accountID, blobID, name string) {
	a, ok := h.account(user, accountID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := h.blobs.open(a.owner, blobID)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.log.Error("jmap download", "user", user, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	typ := r.URL.Query().Get("type")
	if mt, params, err := mime.ParseMediaType(typ); err == nil {
		typ = mime.FormatMediaType(mt, params)
	} else {
		typ = "application/octet-stream"
	}
	w.Header().Set("Content-Type", typ)
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if disp == "" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", disp)
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, immutable, max-age=31536000")
	http.ServeContent(w, r, "", time.Time{}, f)
}
