package deployd

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

func TestHistoricalLeaseRecognitionRequiresExactGrant(t *testing.T) {
	pid := os.Getpid()
	token := controllerrole.ProcessStartToken(pid)
	if token == "" {
		t.Skip("requires Linux process identity")
	}
	runtime := NewHostRuntime(Defaults())
	lease := controllerrole.Lease{Slot: "blue", Term: 42, Generation: 7, LeaseID: "known", Nonce: "nonce", OwnerPID: pid, OwnerStartTime: token, Expiry: time.Now().UTC().Add(time.Minute)}
	runtime.legacyRelayGrants = map[string]controllerrole.Lease{"blue": lease}
	matched := map[string]any{"lease": &lease}
	runtime.normalizeHistoricalLease("blue", matched)
	if _, ok := matched["lease"].(controllerrole.Lease); !ok {
		t.Fatal("confirmed lease was not recognized")
	}
	other := lease
	other.Generation++
	mismatched := map[string]any{"lease": &other}
	runtime.normalizeHistoricalLease("blue", mismatched)
	if mismatched["lease"] != &other {
		t.Fatal("a different generation was recognized as a confirmed lease")
	}
}

func TestSlotRelayOnlyAllowsDeploymentOperations(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    slotRelayRequest
		want bool
	}{
		{"health", slotRelayRequest{Slot: "blue", Method: http.MethodGet, Path: "/internal/v1/health"}, true},
		{"deployment drain", slotRelayRequest{Slot: "green", Method: http.MethodPost, Path: "/internal/v1/control", Body: []byte(`{"protocol":4,"action":"drain-exit","session_id":"release-agent-vm-deployd","generation":0,"operation_id":"test"}`)}, true},
		{"other drain", slotRelayRequest{Slot: "green", Method: http.MethodPost, Path: "/internal/v1/control", Body: []byte(`{"protocol":4,"action":"drain-exit","session_id":"other","generation":0,"operation_id":"test"}`)}, false},
		{"guest destroy", slotRelayRequest{Slot: "blue", Method: http.MethodPost, Path: "/internal/v1/control", Body: []byte(`{"protocol":4,"action":"destroy","session_id":"guest","generation":1,"operation_id":"test"}`)}, false},
		{"arbitrary route", slotRelayRequest{Slot: "blue", Method: http.MethodGet, Path: "/api/v1/admin"}, false},
		{"invalid slot", slotRelayRequest{Slot: "../green", Method: http.MethodGet, Path: "/internal/v1/health"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.valid(); got != tc.want {
				t.Fatalf("valid = %t, want %t", got, tc.want)
			}
		})
	}
	for _, invalid := range []string{
		`{"Slot":"blue","Method":"GET","Path":"/internal/v1/health","Socket":"/arbitrary"}`,
		`{"Slot":"blue","Method":"GET","Path":"/internal/v1/health"}{}`,
		`{"Slot":"blue","Method":"GET","Path":"/internal/v1/health"}` + strings.Repeat(" ", slotRelayRequestLimit),
	} {
		if err := readSlotRelayMessage(strings.NewReader(invalid), slotRelayRequestLimit, &slotRelayRequest{}); err == nil {
			t.Fatal("decoded invalid relay request")
		}
	}
}
