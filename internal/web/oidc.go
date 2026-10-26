package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/chriss-de/go-ccdav/internal/config"
)

// OIDC login: authorization code flow with PKCE via x/oauth2, then the
// userinfo endpoint for the claims. The code is exchanged server to server
// over TLS with the client secret, so the userinfo response is trusted
// without verifying an ID token.

type discovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

type oidcClient struct {
	http *http.Client

	mu     sync.Mutex
	issuer string
	disc   *discovery
}

// discover fetches (and caches per issuer) the provider metadata.
func (o *oidcClient) discover(ctx context.Context, issuer string) (*discovery, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.disc != nil && o.issuer == issuer {
		return o.disc, nil
	}
	url := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	var d discovery
	if err := o.getJSON(ctx, url, "", &d); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("oidc discovery: %s lacks authorization, token or userinfo endpoint", url)
	}
	o.issuer, o.disc = issuer, &d
	return &d, nil
}

func (o *oidcClient) getJSON(ctx context.Context, url, bearer string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

func (u *UI) oauthConfig(c *config.Config, d *discovery) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.OIDC.ClientID,
		ClientSecret: c.OIDC.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: d.AuthorizationEndpoint, TokenURL: d.TokenEndpoint},
		RedirectURL:  c.BaseURL + u.prefix + "callback",
		Scopes:       c.OIDC.Scopes,
	}
}

// oidcStart redirects to the provider. state and the PKCE verifier travel
// in a short-lived signed cookie.
func (u *UI) oidcStart(w http.ResponseWriter, r *http.Request) {
	c := u.cfg.Get()
	d, err := u.oidc.discover(r.Context(), c.OIDC.Issuer)
	if err != nil {
		u.log.Error("oidc", "err", err)
		u.renderLogin(w, r, "The login provider is not reachable right now.", http.StatusBadGateway)
		return
	}
	state, verifier := randomString(18), oauth2.GenerateVerifier()
	u.setCookie(w, r, oidcCookie, u.signTimed("oidc", oidcTTL, state, verifier), oidcTTL)
	http.Redirect(w, r, u.oauthConfig(c, d).AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (u *UI) oidcCallback(w http.ResponseWriter, r *http.Request) {
	c := u.cfg.Get()
	fail := func(msg string, args ...any) {
		u.log.Warn("oidc login failed", args...)
		u.clearCookie(w, r, oidcCookie)
		u.renderLogin(w, r, msg, http.StatusForbidden)
	}
	if c.OIDC == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail("Login was cancelled or denied.", "error", e, "desc", q.Get("error_description"))
		return
	}
	ck, err := r.Cookie(oidcCookie)
	if err != nil {
		fail("Login expired, please try again.", "reason", "no state cookie")
		return
	}
	f, ok := u.verifyTimed("oidc", ck.Value)
	if !ok || len(f) != 2 || q.Get("state") == "" || q.Get("state") != f[0] {
		fail("Login expired, please try again.", "reason", "state mismatch")
		return
	}
	d, err := u.oidc.discover(r.Context(), c.OIDC.Issuer)
	if err != nil {
		fail("The login provider is not reachable right now.", "err", err)
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, u.oidc.http)
	tok, err := u.oauthConfig(c, d).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f[1]))
	if err != nil {
		fail("Login failed.", "reason", "code exchange", "err", err)
		return
	}
	var info map[string]any
	if err := u.oidc.getJSON(r.Context(), d.UserinfoEndpoint, tok.AccessToken, &info); err != nil {
		fail("Login failed.", "reason", "userinfo", "err", err)
		return
	}
	user, ok := oidcUser(c, info)
	if !ok {
		login, _ := info[c.OIDC.UsernameClaim].(string)
		fail("Your account is not allowed on this server.", "claim", c.OIDC.UsernameClaim, "value", login,
			"email_verified", info["email_verified"])
		return
	}
	u.clearCookie(w, r, oidcCookie)
	if err := u.startSession(w, r, user); err != nil {
		fail("Login failed.", "reason", "session", "err", err)
		return
	}
	u.log.Info("login", "user", user, "via", "oidc")
	http.Redirect(w, r, u.prefix, http.StatusSeeOther)
}

// oidcUser maps a login's userinfo to a configured user. With the email
// claim it matches the users' emails, verified ones only; any other claim
// must equal a user name. Crossing over would let whoever can set an IdP
// username, or an unverified email, to someone's address log in as them.
func oidcUser(c *config.Config, info map[string]any) (string, bool) {
	v, _ := info[c.OIDC.UsernameClaim].(string)
	if v == "" {
		return "", false
	}
	if c.OIDC.UsernameClaim == "email" {
		// Some IdPs send the flag as a string.
		if verified := info["email_verified"]; verified != true && verified != "true" {
			return "", false
		}
		return c.UserByEmail(v)
	}
	_, ok := c.Users[v]
	return v, ok
}
