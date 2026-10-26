package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
)

// session is the JMAP session object (RFC 8620 §2).
type session struct {
	Capabilities    map[string]any         `json:"capabilities"`
	Accounts        map[string]sessionAcct `json:"accounts"`
	PrimaryAccounts map[string]string      `json:"primaryAccounts"`
	Username        string                 `json:"username"`
	APIURL          string                 `json:"apiUrl"`
	DownloadURL     string                 `json:"downloadUrl"`
	UploadURL       string                 `json:"uploadUrl"`
	EventSourceURL  string                 `json:"eventSourceUrl"`
	State           string                 `json:"state"`
}

type sessionAcct struct {
	Name                string         `json:"name"`
	IsPersonal          bool           `json:"isPersonal"`
	IsReadOnly          bool           `json:"isReadOnly"`
	AccountCapabilities map[string]any `json:"accountCapabilities"`
}

// sessionAccounts builds the accounts and primary accounts of user.
// Every capability except core applies to every account for now.
func (h *Handler) sessionAccounts(user string) (map[string]sessionAcct, map[string]string) {
	accts := map[string]sessionAcct{}
	for id, a := range h.accounts(user) {
		caps := map[string]any{}
		for uri := range h.caps {
			if uri != CapCore {
				caps[uri] = struct{}{}
			}
		}
		accts[id] = sessionAcct{Name: a.owner, IsPersonal: a.personal, IsReadOnly: a.readOnly, AccountCapabilities: caps}
	}
	primary := map[string]string{}
	for uri := range h.caps {
		if uri != CapCore {
			primary[uri] = AccountID(user)
		}
	}
	return accts, primary
}

// sessionState changes whenever the session object would change, apart
// from its URLs (which depend on how the client reached the server).
func (h *Handler) sessionState(user string) string {
	accts, primary := h.sessionAccounts(user)
	sum := sha256.New()
	for _, v := range []any{user, h.caps, accts, primary} {
		b, _ := json.Marshal(v) // maps marshal with sorted keys
		sum.Write(b)
	}
	return hex.EncodeToString(sum.Sum(nil)[:8])
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request, user string) {
	base := h.baseURL(r) + h.prefix
	accts, primary := h.sessionAccounts(user)
	writeJSON(w, http.StatusOK, session{
		Capabilities:    maps.Clone(h.caps),
		Accounts:        accts,
		PrimaryAccounts: primary,
		Username:        user,
		APIURL:          base + "api",
		DownloadURL:     base + "download/{accountId}/{blobId}/{name}?type={type}",
		UploadURL:       base + "upload/{accountId}/",
		EventSourceURL:  base + "eventsource?types={types}&closeafter={closeafter}&ping={ping}",
		State:           h.sessionState(user),
	})
}
