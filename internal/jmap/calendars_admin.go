package jmap

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/chriss-de/go-ccdav/internal/ical"
	"github.com/chriss-de/go-ccdav/internal/rrule"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// SetCalendarHasEvent: destroying a non-empty calendar without
// onDestroyRemoveEvents.
const SetCalendarHasEvent = "calendarHasEvent"

// newCalendarComponents are what calendars created via JMAP take: events
// and tasks (the default calendar takes journals too).
var newCalendarComponents = []string{"VEVENT", "VTODO"}

type calendarSetArgs struct {
	SetArgs
	OnDestroyRemoveEvents bool    `json:"onDestroyRemoveEvents"`
	OnSuccessSetIsDefault *string `json:"onSuccessSetIsDefault"`
}

// calendarChange holds the client-settable Calendar properties given.
type calendarChange struct {
	name, description, color, timeZone, availability *string
	sortOrder                                        *int
	isSubscribed, isVisible                          *bool
	alertsWith, alertsWithout                        *json.RawMessage
	shares                                           *[]store.Share
}

func (c *Call) parseCalendarProps(owner string, obj, current map[string]any) (calendarChange, error) {
	var ch calendarChange
	for _, k := range keysWithRemoved(obj, current) {
		val := obj[k] // a property a patch removed is null
		if current != nil && jsonEqual(val, current[k]) {
			continue // unchanged, including server-set ones
		}
		bad := func() error { return failSet(SetInvalidProperties, "invalid "+k, k) }
		optString := func() (*string, error) {
			s, ok := val.(string)
			if val != nil && !ok {
				return nil, bad()
			}
			return &s, nil
		}
		var err error
		switch k {
		case "name":
			s, ok := val.(string)
			if !ok || s == "" {
				return ch, bad()
			}
			ch.name = &s
		case "description":
			ch.description, err = optString()
		case "color":
			ch.color, err = optString()
		case "timeZone":
			if ch.timeZone, err = optString(); err == nil && *ch.timeZone != "" {
				if _, lerr := time.LoadLocation(*ch.timeZone); lerr != nil {
					err = bad()
				}
			}
		case "includeInAvailability":
			s, ok := val.(string)
			if !ok || !slices.Contains([]string{"all", "attending", "none"}, s) {
				return ch, bad()
			}
			ch.availability = &s
		case "sortOrder":
			f, ok := val.(float64)
			if !ok || f < 0 || f != float64(int(f)) {
				return ch, bad()
			}
			n := int(f)
			ch.sortOrder = &n
		case "isSubscribed", "isVisible":
			b, ok := val.(bool)
			if !ok {
				return ch, bad()
			}
			if k == "isSubscribed" {
				ch.isSubscribed = &b
			} else {
				ch.isVisible = &b
			}
		case "defaultAlertsWithTime", "defaultAlertsWithoutTime":
			m, ok := val.(map[string]any)
			if val != nil && !ok {
				return ch, bad()
			}
			// Each must be a valid alert, as on an event.
			for id, a := range m {
				if am, ok := a.(map[string]any); !ok || !validID(id) || !validAlert(am) {
					return ch, failSet(SetInvalidProperties, "invalid alert "+id, k+"/"+id)
				}
			}
			var raw json.RawMessage
			if len(m) > 0 {
				raw, _ = json.Marshal(m)
			}
			if k == "defaultAlertsWithTime" {
				ch.alertsWith = &raw
			} else {
				ch.alertsWithout = &raw
			}
		case "shareWith":
			if current != nil && current[k] == nil {
				return ch, failSet(SetForbidden, "only the owner can share", k)
			}
			shares, err := parseShareWith(c.h.cfg.Get(), owner, val)
			if err != nil {
				return ch, err
			}
			ch.shares = &shares
		default:
			return ch, failSet(SetInvalidProperties, k+" cannot be set", k)
		}
		if err != nil {
			return ch, err
		}
	}
	return ch, nil
}

// keysWithRemoved lists obj's keys plus those of current that obj lacks.
func keysWithRemoved(obj, current map[string]any) []string {
	ks := slices.Collect(maps.Keys(obj))
	for k := range current {
		if _, ok := obj[k]; !ok {
			ks = append(ks, k)
		}
	}
	slices.Sort(ks)
	return ks
}

func validID(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validAlert(a map[string]any) bool {
	trig, _ := a["trigger"].(map[string]any)
	switch str(trig["@type"]) {
	case "AbsoluteTrigger":
		_, err := time.Parse(time.RFC3339, str(trig["when"]))
		return err == nil
	case "OffsetTrigger", "":
		_, err := rrule.ParseDuration(str(trig["offset"]))
		return err == nil
	}
	return true
}

// tzCalendar is a calendar's stored time zone: a VCALENDAR with one
// VTIMEZONE (the CalDAV calendar-timezone property).
func tzCalendar(name string) string {
	if name == "" {
		return ""
	}
	vtz, err := rrule.VTimezone(name, time.Now().AddDate(-1, 0, 0))
	if err != nil {
		return ""
	}
	cal := &ical.Component{Name: "VCALENDAR", Props: []*ical.Prop{{Name: "VERSION", Value: "2.0"}, {Name: "PRODID", Value: "-//ccdav//JMAP//EN"}},
		Children: []*ical.Component{vtz}}
	return string(cal.Bytes())
}

// applyOwner writes a change into the collection's properties.
func (ch calendarChange) applyOwner(m *store.Meta) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&m.DisplayName, ch.name)
	set(&m.Description, ch.description)
	set(&m.Color, ch.color)
	if ch.availability != nil {
		m.Availability = *ch.availability
		if m.Availability == "all" {
			m.Availability = ""
		}
	}
	if ch.timeZone != nil {
		m.Timezone = tzCalendar(*ch.timeZone)
	}
	if ch.sortOrder != nil {
		m.Order = strconv.Itoa(*ch.sortOrder)
	}
	if ch.isSubscribed != nil {
		m.Unsubscribed = !*ch.isSubscribed
	}
	if ch.isVisible != nil {
		m.Invisible = !*ch.isVisible
	}
	if ch.alertsWith != nil {
		m.DefaultAlertsWithTime = *ch.alertsWith
	}
	if ch.alertsWithout != nil {
		m.DefaultAlertsWithoutTime = *ch.alertsWithout
	}
}

// calendarSet implements Calendar/set. Someone a calendar is shared with
// changes only their own name, color, order, subscription and visibility.
func (c *Call) calendarSet(raw json.RawMessage) (any, error) {
	var args calendarSetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	v, err := c.View(args.AccountID, store.Calendars)
	if err != nil {
		return nil, err
	}
	old := v.State(store.LevelCollections)
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
		created, err := c.createCalendar(v, cid, args.Create[cid])
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
	if v, err = c.View(args.AccountID, store.Calendars); err != nil {
		return nil, err
	}
	if args.OnSuccessSetIsDefault != nil {
		id, _ := c.ResolveID(*args.OnSuccessSetIsDefault)
		if col := v.Colls[id]; col == nil || col.Name != c.h.defaultName(store.Calendars) {
			return nil, Errorf(ErrInvalidArguments, "the default calendar can't be changed")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(args.Update)) {
		if err := c.updateCalendar(v, id, args.Update[id]); err != nil {
			if err := fail(&resp.NotUpdated, id, err); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Updated == nil {
			resp.Updated = map[string]any{}
		}
		resp.Updated[id] = nil
	}
	for _, id := range args.Destroy {
		if err := c.destroyCalendar(v, id, args.OnDestroyRemoveEvents); err != nil {
			if err := fail(&resp.NotDestroyed, id, err); err != nil {
				return nil, err
			}
			continue
		}
		resp.Destroyed = append(resp.Destroyed, id)
	}
	resp.NewState = v.State(store.LevelCollections)
	return resp, nil
}

func (c *Call) createCalendar(v *View, cid string, raw json.RawMessage) (map[string]any, error) {
	if !v.Own {
		return nil, failSet(SetForbidden, "calendars can only be created in your own account")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, failSet(SetInvalidProperties, "not an object")
	}
	ch, err := c.parseCalendarProps(v.Owner, obj, nil)
	if err != nil {
		return nil, err
	}
	if ch.name == nil {
		return nil, failSet(SetInvalidProperties, "name is required", "name")
	}
	m := store.Meta{Components: newCalendarComponents}
	ch.applyOwner(&m)
	col, err := c.h.store.CreateCollection(v.Owner, store.Calendars, newUUID(), m)
	if err != nil {
		return nil, err
	}
	if ch.shares != nil {
		if err := col.SetShares(*ch.shares); err != nil {
			return nil, err
		}
	}
	id := col.ID()
	c.Created(cid, id)
	v.Colls[id], v.Access[id] = col, store.AccessOwner
	return withoutKeys(c.calendar(v, col), "name", "description", "color"), nil
}

func (c *Call) updateCalendar(v *View, id string, raw json.RawMessage) error {
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		return failSet(SetInvalidPatch, "not a PatchObject")
	}
	real, _ := c.ResolveID(id)
	col := v.Colls[real]
	if col == nil {
		return failSet(SetNotFound, "no such calendar")
	}
	cur := c.calendar(v, col)
	next := copyJSON(cur)
	if err := ApplyPatch(next, patch); err != nil {
		return failSet(SetInvalidPatch, err.Error())
	}
	ch, err := c.parseCalendarProps(v.Owner, next, copyJSON(cur))
	if err != nil {
		return err
	}
	if !v.Own {
		for prop, set := range map[string]bool{"description": ch.description != nil, "timeZone": ch.timeZone != nil,
			"includeInAvailability": ch.availability != nil, "defaultAlertsWithTime": ch.alertsWith != nil, "defaultAlertsWithoutTime": ch.alertsWithout != nil} {
			if set {
				return failSet(SetForbidden, "only the owner can change "+prop, prop)
			}
		}
		return c.h.store.UpdateSharePrefs(c.User, col.Key(), func(p *store.SharePrefs) {
			if ch.name != nil {
				p.DisplayName = *ch.name
			}
			if ch.color != nil {
				p.Color = *ch.color
			}
			if ch.sortOrder != nil {
				p.Order = strconv.Itoa(*ch.sortOrder)
			}
			if ch.isSubscribed != nil {
				p.Hidden = !*ch.isSubscribed
			}
			if ch.isVisible != nil {
				p.Invisible = !*ch.isVisible
			}
		})
	}
	if ch.shares != nil {
		if err := col.SetShares(*ch.shares); err != nil {
			return err
		}
	}
	return col.UpdateMeta(ch.applyOwner)
}

func (c *Call) destroyCalendar(v *View, id string, removeEvents bool) error {
	real, _ := c.ResolveID(id)
	col := v.Colls[real]
	if col == nil {
		return failSet(SetNotFound, "no such calendar")
	}
	if !v.Own {
		return failSet(SetForbidden, "shared with you; set isSubscribed to false to hide it")
	}
	if len(col.List()) > 0 && !removeEvents {
		return failSet(SetCalendarHasEvent, "not empty; set onDestroyRemoveEvents")
	}
	return c.h.store.DeleteCollection(col)
}
