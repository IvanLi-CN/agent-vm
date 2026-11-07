package config

import "testing"

func TestInstallationAuthority(t *testing.T) {
	for _, dsn := range []string{"state.sqlite3", "http://user:secret@127.0.0.1:4001", "https://user:secret@remote.invalid:4001", "https://user:secret@127.0.0.1:4002", "https://127.0.0.1:4001", "https://user@127.0.0.1:4001", "https://user:@127.0.0.1:4001", "https://:secret@127.0.0.1:4001", "https://user:secret@127.0.0.1:4001/other", "https://user:secret@127.0.0.1:4001?", "https://user:secret@127.0.0.1:4001#fragment"} {
		if ValidateInstallationAuthority(dsn) == nil {
			t.Errorf("accepted nonlocal or unauthenticated initialization: %s", dsn)
		}
	}
	if err := ValidateInstallationAuthority("https://user:encoded%40secret@127.0.0.1:4001"); err != nil {
		t.Fatal(err)
	}
}
