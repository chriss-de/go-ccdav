package rrule

//go:generate go run ./internal/genwinzones -o windowszones.go

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // embedded IANA database: do not depend on the host

	"github.com/chriss-de/go-ccdav/internal/ical"
)

// maxCachedZones bounds each zone cache: TZIDs and VTIMEZONEs come from
// users, and every distinct one would otherwise stay forever.
const maxCachedZones = 4096

var (
	namedCache sync.Map // tzid -> *time.Location (IANA / Windows / fixed)
	vtzCache   sync.Map // tzid + "\x00" + sha256(VTIMEZONE) -> *time.Location
	namedCount atomic.Int64
	vtzCount   atomic.Int64
	lowerWin   = sync.OnceValue(func() map[string]string {
		m := make(map[string]string, len(windowsZones)+len(windowsDisplayZones))
		for k, v := range windowsDisplayZones {
			m[strings.ToLower(k)] = v
		}
		for k, v := range windowsZones {
			m[strings.ToLower(k)] = v
		}
		return m
	})
)

// LoadLocation resolves a TZID. The lookup order is:
//
//  1. an IANA name, also after stripping vendor prefixes such as
//     "/mozilla.org/20050126_1/Europe/Berlin" or a leading "/";
//  2. a Windows zone name ("W. Europe Standard Time") or Windows display
//     name ("(UTC+01:00) Amsterdam, Berlin, ...") via the CLDR table;
//  3. a VTIMEZONE in cal with that TZID, converted to a *time.Location;
//  4. a fixed offset spelled like "UTC+01:00", "GMT-0500" or "Etc/GMT+1";
//
// and finally UTC with ok == false. Results are cached. TZIDs longer than
// maxTZID are not looked up.
func LoadLocation(tzid string, cal *ical.Component) (loc *time.Location, ok bool) {
	tzid = strings.TrimSpace(strings.Trim(tzid, `"`))
	if tzid == "" || len(tzid) > maxTZID {
		return time.UTC, false
	}
	if v, hit := namedCache.Load(tzid); hit {
		return v.(*time.Location), true
	}
	if loc := lookupNamed(tzid); loc != nil {
		cache(&namedCache, &namedCount, tzid, loc)
		return loc, true
	}
	if cal != nil {
		for _, vtz := range cal.ChildrenNamed("VTIMEZONE") {
			if strings.TrimSpace(vtz.Value("TZID")) != tzid {
				continue
			}
			sum := sha256.Sum256(vtz.Bytes())
			key := tzid + "\x00" + string(sum[:])
			if v, hit := vtzCache.Load(key); hit {
				return v.(*time.Location), true
			}
			if loc, err := LocationFromVTimezone(vtz); err == nil {
				cache(&vtzCache, &vtzCount, key, loc)
				return loc, true
			}
		}
	}
	if loc := parseFixedZone(tzid); loc != nil {
		cache(&namedCache, &namedCount, tzid, loc)
		return loc, true
	}
	return time.UTC, false
}

// maxTZID is longer than any real TZID, vendor prefixes and Windows
// display names included.
const maxTZID = 256

// cache stores loc, emptying the cache first when it is full.
func cache(m *sync.Map, n *atomic.Int64, key string, loc *time.Location) {
	if n.Add(1) > maxCachedZones {
		m.Clear()
		n.Store(1)
	}
	m.Store(key, loc)
}

// LoadZone is time.LoadLocation for names that come from users: "Local"
// would be the server's own zone, and "" UTC under another name.
func LoadZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("%w: time zone %q", ErrValue, name)
	}
	return time.LoadLocation(name)
}

func lookupNamed(tzid string) *time.Location {
	if loc := loadIANA(tzid); loc != nil {
		return loc
	}
	// Vendor prefixes: try every suffix of the slash separated path that
	// could be an IANA name (loadIANA takes at most 64 bytes).
	name := strings.Trim(tzid, "/")
	for {
		if len(name) <= 64 {
			if loc := loadIANA(name); loc != nil {
				return loc
			}
		}
		i := strings.IndexByte(name, '/')
		if i < 0 {
			break
		}
		name = name[i+1:]
	}
	if name, ok := windowsZones[tzid]; ok {
		return loadIANA(name)
	}
	if name, ok := windowsDisplayZones[tzid]; ok {
		return loadIANA(name)
	}
	if name, ok := lowerWin()[strings.ToLower(tzid)]; ok {
		return loadIANA(name)
	}
	return nil
}

func loadIANA(name string) *time.Location {
	// "Local" would leak the server's zone; "" is UTC in time.LoadLocation.
	if name == "" || name == "Local" || len(name) > 64 || strings.Contains(name, "..") || strings.ContainsAny(name, "\\\x00") {
		return nil
	}
	if strings.EqualFold(name, "UTC") || strings.EqualFold(name, "GMT") || strings.EqualFold(name, "Z") {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil
	}
	return loc
}

// parseFixedZone understands "UTC+1", "UTC+01:00", "GMT-0500" and the
// "(UTC+01:00) Some City" prefix of unknown Windows display names.
func parseFixedZone(s string) *time.Location {
	if strings.HasPrefix(s, "(") {
		end := strings.IndexByte(s, ')')
		if end < 0 {
			return nil
		}
		s = s[1:end]
	}
	u := strings.ToUpper(s)
	if !strings.HasPrefix(u, "UTC") && !strings.HasPrefix(u, "GMT") {
		return nil
	}
	rest := u[3:]
	if rest == "" {
		return time.UTC
	}
	sign := 1
	switch rest[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return nil
	}
	rest = strings.ReplaceAll(rest[1:], ":", "")
	var h, m int
	var err error
	switch len(rest) {
	case 1, 2:
		h, err = strconv.Atoi(rest)
	case 4:
		h, err = strconv.Atoi(rest[:2])
		if err == nil {
			m, err = strconv.Atoi(rest[2:])
		}
	default:
		return nil
	}
	if err != nil || h > 14 || m > 59 {
		return nil
	}
	off := sign * (h*3600 + m*60)
	if off == 0 {
		return time.UTC
	}
	return time.FixedZone(s, off)
}

// VTIMEZONE conversion bounds: observances are expanded from their DTSTART
// (but not before vtzFirstYear) up to vtzLastYear.
const (
	vtzFirstYear      = 1800
	vtzLastYear       = 2200
	vtzMaxTransitions = 10000
)

type transition struct {
	at     int64 // unix seconds
	offset int
	isDST  bool
	abbr   string
}

// LocationFromVTimezone converts a VTIMEZONE component into a
// *time.Location. The STANDARD/DAYLIGHT observances (DTSTART + RRULE/RDATE)
// are expanded with this package's rule engine up to the year 2200, the
// transitions are encoded as an in-memory TZif blob and loaded with
// time.LoadLocationFromTZData.
func LocationFromVTimezone(vtz *ical.Component) (*time.Location, error) {
	tzid := strings.TrimSpace(vtz.Value("TZID"))
	var trans []transition
	// The transitions of all observances count against one budget: many
	// small observances must not add up to an unbounded slice.
	tooMany := false
	work := newBudget() // shared by the observances' rules
	for _, obs := range vtz.Children {
		if tooMany {
			break
		}
		if obs.Name != "STANDARD" && obs.Name != "DAYLIGHT" {
			continue
		}
		from, err1 := parseOffset(obs.Value("TZOFFSETFROM"))
		to, err2 := parseOffset(obs.Value("TZOFFSETTO"))
		if err2 != nil {
			continue
		}
		if err1 != nil {
			from = to
		}
		abbr := ical.UnescapeText(obs.Value("TZNAME"))
		if abbr == "" || len(abbr) > 32 {
			abbr = formatOffset(to)
		}
		isDST := obs.Name == "DAYLIGHT"
		add := func(local time.Time) {
			// local holds wall-clock fields (in UTC) in the "from" offset.
			if local.Year() < vtzFirstYear || local.Year() > vtzLastYear {
				return
			}
			if len(trans) >= vtzMaxTransitions {
				tooMany = true
				return
			}
			trans = append(trans, transition{at: local.Unix() - int64(from), offset: to, isDST: isDST, abbr: abbr})
		}
		dp := obs.Prop("DTSTART")
		if dp == nil {
			continue
		}
		start, err := parseDT(strings.TrimSpace(dp.Value), "", "", nil, time.UTC)
		if err != nil {
			continue
		}
		local := start.Time // treated as wall clock (in UTC)
		if dp.Value != "" && strings.HasSuffix(strings.ToUpper(dp.Value), "Z") {
			local = local.Add(time.Duration(from) * time.Second)
		}
		if local.Year() < vtzFirstYear {
			// Old DTSTARTs (Outlook uses 1601) would cost centuries of
			// iterations: move them forward keeping month/day/time.
			local = time.Date(vtzFirstYear, local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, time.UTC)
		}
		add(local)
		for _, rp := range obs.PropsNamed("RRULE") {
			r, err := Parse(rp.Value)
			if err != nil {
				continue
			}
			rr := *r
			until, hasUntil := rr.Until, !rr.Until.IsZero()
			untilUTC := rr.UntilUTC
			if rr.UntilIsDate {
				until = until.Add(24*time.Hour - time.Second)
			}
			rr.Until, rr.UntilUTC, rr.UntilIsDate = time.Time{}, false, false
			n := 0
			for t := range rr.iter(local, work) {
				if t.Year() > vtzLastYear || n > vtzMaxTransitions || tooMany {
					break
				}
				if hasUntil {
					inst := t
					if untilUTC {
						inst = t.Add(-time.Duration(from) * time.Second)
					}
					if inst.After(until) {
						break
					}
				}
				add(t)
				n++
			}
		}
		for _, rp := range obs.PropsNamed("RDATE") {
			starts, _, err := ParseDateTimeList(rp, nil, time.UTC)
			if err != nil {
				continue
			}
			for _, s := range starts {
				if tooMany {
					break
				}
				t := s.Time
				if !s.Floating {
					t = t.Add(time.Duration(from) * time.Second)
				}
				add(t)
			}
		}
	}
	if tooMany {
		return nil, fmt.Errorf("%w: VTIMEZONE %q has more than %d transitions", ErrValue, tzid, vtzMaxTransitions)
	}
	if len(trans) == 0 {
		return nil, fmt.Errorf("%w: VTIMEZONE %q has no usable observance", ErrValue, tzid)
	}
	slices.SortStableFunc(trans, func(a, b transition) int { return cmp.Compare(a.at, b.at) })
	// The zone before the first transition: the first observance's
	// TZOFFSETFROM is unknown here, so use the earliest standard offset.
	initial := trans[0]
	for _, t := range trans {
		if !t.isDST {
			initial = t
			break
		}
	}
	initial.at = 0
	name := tzid
	if name == "" {
		name = "VTIMEZONE"
	}
	return time.LoadLocationFromTZData(name, buildTZif(initial, trans))
}

func parseOffset(s string) (int, error) {
	s = strings.TrimSpace(s)
	if len(s) != 5 && len(s) != 7 || (s[0] != '+' && s[0] != '-') {
		return 0, fmt.Errorf("%w: bad UTC offset %q", ErrValue, s)
	}
	h, ok1 := atoi2(s[1:3])
	m, ok2 := atoi2(s[3:5])
	sec, ok3 := 0, true
	if len(s) == 7 {
		sec, ok3 = atoi2(s[5:7])
	}
	if !ok1 || !ok2 || !ok3 || h > 23 || m > 59 || sec > 59 {
		return 0, fmt.Errorf("%w: bad UTC offset %q", ErrValue, s)
	}
	off := h*3600 + m*60 + sec
	if s[0] == '-' {
		off = -off
	}
	return off, nil
}

func formatOffset(off int) string {
	sign := '+'
	if off < 0 {
		sign, off = '-', -off
	}
	return fmt.Sprintf("%c%02d%02d", sign, off/3600, off/60%60)
}

// buildTZif encodes transitions as a version 2 TZif file (RFC 8536) with
// an empty version 1 body and no footer TZ string (times after the last
// transition keep the last offset).
func buildTZif(initial transition, trans []transition) []byte {
	type ttype struct {
		offset int
		isDST  bool
		abbr   string
	}
	types := []ttype{{initial.offset, initial.isDST, initial.abbr}} // type 0: before the first transition
	var chars []byte
	abbrIdx := map[string]int{}
	idxOf := func(a string) int {
		if i, ok := abbrIdx[a]; ok {
			return i
		}
		i := len(chars)
		chars = append(chars, a...)
		chars = append(chars, 0)
		abbrIdx[a] = i
		return i
	}
	typeIdx := map[ttype]int{}
	var times []int64
	var idx []byte
	for _, t := range trans {
		tt := ttype{t.offset, t.isDST, t.abbr}
		i, ok := typeIdx[tt]
		if !ok {
			if len(types) >= 255 {
				continue
			}
			i = len(types)
			types = append(types, tt)
			typeIdx[tt] = i
		}
		if len(times) > 0 && times[len(times)-1] == t.at {
			idx[len(idx)-1] = byte(i) // same instant: last one wins
			continue
		}
		times = append(times, t.at)
		idx = append(idx, byte(i))
	}
	for i := range types {
		// Abbreviation indexes are a single byte: drop names that would
		// not fit.
		if _, ok := abbrIdx[types[i].abbr]; !ok && len(chars)+len(types[i].abbr) >= 255 {
			types[i].abbr = types[0].abbr
		}
		idxOf(types[i].abbr)
	}

	var b bytes.Buffer
	be := func(v any) { binary.Write(&b, binary.BigEndian, v) }
	header := func(timecnt, typecnt, charcnt int) {
		b.WriteString("TZif2")
		b.Write(make([]byte, 15))
		be([6]uint32{0, 0, 0, uint32(timecnt), uint32(typecnt), uint32(charcnt)})
	}
	writeTypes := func() {
		for _, t := range types {
			be(int32(t.offset))
			if t.isDST {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
			b.WriteByte(byte(abbrIdx[t.abbr]))
		}
		b.Write(chars)
	}
	// Version 1 body: no transitions, the types only.
	header(0, len(types), len(chars))
	writeTypes()
	// Version 2 body with 64-bit transition times.
	header(len(times), len(types), len(chars))
	for _, t := range times {
		be(t)
	}
	b.Write(idx)
	writeTypes()
	b.WriteString("\n\n")
	return b.Bytes()
}
