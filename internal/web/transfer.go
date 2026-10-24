package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/carddav"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/store"
)

const maxImport = 50 << 20

var unsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

// export sends a collection as one .ics or .vcf file. Anyone with read
// access may export.
func (u *UI) export(w http.ResponseWriter, r *http.Request) {
	user, _ := u.session(r)
	if user == "" {
		http.Redirect(w, r, u.prefix, http.StatusSeeOther)
		return
	}
	col, err := u.byKey(r.URL.Query().Get("key"))
	if err != nil || col.AccessOf(user, u.cfg.Get().GroupsOf(user)) == "" {
		http.NotFound(w, r)
		return
	}
	name := col.Meta().DisplayName
	if name == "" {
		name = col.Name
	}
	var objs [][]byte
	for _, o := range col.List() {
		if b, _, err := col.Get(o.Name); err == nil {
			objs = append(objs, b)
		}
	}
	var body []byte
	ext, ctype := ".ics", "text/calendar"
	if col.Kind == store.Calendars {
		body = caldav.Merge(objs, name)
	} else {
		ext, ctype = ".vcf", "text/vcard"
		for _, b := range objs {
			body = append(body, b...)
			if len(b) > 0 && b[len(b)-1] != '\n' {
				body = append(body, '\r', '\n')
			}
		}
	}
	file := strings.TrimSpace(unsafeFileChars.ReplaceAllString(name, "_"))
	if file == "" {
		file = "export"
	}
	w.Header().Set("Content-Type", ctype+"; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, file, ext))
	w.Write(body)
}

// importFile adds the objects of an uploaded .ics / .vcf to a collection
// the user owns. Objects with a UID already in the collection are
// replaced, so importing the same file twice updates instead of
// duplicating.
func (u *UI) importFile(w http.ResponseWriter, r *http.Request) {
	user, raw := u.session(r)
	if user == "" {
		http.Redirect(w, r, u.prefix, http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImport+1<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			u.renderError(w, r, user, raw, errUser("The file is too large (50 MB at most)."))
			return
		}
		http.Error(w, "invalid upload", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	if r.PostFormValue("csrf") != u.csrfToken(raw) {
		http.Error(w, "invalid form (expired page?) - reload and try again", http.StatusForbidden)
		return
	}
	col, err := u.own(user, r.PostFormValue("key"))
	if err != nil {
		u.renderError(w, r, user, raw, err)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		u.renderError(w, r, user, raw, errUser("Choose a file to import."))
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxImport))
	f.Close()
	if err != nil {
		u.renderError(w, r, user, raw, err)
		return
	}
	added, updated, skipped, err := u.importInto(col, data)
	if err != nil {
		u.renderError(w, r, user, raw, errUser("Could not read the file: "+err.Error()))
		return
	}
	u.log.Info("import", "user", user, "collection", col.Key(), "new", added, "updated", updated, "skipped", skipped)
	d := u.page(r, user, raw)
	name := col.Meta().DisplayName
	if name == "" {
		name = col.Name
	}
	d.Msg = fmt.Sprintf("Imported into %s: %d new, %d updated, %d skipped.", name, added, updated, skipped)
	u.render(w, http.StatusOK, "dashboard.html", d)
}

func (u *UI) importInto(col *store.Collection, data []byte) (added, updated, skipped int, err error) {
	type obj struct {
		uid  string
		data []byte
	}
	var objs []obj
	ext := ".ics"
	if col.Kind == store.Calendars {
		split, sk, err := caldav.Split(data)
		if err != nil {
			return 0, 0, 0, err
		}
		skipped = sk
		comps := col.Meta().Components
		for _, o := range split {
			cal, err := ical.ParseBytes(o.Data)
			if err != nil {
				skipped++
				continue
			}
			if _, _, err := caldav.Validate(cal, comps); err != nil {
				skipped++ // e.g. tasks into an events-only calendar
				continue
			}
			objs = append(objs, obj{o.UID, o.Data})
		}
	} else {
		ext = ".vcf"
		cards, sk, err := carddav.Split(data)
		if err != nil {
			return 0, 0, 0, err
		}
		skipped = sk
		for _, c := range cards {
			objs = append(objs, obj{c.UID, c.Data})
		}
	}
	for _, o := range objs {
		name, exists := col.ByUID(o.uid)
		if !exists {
			name = objectName(col, o.uid, ext)
		}
		if _, _, err := col.Put(name, o.data, store.Cond{}); err != nil {
			skipped++
			continue
		}
		if exists {
			updated++
		} else {
			added++
		}
	}
	return added, updated, skipped, nil
}

var safeUID = regexp.MustCompile(`^[A-Za-z0-9@._-]{1,200}$`)

// objectName derives a resource name from the UID when that is safe and
// free, otherwise a random one.
func objectName(col *store.Collection, uid, ext string) string {
	if safeUID.MatchString(uid) && !strings.HasPrefix(uid, ".") {
		if _, err := col.Stat(uid + ext); err != nil {
			return uid + ext
		}
	}
	return newCollectionName() + ext
}

func (u *UI) deleteCollection(user, key, confirm string) error {
	col, err := u.own(user, key)
	if err != nil {
		return err
	}
	if confirm != "yes" {
		return errUser("Tick the confirmation box to delete the collection.")
	}
	return u.st.DeleteCollection(col)
}
