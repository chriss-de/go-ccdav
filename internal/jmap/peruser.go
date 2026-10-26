package jmap

import "github.com/chriss-de/go-ccdav/internal/peruser"

// overlayOf returns the caller's per-user data for an event, if any.
func (v *View) overlayOf(id string) peruser.Data {
	if ev, ok := v.Overlay().Events[id]; ok {
		return peruser.Decode(ev.Data)
	}
	return peruser.Data{}
}
