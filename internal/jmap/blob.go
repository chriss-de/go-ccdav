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

// maxPendingUploads bounds the bytes of a user's unexpired uploads.
var maxPendingUploads int64 = 200 << 20

// blobStore keeps uploads as users/<uploader>/.jmap-blobs/<sha256>. The
// blob id is "G" + the hex digest, so equal uploads share one file. Blobs
// belong to whoever uploaded them, whatever account they named: others
// with access to that account (a read-only sharee) can't fetch them by
// id, which a hash of the content would make guessable.
type blobStore struct {
	root string
}

func (b blobStore) dir(user string) string {
	return filepath.Join(b.root, "users", user, ".jmap-blobs")
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

// errPendingFull: the upload would take the user's pending uploads past
// maxPendingUploads.
var errPendingFull = errors.New("jmap: too many pending uploads")

// put stores r for user and returns the blob id and size. Expired blobs
// of the same user are removed on the way.
func (b blobStore) put(user string, r io.Reader) (string, int64, error) {
	dir := b.dir(user)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", 0, err
	}
	room := maxPendingUploads - b.expire(dir)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(r, max(room, 0)+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if n > room {
		return "", 0, errPendingFull
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

// open opens a blob user uploaded.
func (b blobStore) open(user, id string) (*os.File, error) {
	p, ok := blobPath(b.dir(user), id)
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

// expire removes expired blobs and returns the bytes of the others.
func (b blobStore) expire(dir string) (kept int64) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range ents {
		fi, err := e.Info()
		switch {
		case err != nil:
		case time.Since(fi.ModTime()) > blobTTL:
			os.Remove(filepath.Join(dir, e.Name()))
		default:
			kept += fi.Size()
		}
	}
	return kept
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
	id, size, err := h.blobs.put(user, http.MaxBytesReader(w, r.Body, maxSizeUpload))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeProblem(w, problem{Type: probLimit, Limit: "maxSizeUpload", Detail: "upload too large", Status: http.StatusRequestEntityTooLarge})
			return
		}
		if errors.Is(err, errPendingFull) {
			writeProblem(w, problem{Type: probLimit, Limit: "maxSizeUpload", Status: http.StatusRequestEntityTooLarge,
				Detail: "too many uploads in the last day; try again later"})
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
	if _, ok := h.account(user, accountID); !ok {
		http.NotFound(w, r)
		return
	}
	f, err := h.blobs.open(user, blobID)
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
