// Package directory maintains the generated, read-only address book in
// every user's home: one vCard per configured user and one group vCard per
// group the home's owner belongs to. It is a normal stored collection, so
// sync, queries and export work unchanged; Sync rewrites only what changed.
package directory

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// Name is the collection name of the directory in each address book home.
const Name = "ccdav-directory"

var meta = store.Meta{
	DisplayName: "Directory",
	Description: "Everyone on this server. Generated, read-only.",
	ReadOnly:    true,
}

// Sync brings every user's directory in line with cfg. With the directory
// turned off it deletes the generated address books instead.
func Sync(cfg *config.Config, st *store.Store, defaults map[string]store.Meta) error {
	var errs []error
	if !cfg.DirectoryEnabled() {
		for _, c := range st.All() {
			if c.Kind == store.AddressBooks && c.Name == Name && c.ReadOnly() {
				errs = append(errs, st.DeleteCollection(c))
			}
		}
		return errors.Join(errs...)
	}
	for _, user := range slices.Sorted(maps.Keys(cfg.Users)) {
		errs = append(errs, syncUser(cfg, st, user, defaults))
	}
	return errors.Join(errs...)
}

func syncUser(cfg *config.Config, st *store.Store, user string, defaults map[string]store.Meta) error {
	// The home's default address books must come first: EnsureHome only
	// creates them for a home that doesn't exist yet.
	if err := st.EnsureHome(user, store.AddressBooks, defaults); err != nil {
		return err
	}
	col, err := st.Collection(user, store.AddressBooks, Name)
	if errors.Is(err, store.ErrNotFound) {
		col, err = st.CreateCollection(user, store.AddressBooks, Name, meta)
	}
	if err != nil {
		return err
	}
	if !col.ReadOnly() {
		return fmt.Errorf("directory: %s has an own address book named %q, not touching it", user, Name)
	}
	want := Cards(cfg, user)
	for name, data := range want {
		if old, _, err := col.Get(name); err == nil && bytes.Equal(old, data) {
			continue
		}
		if _, _, err := col.Put(name, data, store.Cond{}); err != nil {
			return fmt.Errorf("directory: %s/%s: %w", user, name, err)
		}
	}
	for _, o := range col.List() {
		if _, ok := want[o.Name]; !ok {
			if err := col.Delete(o.Name, store.Cond{}); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// Cards returns the directory's vCards for user's home, by object name.
func Cards(cfg *config.Config, user string) map[string][]byte {
	out := make(map[string][]byte)
	add := func(uid string, props ...*ical.Prop) {
		c := &ical.Component{Name: "VCARD", Props: append([]*ical.Prop{
			{Name: "VERSION", Value: "3.0"},
			{Name: "PRODID", Value: "-//ccdav//directory//EN"},
			{Name: "UID", Value: uid},
		}, props...)}
		out[uid+".vcf"] = c.Bytes()
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Users)) {
		u := cfg.Users[name]
		fn := u.Name
		if fn == "" {
			fn = name
		}
		props := []*ical.Prop{
			{Name: "FN", Value: ical.EscapeText(fn)},
			{Name: "N", Value: structuredName(fn)},
		}
		if u.Email != "" {
			props = append(props, &ical.Prop{Name: "EMAIL", Params: []ical.Param{{Name: "TYPE", Values: []string{"INTERNET"}}}, Value: ical.EscapeText(u.Email)})
		}
		add(UserUID(name), props...)
	}
	for _, g := range cfg.GroupsOf(user) {
		props := []*ical.Prop{
			{Name: "FN", Value: ical.EscapeText(g)},
			{Name: "N", Value: ical.EscapeText(g) + ";;;;"},
			// Apple's group format; DAVx⁵ reads it with "groups are
			// separate vCards".
			{Name: "X-ADDRESSBOOKSERVER-KIND", Value: "group"},
		}
		members := slices.Clone(cfg.Groups[g])
		slices.Sort(members)
		for _, m := range slices.Compact(members) {
			props = append(props, &ical.Prop{Name: "X-ADDRESSBOOKSERVER-MEMBER", Value: "urn:uuid:" + UserUID(m)})
		}
		add(uuidFor("group:"+g), props...)
	}
	return out
}

// structuredName guesses N (family;given;...) from a full name: the last
// word is the family name. Only used for sorting in clients; FN is what
// they show.
func structuredName(fn string) string {
	i := strings.LastIndexByte(fn, ' ')
	if i < 0 {
		return ";" + ical.EscapeText(fn) + ";;;"
	}
	return ical.EscapeText(fn[i+1:]) + ";" + ical.EscapeText(fn[:i]) + ";;;"
}

// UserUID is the stable vCard UID of a user's directory entry.
func UserUID(user string) string { return uuidFor("user:" + user) }

// uuidFor derives a stable name-based UUID (version 5 layout) from s.
func uuidFor(s string) string {
	h := sha1.Sum([]byte("ccdav-directory:" + s))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
