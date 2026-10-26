package jmap

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/jscalendar"
	"github.com/chriss-de/go-ccdav/internal/peruser"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// CapCalendarsParse announces CalendarEvent/parse.
const CapCalendarsParse = "urn:ietf:params:jmap:calendars:parse"

// eventServerSet are CalendarEvent properties only the server sets.
var eventServerSet = []string{"id", "baseEventId", "isOrigin", "utcStart", "utcEnd"}

// eventImmutable are properties an update may not change.
var eventImmutable = []string{"uid", "@type", "version", "recurrenceId", "recurrenceIdTimeZone"}

type eventSetArgs struct {
	SetArgs
	SendSchedulingMessages bool `json:"sendSchedulingMessages"` // no scheduling yet: ignored
}

// storeEvent validates and writes a calendar object; a UID taken by
// another object of the collection is alreadyExists.
func (c *Call) storeEvent(v *View, col *store.Collection, name string, cal *ical.Component, cond store.Cond) error {
	if _, _, err := caldav.Validate(cal, col.Meta().Components); err != nil {
		return failSet(SetInvalidProperties, err.Error())
	}
	_, _, err := col.Put(name, cal.Bytes(), cond)
	var dup *store.UIDConflictError
	if errors.As(err, &dup) {
		id, _ := v.Log.ObjectID(col, dup.Existing)
		return &setErr{SetError{Type: SetAlreadyExists, Description: "the UID is in use", ExistingID: id}}
	}
	if errors.Is(err, store.ErrQuota) {
		return failSet(SetOverQuota, "the owner's storage quota is used up")
	}
	return err
}

// eventSet implements CalendarEvent/set. Ids of instances
// ("<id>_<recurrence id>") change or cancel that one occurrence.
func (c *Call) eventSet(raw json.RawMessage) (any, error) {
	var args eventSetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.Calendars)
	if err != nil {
		return nil, err
	}
	old := v.State(store.LevelObjects)
	if err := CheckSet(args.SetArgs, old); err != nil {
		return nil, err
	}
	resp := &SetResponse{AccountID: args.AccountID, OldState: &old}
	fail := func(m *map[string]SetError, id string, err error) error {
		se, ok := asSetError(err)
		if !ok {
			return err
		}
		if *m == nil {
			*m = map[string]SetError{}
		}
		(*m)[id] = *se
		return nil
	}
	for _, cid := range slices.Sorted(maps.Keys(args.Create)) {
		created, err := c.createEvent(v, cid, args.Create[cid])
		if err != nil {
			if err := fail(&resp.NotCreated, cid, err); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[cid] = created
	}
	for _, id := range slices.Sorted(maps.Keys(args.Update)) {
		changed, err := c.updateEvent(v, id, args.Update[id])
		if err != nil {
			if err := fail(&resp.NotUpdated, id, err); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Updated == nil {
			resp.Updated = map[string]any{}
		}
		resp.Updated[id] = changed
	}
	for _, id := range args.Destroy {
		if err := c.destroyEvent(v, id); err != nil {
			if err := fail(&resp.NotDestroyed, id, err); err != nil {
				return nil, err
			}
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	resp.NewState = v.State(store.LevelObjects)
	return resp, nil
}

func (c *Call) createEvent(v *View, cid string, raw json.RawMessage) (map[string]any, error) {
	var ev map[string]any
	if err := json.Unmarshal(raw, &ev); err != nil || ev == nil {
		return nil, failSet(SetInvalidProperties, "not an object")
	}
	for _, p := range eventServerSet {
		if _, ok := ev[p]; ok {
			return nil, failSet(SetInvalidProperties, p+" is set by the server", p)
		}
	}
	if t, ok := ev["@type"]; ok && t != "Event" {
		return nil, failSet(SetInvalidProperties, "@type must be Event", "@type")
	}
	col, err := c.target(v, ev["calendarIds"], "calendarIds")
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	uid, _ := ev["uid"].(string)
	if uid == "" {
		uid = newUUID()
		ev["uid"] = uid
		out["uid"] = uid
	}
	if other, ok := col.ByUID(uid); ok {
		id, _ := v.Log.ObjectID(col, other)
		return nil, &setErr{SetError{Type: SetAlreadyExists, Description: "the UID is in use", ExistingID: id}}
	}
	cal, _, err := jscalendar.Merge(nil, ev)
	if err != nil {
		return nil, err
	}
	name := newUUID() + ".ics"
	if err := c.storeEvent(v, col, name, cal, store.Cond{IfNoneMatch: "*"}); err != nil {
		return nil, err
	}
	ref, ok := v.Log.ObjectAt(col, name)
	if !ok {
		return nil, errors.New("jmap: created event has no id")
	}
	c.Created(cid, ref.ID)
	out["id"] = ref.ID
	if e, ok := c.h.load(eventType, v, ref); ok {
		out["created"], out["updated"] = e.obj["created"], e.obj["updated"]
	}
	return out, nil
}

// splitInstance separates an instance id into the event id and the
// LocalDateTime recurrence id.
func splitInstance(id string) (base, rid string, ok bool) {
	base, r, ok := strings.Cut(id, "_")
	if !ok {
		return id, "", false
	}
	t, err := time.Parse("20060102T150405", r)
	if err != nil {
		return id, "", false
	}
	return base, t.Format(localFmt), true
}

func (c *Call) updateEvent(v *View, id string, raw json.RawMessage) (map[string]any, error) {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		return nil, failSet(SetInvalidPatch, "not a PatchObject")
	}
	for path := range patch {
		top, _, _ := strings.Cut(path, "/")
		if slices.Contains(eventServerSet, top) {
			return nil, failSet(SetInvalidProperties, top+" is set by the server", top)
		}
	}
	real, ok := c.ResolveID(id)
	if !ok {
		return nil, failSet(SetNotFound, "unknown creation id")
	}
	base, rid, isInstance := splitInstance(real)
	var perUser *peruser.Data
	res, err := c.writeEvent(v, base, func(owner map[string]any) (map[string]any, error) {
		if !v.hasOverlay() {
			return patchEvent(owner, patch, isInstance, rid)
		}
		// Someone else's calendar: the patch applies to the caller's view,
		// their per-user values go into their overlay, the rest into the
		// calendar object.
		next, err := patchEvent(copyJSON(peruser.Apply(owner, v.overlayOf(base))), patch, isInstance, rid)
		if err != nil {
			return nil, err
		}
		if paths := jscalendar.Validate(next); len(paths) > 0 {
			return nil, &jscalendar.InvalidError{Paths: paths}
		}
		file, d := peruser.Split(next, owner)
		perUser = &d
		return file, nil
	}, isInstance, rid)
	if err != nil || perUser == nil {
		return res, err
	}
	if err := c.h.store.SetOverlay(c.User, v.Owner, base, peruser.Encode(*perUser)); err != nil {
		if errors.Is(err, store.ErrTooLarge) {
			return nil, failSet(SetTooLarge, "too much per-user data")
		}
		return nil, err
	}
	v.overlay = nil // reload: the new state must count this change
	return res, nil
}

// patchEvent applies a PatchObject to an event, or with isInstance to
// its occurrence at rid (whose result becomes that override's patch).
func patchEvent(e, patch map[string]any, isInstance bool, rid string) (map[string]any, error) {
	if !isInstance {
		next := copyJSON(e)
		if err := ApplyPatch(next, patch); err != nil {
			return nil, failSet(SetInvalidPatch, err.Error())
		}
		for _, p := range eventImmutable {
			if !jsonEqual(next[p], e[p]) {
				return nil, failSet(SetInvalidProperties, p+" cannot be changed", p)
			}
		}
		return next, nil
	}
	inst, ok := instance(e, rid)
	if !ok {
		return nil, failSet(SetNotFound, "no such occurrence")
	}
	delete(inst, "baseEventId")
	next := copyJSON(inst)
	if err := ApplyPatch(next, patch); err != nil {
		return nil, failSet(SetInvalidPatch, err.Error())
	}
	for _, p := range append(eventImmutable, "calendarIds") {
		if !jsonEqual(next[p], inst[p]) {
			return nil, failSet(SetInvalidProperties, p+" cannot be changed for one occurrence", p)
		}
	}
	out := copyJSON(e)
	ro, _ := out["recurrenceOverrides"].(map[string]any)
	if ro == nil {
		ro = map[string]any{}
		out["recurrenceOverrides"] = ro
	}
	ro[rid] = jscalendar.PatchFor(e, rid, next)
	return out, nil
}

// writeEvent loads the stored event base, lets change build the new
// event, and writes it (moving it if calendarIds changed). A CalDAV
// write in between makes it start over, so neither change is lost.
func (c *Call) writeEvent(v *View, base string, change func(e map[string]any) (map[string]any, error), instanceOnly bool, rid string) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		ref, ok := v.Log.Lookup(base)
		if !ok || v.Colls[ref.Coll] == nil {
			return nil, failSet(SetNotFound, "no such event")
		}
		col := v.Colls[ref.Coll]
		data, info, err := col.Get(ref.Name)
		if err != nil {
			return nil, failSet(SetNotFound, "no such event")
		}
		e, ok := c.h.loadRaw(eventType, v, ref)
		if !ok {
			return nil, failSet(SetForbidden, "the stored event cannot be converted")
		}
		if instanceOnly {
			if _, isOverride := overrides(e.obj)[rid]; !isOverride && !c.h.occurs(c.ctx, v, ref, e.obj, rid) {
				return nil, failSet(SetNotFound, "no such occurrence")
			}
		}
		next, err := change(copyJSON(e.obj))
		if err != nil {
			return nil, err
		}
		target := col
		if !jsonEqual(next["calendarIds"], e.obj["calendarIds"]) {
			if target, err = c.target(v, next["calendarIds"], "calendarIds"); err != nil {
				return nil, err
			}
		}
		prev, err := ical.ParseBytes(data)
		if err != nil {
			return nil, failSet(SetForbidden, "the stored event cannot be parsed")
		}
		cal, changed, err := jscalendar.Merge(prev, next)
		if err != nil {
			return nil, err
		}
		// Only writing needs write access: per-user changes don't.
		if changed || target != col {
			if err := v.writable(ref.Coll); err != nil {
				return nil, err
			}
		}
		if target != col {
			name := ref.Name
			if _, err := target.Stat(name); err == nil {
				name = newUUID() + ".ics"
			}
			if _, err := col.Move(ref.Name, target, name, store.Cond{IfMatch: info.ETag}); err != nil {
				if errors.Is(err, store.ErrPrecondition) && attempt < maxWriteRetries {
					continue
				}
				var dup *store.UIDConflictError
				if errors.As(err, &dup) {
					other, _ := v.Log.ObjectID(target, dup.Existing)
					return nil, &setErr{SetError{Type: SetAlreadyExists, Description: "the UID is in use in the target calendar", ExistingID: other}}
				}
				return nil, err
			}
			col, ref.Name = target, name
			if info, err = col.Stat(name); err != nil {
				return nil, err
			}
		}
		if !changed {
			return nil, nil
		}
		if testHookBeforeWrite != nil {
			testHookBeforeWrite()
		}
		err = c.storeEvent(v, col, ref.Name, cal, store.Cond{IfMatch: info.ETag})
		if errors.Is(err, store.ErrPrecondition) && attempt < maxWriteRetries {
			continue
		}
		if err != nil {
			return nil, err
		}
		ref, _ = v.Log.Lookup(base)
		if e, ok := c.h.load(eventType, v, ref); ok {
			return map[string]any{"updated": e.obj["updated"]}, nil
		}
		return nil, nil
	}
}

// destroyEvent deletes an event, or cancels one occurrence of it.
func (c *Call) destroyEvent(v *View, id string) error {
	real, ok := c.ResolveID(id)
	if !ok {
		return failSet(SetNotFound, "unknown creation id")
	}
	base, rid, isInstance := splitInstance(real)
	if isInstance {
		_, err := c.writeEvent(v, base, func(e map[string]any) (map[string]any, error) {
			ro, _ := e["recurrenceOverrides"].(map[string]any)
			if ro == nil {
				ro = map[string]any{}
				e["recurrenceOverrides"] = ro
			}
			ro[rid] = map[string]any{"excluded": true}
			return e, nil
		}, true, rid)
		return err
	}
	ref, ok := v.Log.Lookup(base)
	if !ok || v.Colls[ref.Coll] == nil {
		return failSet(SetNotFound, "no such event")
	}
	if err := v.writable(ref.Coll); err != nil {
		return err
	}
	err := v.Colls[ref.Coll].Delete(ref.Name, store.Cond{})
	if errors.Is(err, store.ErrNotFound) {
		return failSet(SetNotFound, "no such event")
	}
	return err
}

// eventCopy implements CalendarEvent/copy: the stored calendar object is
// copied as it is, then the overrides are merged in.
func (c *Call) eventCopy(raw json.RawMessage) (any, error) {
	var args copyArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.FromAccountID == args.AccountID {
		return nil, Errorf(ErrInvalidArguments, "fromAccountId and accountId must differ; use /set to move")
	}
	from, err := c.View(args.FromAccountID, store.Calendars)
	if err != nil {
		var me *MethodError
		if errors.As(err, &me) && me.Type == ErrAccountNotFound {
			return nil, Errorf(ErrFromAccountNotFound, "no account %q", args.FromAccountID)
		}
		return nil, err
	}
	if args.IfFromInState != nil && *args.IfFromInState != from.State(store.LevelObjects) {
		return nil, Errorf(ErrStateMismatch, "fromAccount state differs")
	}
	to, err := c.View(args.AccountID, store.Calendars)
	if err != nil {
		return nil, err
	}
	old := to.State(store.LevelObjects)
	if err := CheckSet(SetArgs{IfInState: args.IfInState, Create: args.Create}, old); err != nil {
		return nil, err
	}
	resp := &copyResponse{FromAccountID: args.FromAccountID, AccountID: args.AccountID, OldState: &old}
	var copied []string
	for _, cid := range slices.Sorted(maps.Keys(args.Create)) {
		srcID, out, err := c.copyEvent(from, to, cid, args.Create[cid])
		if err != nil {
			se, ok := asSetError(err)
			if !ok {
				return nil, err
			}
			if resp.NotCreated == nil {
				resp.NotCreated = map[string]SetError{}
			}
			resp.NotCreated[cid] = *se
			continue
		}
		if resp.Created == nil {
			resp.Created = map[string]any{}
		}
		resp.Created[cid] = out
		copied = append(copied, srcID)
	}
	resp.NewState = to.State(store.LevelObjects)
	if args.OnSuccessDestroyOriginal && len(copied) > 0 {
		b, _ := json.Marshal(SetArgs{AccountID: args.FromAccountID, IfInState: args.DestroyFromIfInState, Destroy: copied})
		set, err := c.eventSet(b)
		if err != nil {
			var me *MethodError
			if !errors.As(err, &me) {
				return nil, err
			}
			c.Respond("error", me)
		} else {
			c.Respond("CalendarEvent/set", set)
		}
	}
	return resp, nil
}

func (c *Call) copyEvent(from, to *View, cid string, raw json.RawMessage) (string, map[string]any, error) {
	var ev map[string]any
	if err := json.Unmarshal(raw, &ev); err != nil || ev == nil {
		return "", nil, failSet(SetInvalidProperties, "not an object")
	}
	srcID, _ := ev["id"].(string)
	ref, ok := from.Log.Lookup(srcID)
	if !ok || from.Colls[ref.Coll] == nil {
		return "", nil, failSet(SetNotFound, "no such event "+srcID)
	}
	data, _, err := from.Colls[ref.Coll].Get(ref.Name)
	if err != nil {
		return "", nil, failSet(SetNotFound, "no such event "+srcID)
	}
	col, err := c.target(to, ev["calendarIds"], "calendarIds")
	if err != nil {
		return "", nil, err
	}
	prev, err := ical.ParseBytes(data)
	if err != nil {
		return "", nil, failSet(SetForbidden, "the stored event cannot be parsed")
	}
	cal := prev
	if overrides := withoutKeys(ev, "id", "calendarIds"); len(overrides) > 0 {
		cur, err := jscalendar.FromICal(prev)
		if err != nil {
			return "", nil, failSet(SetForbidden, "the stored event cannot be converted")
		}
		cur = copyJSON(cur)
		for k, val := range overrides {
			if slices.Contains(eventImmutable, k) && !jsonEqual(cur[k], val) {
				return "", nil, failSet(SetInvalidProperties, k+" cannot be changed", k)
			}
			cur[k] = val
		}
		if cal, _, err = jscalendar.Merge(prev, cur); err != nil {
			return "", nil, err
		}
	}
	name := newUUID() + ".ics"
	if err := c.storeEvent(to, col, name, cal, store.Cond{IfNoneMatch: "*"}); err != nil {
		return "", nil, err
	}
	nref, ok := to.Log.ObjectAt(col, name)
	if !ok {
		return "", nil, errors.New("jmap: copied event has no id")
	}
	c.Created(cid, nref.ID)
	out := map[string]any{"id": nref.ID}
	if e, ok := c.h.load(eventType, to, nref); ok {
		out["created"], out["updated"] = e.obj["created"], e.obj["updated"]
	}
	return srcID, out, nil
}

// maxParseBytes bounds the blobs one CalendarEvent/parse reads.
const maxParseBytes = maxSizeUpload

type parseArgs struct {
	AccountID  string    `json:"accountId"`
	BlobIDs    []string  `json:"blobIds"`
	Properties *[]string `json:"properties"`
}

// eventParse implements CalendarEvent/parse: uploaded iCalendar files
// to events, one per UID, without storing anything.
func (c *Call) eventParse(raw json.RawMessage) (any, error) {
	var args parseArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if _, ok := c.h.account(c.User, args.AccountID); !ok {
		return nil, Errorf(ErrAccountNotFound, "no account %q", args.AccountID)
	}
	// Every blob is parsed in full: bound how many, and their total size.
	ids := slices.Compact(slices.Sorted(slices.Values(args.BlobIDs)))
	if len(ids) > MaxObjectsInGet {
		return nil, Errorf(ErrRequestTooLarge, "more than %d blob ids", MaxObjectsInGet)
	}
	parsed := map[string]any{}
	var notFound, notParsable []string
	left := int64(maxParseBytes)
	for _, id := range ids {
		f, err := c.h.blobs.open(c.User, id)
		if err != nil {
			notFound = append(notFound, id)
			continue
		}
		if fi, err := f.Stat(); err == nil {
			if left -= fi.Size(); left < 0 {
				f.Close()
				return nil, Errorf(ErrRequestTooLarge, "the blobs are larger than %d bytes together", maxParseBytes)
			}
		}
		cals, err := ical.ParseAll(f)
		f.Close()
		if err != nil {
			notParsable = append(notParsable, id)
			continue
		}
		var events []any
		for _, cal := range cals {
			for _, one := range byUID(cal) {
				e, err := jscalendar.FromICal(one)
				if err != nil {
					continue
				}
				for _, k := range []string{"id", "baseEventId", "calendarIds", "isDraft", "isOrigin"} {
					e[k] = nil
				}
				events = append(events, Select(e, args.Properties))
			}
		}
		if len(events) == 0 {
			notParsable = append(notParsable, id)
			continue
		}
		parsed[id] = events
	}
	return map[string]any{"accountId": args.AccountID, "parsed": nilIfNoKeys(parsed), "notFound": notFound, "notParsable": notParsable}, nil
}

func nilIfNoKeys(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// byUID splits a VCALENDAR into one per event series (UID), each with
// the VTIMEZONEs.
func byUID(cal *ical.Component) []*ical.Component {
	var order []string
	groups := map[string][]*ical.Component{}
	var tzs []*ical.Component
	for _, ch := range cal.Children {
		switch ch.Name {
		case "VEVENT":
			uid := ch.Value("UID")
			if _, seen := groups[uid]; !seen {
				order = append(order, uid)
			}
			groups[uid] = append(groups[uid], ch)
		case "VTIMEZONE":
			tzs = append(tzs, ch)
		}
	}
	var out []*ical.Component
	for _, uid := range order {
		one := &ical.Component{Name: "VCALENDAR", Props: cal.Props}
		one.Children = append(slices.Clone(tzs), groups[uid]...)
		out = append(out, one)
	}
	return out
}
