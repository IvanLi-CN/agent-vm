package deployd

import (
	"context"
	"strings"
	"testing"
)

func TestHostActionRejectsUntrustedOperations(t *testing.T) {
	for name, input := range map[string]string{
		"unknown unit":        `{"action":"stop","slot":"sshd.service"}`,
		"unknown action":      `{"action":"daemon-reload","slot":"blue"}`,
		"arbitrary path":      `{"action":"start","slot":"blue","layout":"/bin/sh"}`,
		"arbitrary argument":  `{"action":"stop","slot":"blue","unit":"sshd.service"}`,
		"non-bootstrap slot":  `{"action":"initialize","slot":"green"}`,
		"arbitrary init mode": `{"action":"resume-initialize","slot":"blue","layout":"control"}`,
		"second request":      `{"action":"stop","slot":"green"}{}`,
		"oversized request":   `{"action":"stop","slot":"green"}` + strings.Repeat(" ", 512),
	} {
		t.Run(name, func(t *testing.T) {
			err := handleHostAction(t.Context(), strings.NewReader(input), func(context.Context, hostAction) error {
				t.Fatal("invalid request reached the effect handler")
				return nil
			})
			if err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
	var got hostAction
	err := handleHostAction(t.Context(), strings.NewReader(`{"action":"restart","slot":"green","layout":"control","rollback":true}`), func(_ context.Context, action hostAction) error {
		got = action
		return nil
	})
	if err != nil || got != (hostAction{Action: "restart", Slot: "green", Layout: "control", Rollback: true}) {
		t.Fatalf("expected fixed legacy rollback request, got %#v (%v)", got, err)
	}
}

func TestSlotIdentityRequiresConfiguredAndRunningUnprivilegedUser(t *testing.T) {
	for _, output := range []string{
		"MainPID=123\nUser=root\n",
		"MainPID=123\nUser=\n",
		"MainPID=123\nUser=agent-vm-server\nUser=root\n",
		"MainPID=-1\nUser=agent-vm-server\n",
	} {
		if _, err := unprivilegedSlotPID(output); err == nil {
			t.Errorf("accepted invalid systemd identity %q", output)
		}
	}
	pid, err := unprivilegedSlotPID("MainPID=123\nUser=agent-vm-server\n")
	if err != nil || pid != 123 {
		t.Fatalf("valid systemd identity returned pid=%d err=%v", pid, err)
	}
	if !processUsesSlotUID("Name:\tagent-vm-server\nUid:\t997\t997\t997\t997\n", "997") ||
		processUsesSlotUID("Name:\tagent-vm-server\nUid:\t997\t997\t0\t997\n", "997") {
		t.Fatal("expected all process UIDs to match the dedicated account")
	}
}
