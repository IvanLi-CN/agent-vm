package controllerrole

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestProcessIdentityParsesNamesWithClosingParen(t *testing.T) {
	stat := "42 (name ) with spaces) R " + strings.Repeat("0 ", 18) + "123 0"
	if state, token := parseProcessIdentity(stat); state != "R" || token != "123" {
		t.Fatalf("state=%q token=%q", state, token)
	}
	for _, malformed := range []string{"42 (name)", "42 (name) ", "42 (name) R " + strings.Repeat("0 ", 19)} {
		if _, token := parseProcessIdentity(malformed); token != "" {
			t.Fatalf("invalid stat accepted: %q", malformed)
		}
	}
}

func TestCrossAccountOwnerIdentity(t *testing.T) {
	raw := os.Getenv("AGENT_VM_TEST_CROSS_UID_OWNER_PID")
	if raw == "" {
		t.Skip("requires a live process owned by another user")
	}
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 0 {
		t.Fatalf("invalid test PID: %q", raw)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("fixture does not reproduce signal permission denial: %v", err)
	}
	token := ProcessStartToken(pid)
	if token == "" || !ownerAlive(pid, token) || ownerAlive(pid, token+"0") {
		t.Fatalf("cross-account process identity was not verified exactly: token=%q", token)
	}
}
