package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/store"
)

// JMAP Sharing (RFC 9670).
const (
	CapPrincipals      = "urn:ietf:params:jmap:principals"
	CapPrincipalsOwner = "urn:ietf:params:jmap:principals:owner"
)

// Principals are the users and groups of config.yaml, visible to every
// user as they are via DAV. A user's principal id is their account id;
// a group's is "g" + hex(name).

// PrincipalID is the principal id of a user.
func PrincipalID(user string) string { return AccountID(user) }

// GroupPrincipalID is the principal id of a config.yaml group.
func GroupPrincipalID(group string) string { return "g" + hex.EncodeToString([]byte(group)) }

// principalOf decodes a principal id into a user or a group of cfg.
func principalOf(cfg *config.Config, id string) (user, group string, ok bool) {
	if u, ok := ownerOf(id); ok {
		_, exists := cfg.Users[u]
		return u, "", exists
	}
	if h, ok := strings.CutPrefix(id, "g"); ok {
		if b, err := hex.DecodeString(h); err == nil && len(b) > 0 {
			_, exists := cfg.Groups[string(b)]
			return "", string(b), exists
		}
	}
	return "", "", false
}

func (h *Handler) registerPrincipals() {
	// Principals live in each user's own account.
	h.AddCapability(CapPrincipals, struct{}{}, func(a account) any {
		if !a.personal {
			return nil
		}
		return map[string]any{"currentUserPrincipalId": PrincipalID(a.viewer)}
	})
	h.AddCapability(CapPrincipalsOwner, struct{}{}, func(a account) any {
		return map[string]any{"accountIdForPrincipal": AccountID(a.viewer), "principalId": PrincipalID(a.owner)}
	})
	h.Register("Principal/get", CapPrincipals, func(c *Call, args json.RawMessage) (any, error) { return c.principalGet(args) })
	h.Register("Principal/changes", CapPrincipals, func(c *Call, args json.RawMessage) (any, error) { return c.principalChanges(args) })
	h.Register("Principal/query", CapPrincipals, func(c *Call, args json.RawMessage) (any, error) { return c.principalQuery(args) })
	h.Register("Principal/queryChanges", CapPrincipals, func(*Call, json.RawMessage) (any, error) {
		return nil, Errorf(ErrCannotCalculateChanges, "query changes are not supported; query again")
	})
}

// principalState changes with the users and groups of the config.
func principalState(cfg *config.Config) string {
	sum := sha256.New()
	for _, u := range slices.Sorted(maps.Keys(cfg.Users)) {
		sum.Write([]byte("u\x00" + u + "\x00" + cfg.Users[u].Name + "\x00" + cfg.Users[u].Email + "\n"))
	}
	for _, g := range slices.Sorted(maps.Keys(cfg.Groups)) {
		sum.Write([]byte("g\x00" + g + "\x00" + strings.Join(cfg.Groups[g], ",") + "\n"))
	}
	return hex.EncodeToString(sum.Sum(nil)[:8])
}

// principalsView checks that accountId is the caller's own account,
// where principals live.
func (c *Call) principalsView(accountID string) error {
	a, ok := c.h.account(c.User, accountID)
	if !ok || !a.personal {
		return Errorf(ErrAccountNotFound, "principals are in your own account")
	}
	return nil
}

// principals returns all Principal objects as the caller sees them,
// sorted by id.
func (c *Call) principals() []map[string]any {
	cfg := c.h.cfg.Get()
	visible := c.h.sessionAccountsOf(c.User)
	var out []map[string]any
	for _, u := range slices.Sorted(maps.Keys(cfg.Users)) {
		usr := cfg.Users[u]
		name := usr.Name
		if name == "" {
			name = u
		}
		var email any
		if usr.Email != "" {
			email = usr.Email
		}
		var accts any
		if a, ok := visible[AccountID(u)]; ok {
			accts = map[string]any{AccountID(u): a.AccountCapabilities}
		}
		out = append(out, map[string]any{
			"id": PrincipalID(u), "type": "individual", "name": name, "description": nil,
			"email": email, "timeZone": nil, "capabilities": map[string]any{}, "accounts": accts,
		})
	}
	for _, g := range slices.Sorted(maps.Keys(cfg.Groups)) {
		out = append(out, map[string]any{
			"id": GroupPrincipalID(g), "type": "group", "name": g, "description": nil,
			"email": nil, "timeZone": nil, "capabilities": map[string]any{}, "accounts": nil,
		})
	}
	return out
}

// sessionAccountsOf is the accounts part of user's session, by id.
func (h *Handler) sessionAccountsOf(user string) map[string]sessionAcct {
	accts, _ := h.sessionAccounts(user)
	return accts
}

func (c *Call) principalGet(raw json.RawMessage) (any, error) {
	var args GetArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := CheckGet(args); err != nil {
		return nil, err
	}
	if err := c.principalsView(args.AccountID); err != nil {
		return nil, err
	}
	resp := &GetResponse{AccountID: args.AccountID, State: principalState(c.h.cfg.Get()), List: []any{}, NotFound: []string{}}
	all := c.principals()
	byID := map[string]map[string]any{}
	for _, p := range all {
		byID[p["id"].(string)] = p
	}
	ids := make([]string, 0, len(all))
	for _, p := range all {
		ids = append(ids, p["id"].(string))
	}
	if args.IDs != nil {
		ids = *args.IDs
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if p, ok := byID[id]; ok {
			resp.List = append(resp.List, Select(p, args.Properties))
		} else {
			resp.NotFound = append(resp.NotFound, id)
		}
	}
	return resp, nil
}

// principalChanges can only tell "nothing changed": the config has no
// history. Anything else is a resync.
func (c *Call) principalChanges(raw json.RawMessage) (any, error) {
	var args ChangesArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := c.principalsView(args.AccountID); err != nil {
		return nil, err
	}
	state := principalState(c.h.cfg.Get())
	if args.SinceState != state {
		return nil, Errorf(ErrCannotCalculateChanges, "the users or groups changed")
	}
	return &ChangesResponse{AccountID: args.AccountID, OldState: state, NewState: state,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}, nil
}

var principalConds = map[string]bool{"accountIds": true, "email": true, "name": true, "text": true, "type": true, "timeZone": true}

func (c *Call) principalQuery(raw json.RawMessage) (any, error) {
	var args QueryArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := c.principalsView(args.AccountID); err != nil {
		return nil, err
	}
	f, err := parseFilter(args.Filter, principalConds)
	if err != nil {
		return nil, err
	}
	if _, err := parseSort(args.Sort, map[string]bool{}); err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range c.principals() {
		ok, err := f.eval(func(cond map[string]json.RawMessage) (bool, error) { return matchPrincipal(p, cond) })
		if err != nil {
			return nil, err
		}
		if ok {
			ids = append(ids, p["id"].(string))
		}
	}
	resp, err := Page(ids, args, maxQueryLimit)
	if err != nil {
		return nil, err
	}
	resp.QueryState = principalState(c.h.cfg.Get())
	return resp, nil
}

func matchPrincipal(p map[string]any, cond map[string]json.RawMessage) (bool, error) {
	for k, raw := range cond {
		if k == "accountIds" {
			var want []string
			if err := json.Unmarshal(raw, &want); err != nil {
				return false, Errorf(ErrInvalidArguments, "filter accountIds: want a list")
			}
			accts, _ := p["accounts"].(map[string]any)
			if !slices.ContainsFunc(want, func(id string) bool { _, ok := accts[id]; return ok }) {
				return false, nil
			}
			continue
		}
		var want string
		if err := json.Unmarshal(raw, &want); err != nil {
			return false, Errorf(ErrInvalidArguments, "filter %s: want a string", k)
		}
		var ok bool
		switch k {
		case "type":
			ok = p["type"] == want
		case "timeZone":
			ok = p["timeZone"] == want
		case "email", "name", "text":
			var fields []string
			for _, f := range []string{"name", "email", "description"} {
				if k == "text" || k == f {
					if s, isStr := p[f].(string); isStr {
						fields = append(fields, s)
					}
				}
			}
			ok = anyContains(fields, want)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// shareWith is the owner's view of a collection's shares, by principal
// id (RFC 9670 rights; nobody else may share or delete).
func shareWith(col *store.Collection) map[string]any {
	out := map[string]any{}
	for _, sh := range col.Shares() {
		id := PrincipalID(sh.User)
		if sh.Group != "" {
			id = GroupPrincipalID(sh.Group)
		}
		out[id] = map[string]any{"mayRead": true, "mayWrite": sh.Access == store.AccessWrite, "mayShare": false, "mayDelete": false}
	}
	return out
}

// parseShareWith turns a shareWith value into shares. Entries without
// any right are dropped; mayShare and mayDelete can't be granted.
func parseShareWith(cfg *config.Config, owner string, val any) ([]store.Share, error) {
	if val == nil {
		return nil, nil
	}
	m, ok := val.(map[string]any)
	if !ok {
		return nil, failSet(SetInvalidProperties, "shareWith must be an object", "shareWith")
	}
	shares := []store.Share{}
	for _, id := range slices.Sorted(maps.Keys(m)) {
		path := "shareWith/" + id
		rights, ok := m[id].(map[string]any)
		if !ok {
			return nil, failSet(SetInvalidProperties, "rights must be an object", path)
		}
		user, group, ok := principalOf(cfg, id)
		if !ok || user == owner {
			return nil, failSet(SetInvalidProperties, "unknown principal "+id, path)
		}
		get := func(k string) (bool, error) {
			v, present := rights[k]
			b, isBool := v.(bool)
			if present && !isBool {
				return false, failSet(SetInvalidProperties, k+" must be a boolean", path+"/"+k)
			}
			return b, nil
		}
		var r [4]bool
		for i, k := range []string{"mayRead", "mayWrite", "mayShare", "mayDelete"} {
			b, err := get(k)
			if err != nil {
				return nil, err
			}
			r[i] = b
		}
		switch {
		case r[2] || r[3]:
			return nil, failSet(SetInvalidProperties, "only the owner may share or delete", path)
		case r[1]:
			shares = append(shares, store.Share{User: user, Group: group, Access: store.AccessWrite})
		case r[0]:
			shares = append(shares, store.Share{User: user, Group: group, Access: store.AccessRead})
		}
	}
	return shares, nil
}
