package server

import (
	"net/http"
	"testing"

	"github.com/chriss-de/go-ccdav/internal/config"
	"github.com/chriss-de/go-ccdav/internal/directory"
)

func TestDirectoryReadOnly(t *testing.T) {
	var cfg *config.Config
	srv, st := newServer(t, func(c *config.Config) {
		c.Groups = map[string][]string{"family": {"alice", "bob"}}
		cfg = c
	})
	if err := directory.Sync(cfg, st, DefaultAddressBooks); err != nil {
		t.Fatal(err)
	}
	alice := &client{t, srv, "alice"}
	book := "/dav/addressbooks/alice/" + directory.Name + "/"
	card := book + directory.UserUID("bob") + ".vcf"

	// Listed in the home next to the default, with read-only privileges.
	_, out := alice.expect("PROPFIND", "/dav/addressbooks/alice/", `<d:propfind xmlns:d="DAV:"><d:prop><d:displayname/><d:current-user-privilege-set/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, "/dav/addressbooks/alice/contacts/", book, "<d:displayname>Directory</d:displayname>")
	_, out = alice.expect("PROPFIND", book, `<d:propfind xmlns:d="DAV:"><d:prop><d:current-user-privilege-set/></d:prop></d:propfind>`, http.StatusMultiStatus, "Depth", "0")
	contains(t, out, "<d:read/>")
	notContains(t, out, "<d:write", "<d:bind/>", "<d:all/>")

	// Readable and queryable.
	_, out = alice.expect("GET", card, "", http.StatusOK)
	contains(t, out, "FN:bob")
	_, out = alice.expect("REPORT", book, `<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:prop><d:getetag/></d:prop><card:filter><card:prop-filter name="EMAIL"><card:text-match>alice@</card:text-match></card:prop-filter></card:filter></card:addressbook-query>`, http.StatusMultiStatus, "Depth", "1")
	contains(t, out, directory.UserUID("alice"))
	notContains(t, out, directory.UserUID("bob"))

	// No writes of any kind.
	alice.expect("PUT", card, "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:x\r\nFN:x\r\nEND:VCARD\r\n", http.StatusForbidden)
	alice.expect("PUT", book+"new.vcf", "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:y\r\nFN:y\r\nEND:VCARD\r\n", http.StatusForbidden)
	alice.expect("DELETE", card, "", http.StatusForbidden)
	alice.expect("DELETE", book, "", http.StatusForbidden)
	_, out = alice.expect("PROPPATCH", book, `<d:propertyupdate xmlns:d="DAV:"><d:set><d:prop><d:displayname>Mine</d:displayname></d:prop></d:set></d:propertyupdate>`, http.StatusMultiStatus)
	contains(t, out, "HTTP/1.1 403")
	if c, _ := st.Collection("alice", "addressbooks", directory.Name); c.Meta().DisplayName != "Directory" || len(c.List()) != 4 {
		t.Fatalf("directory changed: %+v, %d cards", c.Meta(), len(c.List()))
	}
}
