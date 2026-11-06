package state

import (
	"net/url"
	"strings"
	"testing"
)

func TestAuthorityIdentityExcludesCredentialsButBindsBackend(t *testing.T) {
	identity := func(endpoint string) string {
		u, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		return (&Store{remote: &remoteBackend{base: u}}).AuthorityIdentity()
	}
	first := identity("https://operator:secret@localhost:4001")
	if len(first) != 64 || strings.Contains(first, "secret") {
		t.Fatal("unsafe authority identity")
	}
	if first != identity("https://rotated:different@localhost:4001") {
		t.Fatal("credential rotation changed backend identity")
	}
	if first == identity("https://operator:secret@localhost:4002") {
		t.Fatal("different backend shared identity")
	}
	if (&Store{path: "/state/one.sqlite3"}).AuthorityIdentity() == (&Store{path: "/state/two.sqlite3"}).AuthorityIdentity() {
		t.Fatal("different local stores shared identity")
	}
}
