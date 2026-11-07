package state

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
)

// AuthorityIdentity identifies the configured backend independently of the
// changing state epoch. Credentials are excluded before hashing.
func (s *Store) AuthorityIdentity() string {
	identity := "sqlite:" + filepath.Clean(s.path)
	if s.remote != nil {
		endpoint := *s.remote.base
		endpoint.User = nil
		endpoint.Fragment = ""
		identity = "rqlite:" + endpoint.String()
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}
