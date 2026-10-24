package server

import (
	"encoding/xml"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/caldav"
	"github.com/chriss-de/go-ccdav/internal/carddav"
	"github.com/chriss-de/go-ccdav/internal/dav"
	"github.com/chriss-de/go-ccdav/internal/store"
)

var (
	pResourceType          = dav.N(dav.NSDAV, "resourcetype")
	pDisplayName           = dav.N(dav.NSDAV, "displayname")
	pCurrentUserPrincipal  = dav.N(dav.NSDAV, "current-user-principal")
	pPrincipalURL          = dav.N(dav.NSDAV, "principal-URL")
	pPrincipalCollSet      = dav.N(dav.NSDAV, "principal-collection-set")
	pGroupMemberSet        = dav.N(dav.NSDAV, "group-member-set")
	pGroupMembership       = dav.N(dav.NSDAV, "group-membership")
	pOwner                 = dav.N(dav.NSDAV, "owner")
	pCurrentUserPrivSet    = dav.N(dav.NSDAV, "current-user-privilege-set")
	pSupportedReportSet    = dav.N(dav.NSDAV, "supported-report-set")
	pGetETag               = dav.N(dav.NSDAV, "getetag")
	pSyncToken             = dav.N(dav.NSDAV, "sync-token")
	pGetContentType        = dav.N(dav.NSDAV, "getcontenttype")
	pGetContentLength      = dav.N(dav.NSDAV, "getcontentlength")
	pGetLastModified       = dav.N(dav.NSDAV, "getlastmodified")
	pCalendarHomeSet       = dav.N(dav.NSCalDAV, "calendar-home-set")
	pCalendarUserAddrSet   = dav.N(dav.NSCalDAV, "calendar-user-address-set")
	pCalendarUserType      = dav.N(dav.NSCalDAV, "calendar-user-type")
	pCalendarDescription   = dav.N(dav.NSCalDAV, "calendar-description")
	pCalendarTimezone      = dav.N(dav.NSCalDAV, "calendar-timezone")
	pSupportedCompSet      = dav.N(dav.NSCalDAV, "supported-calendar-component-set")
	pSupportedCalData      = dav.N(dav.NSCalDAV, "supported-calendar-data")
	pCalMaxResourceSize    = dav.N(dav.NSCalDAV, "max-resource-size")
	pCalendarData          = dav.N(dav.NSCalDAV, "calendar-data")
	pAddressbookHomeSet    = dav.N(dav.NSCardDAV, "addressbook-home-set")
	pAddressbookDesc       = dav.N(dav.NSCardDAV, "addressbook-description")
	pSupportedAddrData     = dav.N(dav.NSCardDAV, "supported-address-data")
	pCardMaxResourceSize   = dav.N(dav.NSCardDAV, "max-resource-size")
	pAddressData           = dav.N(dav.NSCardDAV, "address-data")
	pGetCTag               = dav.N(dav.NSCS, "getctag")
	pEmailAddressSet       = dav.N(dav.NSCS, "email-address-set")
	pCalendarColor         = dav.N(dav.NSApple, "calendar-color")
	pCalendarOrder         = dav.N(dav.NSApple, "calendar-order")
	resourceTypeCollection = dav.E(dav.N(dav.NSDAV, "collection"), "")
)

// allProps lists the live properties returned for allprop / propname.
func (s *Server) allProps(res *resource) []xml.Name {
	common := []xml.Name{pResourceType, pCurrentUserPrincipal, pCurrentUserPrivSet, pSupportedReportSet}
	switch res.kind {
	case kPrincipal:
		return append(common, pDisplayName, pPrincipalURL, pPrincipalCollSet, pCalendarHomeSet,
			pAddressbookHomeSet, pCalendarUserAddrSet, pCalendarUserType, pEmailAddressSet, pGroupMembership)
	case kGroupPrincipal:
		return append(common, pDisplayName, pPrincipalURL, pPrincipalCollSet, pCalendarUserType, pGroupMemberSet)
	case kHome:
		return append(common, pOwner, pDisplayName)
	case kCollection:
		names := append(common, pOwner, pDisplayName, pGetCTag)
		if res.isCalendar() {
			names = append(names, pCalendarDescription, pCalendarColor, pCalendarOrder,
				pSupportedCompSet, pSupportedCalData, pCalMaxResourceSize)
		} else {
			names = append(names, pAddressbookDesc, pSupportedAddrData, pCardMaxResourceSize)
		}
		if res.col != nil {
			m := res.col.Meta()
			if m.Timezone != "" && res.isCalendar() {
				names = append(names, pCalendarTimezone)
			}
			for k := range m.DeadProps {
				names = append(names, dav.ParseKey(k))
			}
		}
		return names
	case kObject:
		return []xml.Name{pResourceType, pGetETag, pGetContentType, pGetContentLength, pGetLastModified}
	}
	return common
}

func hrefs(paths ...string) string {
	var sb strings.Builder
	for _, p := range paths {
		sb.WriteString(dav.Href(p))
	}
	return sb.String()
}

func privileges(names ...string) string {
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString(dav.E(dav.N(dav.NSDAV, "privilege"), dav.E(dav.N(dav.NSDAV, n), "")))
	}
	return sb.String()
}

func reports(names ...xml.Name) string {
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString(dav.E(dav.N(dav.NSDAV, "supported-report"), dav.E(dav.N(dav.NSDAV, "report"), dav.E(n, ""))))
	}
	return sb.String()
}

// propValue returns the raw XML value of one property and a status.
// req is the requesting element (for calendar-data / address-data with
// sub-elements).
func (s *Server) propValue(user string, res *resource, name xml.Name, req *dav.Element) (string, int) {
	switch name {
	case pCurrentUserPrincipal:
		return hrefs(principalHref(res.base, user)), http.StatusOK
	case pResourceType:
		switch res.kind {
		case kObject:
			return "", http.StatusOK
		case kPrincipal, kGroupPrincipal:
			return resourceTypeCollection + dav.E(dav.N(dav.NSDAV, "principal"), ""), http.StatusOK
		case kCollection:
			if res.isCalendar() {
				return resourceTypeCollection + dav.E(dav.N(dav.NSCalDAV, "calendar"), ""), http.StatusOK
			}
			return resourceTypeCollection + dav.E(dav.N(dav.NSCardDAV, "addressbook"), ""), http.StatusOK
		}
		return resourceTypeCollection, http.StatusOK
	case pCurrentUserPrivSet:
		return privileges(privilegesFor(user, res)...), http.StatusOK
	case pSupportedReportSet:
		switch {
		case res.kind != kCollection && res.kind != kObject:
			return "", http.StatusOK
		case res.isCalendar():
			return reports(dav.N(dav.NSCalDAV, "calendar-multiget"), dav.N(dav.NSCalDAV, "calendar-query"),
				dav.N(dav.NSCalDAV, "free-busy-query"), dav.N(dav.NSDAV, "sync-collection")), http.StatusOK
		default:
			return reports(dav.N(dav.NSCardDAV, "addressbook-multiget"), dav.N(dav.NSCardDAV, "addressbook-query"),
				dav.N(dav.NSDAV, "sync-collection")), http.StatusOK
		}
	case pOwner:
		switch {
		case res.col != nil:
			return hrefs(principalHref(res.base, res.col.Owner)), http.StatusOK
		case res.kind == kHome || res.kind == kCollection:
			return hrefs(principalHref(res.base, res.owner)), http.StatusOK
		}
	}

	switch res.kind {
	case kPrincipal:
		return s.principalProp(res, name)
	case kGroupPrincipal:
		return s.groupProp(res, name)
	case kHome:
		if name == pDisplayName {
			return dav.Esc(res.owner), http.StatusOK
		}
	case kCollection:
		if res.col != nil {
			return s.collectionProp(res, name)
		}
	case kObject:
		if res.exists {
			return objectProp(res, name, req)
		}
	}
	return "", http.StatusNotFound
}

// privilegesFor lists the DAV privileges user has on res.
func privilegesFor(user string, res *resource) []string {
	acc := res.acc
	switch res.kind {
	case kPrincipal, kGroupPrincipal, kPrincipals, kGroups, kRoot:
		if res.kind == kPrincipal && res.owner == user {
			acc = accOwner
		} else {
			acc = accRead
		}
	case kHomes, kHome:
		acc = accOwner
	}
	switch acc {
	case accOwner:
		return []string{"all", "read", "write", "write-properties", "write-content",
			"read-current-user-privilege-set", "bind", "unbind"}
	case accWrite:
		// write-properties only touches the sharee's own view (name, color).
		return []string{"read", "read-current-user-privilege-set", "write", "write-properties",
			"write-content", "bind", "unbind"}
	default:
		return []string{"read", "read-current-user-privilege-set", "write-properties"}
	}
}

func (s *Server) principalProp(res *resource, name xml.Name) (string, int) {
	cfg := s.cfg.Get()
	u := cfg.Users[res.owner]
	switch name {
	case pDisplayName:
		dn := u.Name
		if dn == "" {
			dn = res.owner
		}
		return dav.Esc(dn), http.StatusOK
	case pPrincipalURL:
		return hrefs(principalHref(res.base, res.owner)), http.StatusOK
	case pPrincipalCollSet:
		return hrefs(res.base + "principals/"), http.StatusOK
	case pCalendarHomeSet:
		return hrefs(homeHref(res.base, store.Calendars, res.owner)), http.StatusOK
	case pAddressbookHomeSet:
		return hrefs(homeHref(res.base, store.AddressBooks, res.owner)), http.StatusOK
	case pGroupMembership:
		var out []string
		for _, g := range cfg.GroupsOf(res.owner) {
			out = append(out, groupHref(res.base, g))
		}
		return hrefs(out...), http.StatusOK
	case pCalendarUserAddrSet:
		v := ""
		if u.Email != "" {
			v = dav.Href("mailto:" + u.Email)
		}
		return v + hrefs(principalHref(res.base, res.owner)), http.StatusOK
	case pCalendarUserType:
		return "INDIVIDUAL", http.StatusOK
	case pEmailAddressSet:
		if u.Email == "" {
			return "", http.StatusNotFound
		}
		return dav.T(dav.N(dav.NSCS, "email-address"), u.Email), http.StatusOK
	}
	return "", http.StatusNotFound
}

func (s *Server) groupProp(res *resource, name xml.Name) (string, int) {
	switch name {
	case pDisplayName:
		return dav.Esc(res.owner), http.StatusOK
	case pPrincipalURL:
		return hrefs(groupHref(res.base, res.owner)), http.StatusOK
	case pPrincipalCollSet:
		return hrefs(res.base + "principals/"), http.StatusOK
	case pCalendarUserType:
		return "GROUP", http.StatusOK
	case pGroupMemberSet:
		var out []string
		for _, m := range s.cfg.Get().Groups[res.owner] {
			out = append(out, principalHref(res.base, m))
		}
		return hrefs(out...), http.StatusOK
	}
	return "", http.StatusNotFound
}

// ctag and syncToken of a collection as seen by res.owner: for a shared
// collection the sharee's preference version is appended, so their own
// changes (color, name) are noticed too.
func (s *Server) ctag(res *resource) string {
	t := strconv.FormatUint(res.col.SyncToken(), 10)
	if res.isShared() {
		t += "." + strconv.FormatUint(s.store.SharePrefs(res.owner, res.col.Key()).Version, 10)
	}
	return t
}

func (s *Server) syncToken(res *resource, n uint64) string {
	t := formatSyncToken(res.col.SyncID(), n)
	if res.isShared() {
		t += "." + strconv.FormatUint(s.store.SharePrefs(res.owner, res.col.Key()).Version, 10)
	}
	return t
}

func optional(v string) (string, int) {
	if v == "" {
		return "", http.StatusNotFound
	}
	return dav.Esc(v), http.StatusOK
}

func (s *Server) collectionProp(res *resource, name xml.Name) (string, int) {
	m := res.col.Meta()
	if res.isShared() {
		// The sharee's own name, color and order override the owner's.
		p := s.store.SharePrefs(res.owner, res.col.Key())
		dn := m.DisplayName
		if dn == "" {
			dn = res.col.Name
		}
		ownerName := s.cfg.Get().Users[res.col.Owner].Name
		if ownerName == "" {
			ownerName = res.col.Owner
		}
		m.DisplayName = dn + " (" + ownerName + ")"
		if p.DisplayName != "" {
			m.DisplayName = p.DisplayName
		}
		if p.Color != "" {
			m.Color = p.Color
		}
		if p.Order != "" {
			m.Order = p.Order
		}
	}
	switch name {
	case pDisplayName:
		dn := m.DisplayName
		if dn == "" {
			dn = res.colName
		}
		return dav.Esc(dn), http.StatusOK
	case pGetCTag:
		return dav.Esc(s.ctag(res)), http.StatusOK
	case pSyncToken:
		// Live but not part of allprop (RFC 6578 §4).
		return dav.Esc(s.syncToken(res, res.col.SyncToken())), http.StatusOK
	}
	if res.isCalendar() {
		switch name {
		case pCalendarDescription:
			return dav.Esc(m.Description), http.StatusOK
		case pCalendarColor:
			return optional(m.Color)
		case pCalendarOrder:
			return optional(m.Order)
		case pCalendarTimezone:
			return optional(m.Timezone)
		case pSupportedCompSet:
			comps := m.Components
			if len(comps) == 0 {
				comps = caldav.CalendarComponents
			}
			var sb strings.Builder
			for _, c := range comps {
				sb.WriteString(`<cal:comp name="` + dav.Esc(c) + `"/>`)
			}
			return sb.String(), http.StatusOK
		case pSupportedCalData:
			return `<cal:calendar-data content-type="text/calendar" version="2.0"/>`, http.StatusOK
		case pCalMaxResourceSize:
			return strconv.Itoa(maxResourceSize), http.StatusOK
		}
	} else {
		switch name {
		case pAddressbookDesc:
			return dav.Esc(m.Description), http.StatusOK
		case pSupportedAddrData:
			return `<card:address-data-type content-type="text/vcard" version="3.0"/>` +
				`<card:address-data-type content-type="text/vcard" version="4.0"/>`, http.StatusOK
		case pCardMaxResourceSize:
			return strconv.Itoa(maxResourceSize), http.StatusOK
		}
	}
	if v, ok := m.DeadProps[dav.Key(name)]; ok {
		return v, http.StatusOK
	}
	return "", http.StatusNotFound
}

func objectProp(res *resource, name xml.Name, req *dav.Element) (string, int) {
	switch name {
	case pGetETag:
		return dav.Esc(res.info.ETag), http.StatusOK
	case pGetContentType:
		return kinds[res.sk].contentType, http.StatusOK
	case pGetContentLength:
		return strconv.FormatInt(res.info.Size, 10), http.StatusOK
	case pGetLastModified:
		return res.info.ModTime.Format(http.TimeFormat), http.StatusOK
	case pCalendarData, pAddressData:
		if (name == pCalendarData) != res.isCalendar() {
			return "", http.StatusNotFound
		}
		data, _, err := res.col.Get(res.name)
		if err != nil {
			return "", http.StatusNotFound
		}
		var out string
		if res.isCalendar() {
			out, err = caldav.RenderData(data, req, caldav.FloatingZone(res.col.Meta().Timezone))
		} else {
			out, err = carddav.RenderData(data, req)
		}
		if err != nil {
			return "", http.StatusInternalServerError
		}
		return dav.Esc(out), http.StatusOK
	}
	return "", http.StatusNotFound
}

// responseFor builds a DAV:response for the requested properties. With
// reqProps == nil (allprop) the live properties are returned;
// propNamesOnly returns empty elements (propname).
func (s *Server) responseFor(user string, res *resource, reqProps []*dav.Element, propNamesOnly bool) *dav.Response {
	resp := &dav.Response{Href: res.href()}
	if reqProps == nil {
		for _, n := range s.allProps(res) {
			if propNamesOnly {
				resp.AddProp(http.StatusOK, dav.Prop{Name: n})
				continue
			}
			v, st := s.propValue(user, res, n, nil)
			if st == http.StatusOK {
				resp.AddProp(st, dav.Prop{Name: n, Value: v})
			}
		}
		return resp
	}
	for _, el := range reqProps {
		v, st := s.propValue(user, res, el.Name, el)
		if st != http.StatusOK {
			v = ""
		}
		resp.AddProp(st, dav.Prop{Name: el.Name, Value: v})
	}
	return resp
}

// protectedProps cannot be changed by PROPPATCH.
var protectedProps = []xml.Name{
	pResourceType, pCurrentUserPrincipal, pCurrentUserPrivSet, pSupportedReportSet, pOwner,
	pGetETag, pSyncToken, pGetContentType, pGetContentLength, pGetLastModified, pSupportedCompSet,
	pSupportedCalData, pCalMaxResourceSize, pGetCTag, pCalendarData, pPrincipalURL,
	pCalendarHomeSet, pCalendarUserAddrSet, pAddressbookHomeSet, pSupportedAddrData,
	pCardMaxResourceSize, pAddressData, pGroupMemberSet, pGroupMembership,
}

// applyProp sets or removes one collection property on m. It returns an
// HTTP status for the propstat.
func applyProp(m *store.Meta, sk store.Kind, name xml.Name, el *dav.Element, remove bool) int {
	if slices.Contains(protectedProps, name) {
		return http.StatusForbidden
	}
	text := ""
	if el != nil {
		text = el.Text
	}
	cal := sk == store.Calendars
	switch {
	case name == pDisplayName:
		m.DisplayName = text
	case name == pCalendarDescription && cal, name == pAddressbookDesc && !cal:
		m.Description = text
	case name == pCalendarColor && cal:
		m.Color = strings.TrimSpace(text)
	case name == pCalendarOrder && cal:
		m.Order = strings.TrimSpace(text)
	case name == pCalendarTimezone && cal:
		if !remove {
			if err := caldav.ValidTimezone(text); err != nil {
				return http.StatusForbidden
			}
		}
		m.Timezone = text
	case name.Space == dav.NSCalDAV && !cal, name.Space == dav.NSCardDAV && cal:
		return http.StatusForbidden // the other protocol's properties
	default:
		if remove {
			delete(m.DeadProps, dav.Key(name))
			return http.StatusOK
		}
		if len(el.Inner) > 4096 {
			return http.StatusInsufficientStorage
		}
		if m.DeadProps == nil {
			m.DeadProps = map[string]string{}
		}
		m.DeadProps[dav.Key(name)] = el.Inner
	}
	return http.StatusOK
}
