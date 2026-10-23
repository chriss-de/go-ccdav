package server

import (
	"encoding/xml"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"davd/internal/caldav"
	"davd/internal/dav"
	"davd/internal/store"
)

var (
	pResourceType          = dav.N(dav.NSDAV, "resourcetype")
	pDisplayName           = dav.N(dav.NSDAV, "displayname")
	pCurrentUserPrincipal  = dav.N(dav.NSDAV, "current-user-principal")
	pPrincipalURL          = dav.N(dav.NSDAV, "principal-URL")
	pPrincipalCollSet      = dav.N(dav.NSDAV, "principal-collection-set")
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
	pMaxResourceSize       = dav.N(dav.NSCalDAV, "max-resource-size")
	pCalendarData          = dav.N(dav.NSCalDAV, "calendar-data")
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
			pCalendarUserAddrSet, pCalendarUserType, pEmailAddressSet)
	case kCalHome:
		return append(common, pOwner, pDisplayName)
	case kCalendar:
		names := append(common, pOwner, pDisplayName, pCalendarDescription, pCalendarColor,
			pCalendarOrder, pSupportedCompSet, pSupportedCalData, pMaxResourceSize, pGetCTag)
		if res.col != nil {
			m := res.col.Meta()
			if m.Timezone != "" {
				names = append(names, pCalendarTimezone)
			}
			for k := range m.DeadProps {
				names = append(names, dav.ParseKey(k))
			}
		}
		return names
	case kCalObject:
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
// req is the requesting element (for calendar-data with sub-elements).
func (s *Server) propValue(user string, res *resource, name xml.Name, req *dav.Element) (string, int) {
	cfg := s.cfg.Get()
	switch name {
	case pCurrentUserPrincipal:
		return hrefs(principalHref(user)), http.StatusOK
	case pResourceType:
		switch res.kind {
		case kCalObject:
			return "", http.StatusOK
		case kPrincipal:
			return resourceTypeCollection + dav.E(dav.N(dav.NSDAV, "principal"), ""), http.StatusOK
		case kCalendar:
			return resourceTypeCollection + dav.E(dav.N(dav.NSCalDAV, "calendar"), ""), http.StatusOK
		}
		return resourceTypeCollection, http.StatusOK
	case pCurrentUserPrivSet:
		if res.kind == kPrincipal && res.owner != user {
			return privileges("read"), http.StatusOK
		}
		return privileges("all", "read", "write", "write-properties", "write-content",
			"read-current-user-privilege-set", "bind", "unbind"), http.StatusOK
	case pSupportedReportSet:
		if res.kind == kCalendar || res.kind == kCalObject {
			return reports(dav.N(dav.NSCalDAV, "calendar-multiget"), dav.N(dav.NSCalDAV, "calendar-query"),
				dav.N(dav.NSDAV, "sync-collection")), http.StatusOK
		}
		return "", http.StatusOK
	case pOwner:
		if res.kind == kCalHome || res.kind == kCalendar || res.kind == kCalObject {
			return hrefs(principalHref(res.owner)), http.StatusOK
		}
	}

	switch res.kind {
	case kPrincipal:
		u := cfg.Users[res.owner]
		switch name {
		case pDisplayName:
			dn := u.Name
			if dn == "" {
				dn = res.owner
			}
			return dav.Esc(dn), http.StatusOK
		case pPrincipalURL:
			return hrefs(principalHref(res.owner)), http.StatusOK
		case pPrincipalCollSet:
			return hrefs("/dav/principals/"), http.StatusOK
		case pCalendarHomeSet:
			return hrefs(calHomeHref(res.owner)), http.StatusOK
		case pCalendarUserAddrSet:
			v := ""
			if u.Email != "" {
				v = dav.Href("mailto:" + u.Email)
			}
			return v + hrefs(principalHref(res.owner)), http.StatusOK
		case pCalendarUserType:
			return "INDIVIDUAL", http.StatusOK
		case pEmailAddressSet:
			if u.Email == "" {
				return "", http.StatusNotFound
			}
			return dav.T(dav.N(dav.NSCS, "email-address"), u.Email), http.StatusOK
		}
	case kCalHome:
		if name == pDisplayName {
			return dav.Esc(res.owner), http.StatusOK
		}
	case kCalendar:
		if res.col == nil {
			return "", http.StatusNotFound
		}
		m := res.col.Meta()
		switch name {
		case pDisplayName:
			dn := m.DisplayName
			if dn == "" {
				dn = res.colName
			}
			return dav.Esc(dn), http.StatusOK
		case pCalendarDescription:
			return dav.Esc(m.Description), http.StatusOK
		case pCalendarColor:
			if m.Color == "" {
				return "", http.StatusNotFound
			}
			return dav.Esc(m.Color), http.StatusOK
		case pCalendarOrder:
			if m.Order == "" {
				return "", http.StatusNotFound
			}
			return dav.Esc(m.Order), http.StatusOK
		case pCalendarTimezone:
			if m.Timezone == "" {
				return "", http.StatusNotFound
			}
			return dav.Esc(m.Timezone), http.StatusOK
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
		case pMaxResourceSize:
			return strconv.Itoa(maxResourceSize), http.StatusOK
		case pGetCTag:
			return dav.Esc(strconv.FormatUint(res.col.SyncToken(), 10)), http.StatusOK
		case pSyncToken:
			// Live but not part of allprop (RFC 6578 §4).
			return dav.Esc(formatSyncToken(res.col.SyncID(), res.col.SyncToken())), http.StatusOK
		}
		if v, ok := m.DeadProps[dav.Key(name)]; ok {
			return v, http.StatusOK
		}
	case kCalObject:
		if !res.exists {
			return "", http.StatusNotFound
		}
		switch name {
		case pGetETag:
			return dav.Esc(res.info.ETag), http.StatusOK
		case pGetContentType:
			return "text/calendar; charset=utf-8", http.StatusOK
		case pGetContentLength:
			return strconv.FormatInt(res.info.Size, 10), http.StatusOK
		case pGetLastModified:
			return res.info.ModTime.Format(http.TimeFormat), http.StatusOK
		case pCalendarData:
			data, _, err := res.col.Get(res.name)
			if err != nil {
				return "", http.StatusNotFound
			}
			out, err := caldav.RenderData(data, req, caldav.FloatingZone(res.col.Meta().Timezone))
			if err != nil {
				return "", http.StatusInternalServerError
			}
			return dav.Esc(out), http.StatusOK
		}
	}
	return "", http.StatusNotFound
}

// responseFor builds a DAV:response for the requested properties. With
// names == nil (allprop) the live properties are returned; propNamesOnly
// returns empty elements (propname).
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
	pSupportedCalData, pMaxResourceSize, pGetCTag, pCalendarData, pPrincipalURL,
	pCalendarHomeSet, pCalendarUserAddrSet,
}

// applyProp sets (value != nil) or removes one collection property on m.
// It returns an HTTP status for the propstat.
func applyProp(m *store.Meta, name xml.Name, el *dav.Element, remove bool) int {
	if slices.Contains(protectedProps, name) {
		return http.StatusForbidden
	}
	text := ""
	if el != nil {
		text = el.Text
	}
	switch name {
	case pDisplayName:
		m.DisplayName = text
	case pCalendarDescription:
		m.Description = text
	case pCalendarColor:
		m.Color = strings.TrimSpace(text)
	case pCalendarOrder:
		m.Order = strings.TrimSpace(text)
	case pCalendarTimezone:
		if !remove {
			if err := caldav.ValidTimezone(text); err != nil {
				return http.StatusForbidden
			}
		}
		m.Timezone = text
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
