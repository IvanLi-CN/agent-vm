//go:build !darwin && !freebsd && !openbsd && !netbsd && !linux

package native

func killGroup(_ int) error { return nil }
