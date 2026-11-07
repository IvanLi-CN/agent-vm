package config

import (
	"errors"
	"net/url"
)

// ValidateInstallationAuthority restricts local first-install operations without
// changing the general runtime's supported remote state configuration.
func ValidateInstallationAuthority(dsn string) error {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return errors.New("initial installation requires authenticated HTTPS rqlite on 127.0.0.1:4001")
	}
	password, hasPassword := parsed.User.Password()
	if parsed.Scheme != "https" || parsed.Host != "127.0.0.1:4001" || parsed.User == nil || parsed.User.Username() == "" || !hasPassword || password == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return errors.New("initial installation requires authenticated HTTPS rqlite on 127.0.0.1:4001")
	}
	return nil
}
