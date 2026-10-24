package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"davd/internal/dav"
	"davd/internal/store"
)

// Sync tokens are URIs (RFC 6578 §3.2) carrying the collection's sync ID
// and its change counter.
const syncTokenPrefix = "http://davd/ns/sync/"

func formatSyncToken(id string, n uint64) string {
	return syncTokenPrefix + id + "-" + strconv.FormatUint(n, 10)
}

// parseSyncToken returns the counter of a token for the collection with
// the given sync ID. An empty token is the initial sync (0).
func parseSyncToken(tok, id string) (uint64, error) {
	if tok == "" {
		return 0, nil
	}
	rest, ok := strings.CutPrefix(tok, syncTokenPrefix)
	if !ok {
		return 0, store.ErrInvalidToken
	}
	i := strings.LastIndexByte(rest, '-')
	if i < 0 || rest[:i] != id {
		return 0, store.ErrInvalidToken
	}
	// "-0" (from syncing an empty collection) means "everything", which
	// is the same as an initial sync.
	n, err := strconv.ParseUint(rest[i+1:], 10, 64)
	if err != nil {
		return 0, store.ErrInvalidToken
	}
	return n, nil
}

// syncCollection implements the DAV:sync-collection REPORT (RFC 6578) on
// calendars and address books, with sync-level 1.
func (s *Server) syncCollection(w http.ResponseWriter, user string, res *resource, body *dav.Element) {
	if res.kind != kCollection || res.col == nil {
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSDAV, "supported-report"), "sync-collection is only supported on calendars and address books")
		return
	}
	if lvl := strings.TrimSpace(body.Child(dav.NSDAV, "sync-level").Text); lvl != "" && lvl != "1" && lvl != "infinite" {
		http.Error(w, "bad sync-level", http.StatusBadRequest)
		return
	}
	limit := 0
	if l := body.Child(dav.NSDAV, "limit"); l != nil {
		n, err := strconv.Atoi(strings.TrimSpace(l.Child(dav.NSDAV, "nresults").Text))
		if err != nil || n < 1 {
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	col := res.col
	id := col.SyncID()
	since, err := parseSyncToken(strings.TrimSpace(body.Child(dav.NSDAV, "sync-token").Text), id)
	var changes []store.Change
	var cur uint64
	if err == nil {
		changes, cur, err = col.Changes(since)
	}
	if errors.Is(err, store.ErrInvalidToken) {
		dav.WriteError(w, http.StatusForbidden, dav.N(dav.NSDAV, "valid-sync-token"), "")
		return
	}
	if err != nil {
		s.httpError(w, err)
		return
	}

	ms := &dav.Multistatus{}
	// Truncate incremental results at a token boundary. Each change has a
	// distinct token there, so the client can resume from the last one.
	// (Initial results all share the current token and are not truncated.)
	if limit > 0 && since > 0 && len(changes) > limit {
		changes = changes[:limit]
		cur = changes[limit-1].Token
		ms.Responses = append(ms.Responses, &dav.Response{
			Href:   res.href(),
			Status: http.StatusInsufficientStorage,
			Error:  dav.E(dav.N(dav.NSDAV, "number-of-matches-within-limits"), ""),
		})
	}
	props := propElements(body)
	for _, c := range changes {
		// The current state wins over the logged operation.
		if info, err := col.Stat(c.Name); err == nil {
			ms.Responses = append(ms.Responses, s.responseFor(user, res.object(info, true), props, false))
		} else {
			o := res.object(store.ObjectInfo{Name: c.Name}, false)
			ms.Responses = append(ms.Responses, &dav.Response{Href: o.href(), Status: http.StatusNotFound})
		}
	}
	ms.SyncToken = formatSyncToken(id, cur)
	ms.Write(w)
}
