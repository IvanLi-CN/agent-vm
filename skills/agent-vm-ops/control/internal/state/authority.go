package state

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// AuthorityIdentity identifies the configured backend independently of the
// changing state epoch. Credentials are excluded before hashing.
func (s *Store) AuthorityIdentity() string {
	if s.remote != nil {
		return RQLiteAuthorityIdentity(*s.remote.base)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte("sqlite:"+filepath.Clean(s.path))))
}

func RQLiteAuthorityIdentity(endpoint url.URL) string {
	endpoint.User = nil
	endpoint.Fragment = ""
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if endpoint.Path == "" {
		endpoint.Path = "/"
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte("rqlite:"+endpoint.String())))
}
