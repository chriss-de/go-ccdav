package caldav

import (
	"bytes"
	"slices"

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// Object is one calendar object resource produced by Split.
type Object struct {
	UID  string
	Data []byte
}

// Split turns an iCalendar file (an export, possibly with several
// VCALENDARs) into calendar object resources: one per UID, holding the
// master and its overrides plus the VTIMEZONEs they reference. Components
// without UID are counted as skipped.
func Split(data []byte) (objs []Object, skipped int, err error) {
	cals, err := ical.ParseAll(bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	var (
		order  []string
		groups = map[string][]*ical.Component{}
		tzs    = map[string]*ical.Component{}
		head   []*ical.Prop
	)
	for _, cal := range cals {
		if cal.Name != "VCALENDAR" {
			skipped++
			continue
		}
		if head == nil {
			for _, p := range cal.Props {
				switch p.Name {
				case "VERSION", "PRODID", "CALSCALE":
					head = append(head, p)
				}
			}
		}
		for _, c := range cal.Children {
			if c.Name == "VTIMEZONE" {
				if id := c.Value("TZID"); id != "" && tzs[id] == nil {
					tzs[id] = c
				}
				continue
			}
			uid := c.Value("UID")
			if uid == "" {
				skipped++
				continue
			}
			if _, ok := groups[uid]; !ok {
				order = append(order, uid)
			}
			groups[uid] = append(groups[uid], c)
		}
	}
	if !slices.ContainsFunc(head, func(p *ical.Prop) bool { return p.Name == "VERSION" }) {
		head = append(head, &ical.Prop{Name: "VERSION", Value: "2.0"})
	}
	if !slices.ContainsFunc(head, func(p *ical.Prop) bool { return p.Name == "PRODID" }) {
		head = append(head, &ical.Prop{Name: "PRODID", Value: "-//ccdav//import//EN"})
	}
	for _, uid := range order {
		out := &ical.Component{Name: "VCALENDAR", Props: head}
		seen := map[string]bool{}
		for _, c := range groups[uid] {
			for _, id := range tzidsOf(c) {
				if tz := tzs[id]; tz != nil && !seen[id] {
					seen[id] = true
					out.Children = append(out.Children, tz)
				}
			}
		}
		out.Children = append(out.Children, groups[uid]...)
		objs = append(objs, Object{UID: uid, Data: out.Bytes()})
	}
	return objs, skipped, nil
}

func tzidsOf(c *ical.Component) []string {
	var out []string
	for _, p := range c.Props {
		if id := p.Param("TZID"); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, ch := range c.Children {
		for _, id := range tzidsOf(ch) {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// Merge combines calendar object resources into one iCalendar file (an
// export). VTIMEZONEs are included once per TZID; unparsable objects are
// skipped.
func Merge(objects [][]byte, name string) []byte {
	out := &ical.Component{Name: "VCALENDAR", Props: []*ical.Prop{
		{Name: "VERSION", Value: "2.0"},
		{Name: "PRODID", Value: "-//ccdav//export//EN"},
	}}
	if name != "" {
		out.Props = append(out.Props, &ical.Prop{Name: "X-WR-CALNAME", Value: ical.EscapeText(name)})
	}
	seen := map[string]bool{}
	var comps []*ical.Component
	for _, b := range objects {
		cal, err := ical.ParseBytes(b)
		if err != nil || cal.Name != "VCALENDAR" {
			continue
		}
		for _, c := range cal.Children {
			if c.Name == "VTIMEZONE" {
				if id := c.Value("TZID"); !seen[id] {
					seen[id] = true
					out.Children = append(out.Children, c)
				}
				continue
			}
			comps = append(comps, c)
		}
	}
	out.Children = append(out.Children, comps...)
	return out.Bytes()
}
