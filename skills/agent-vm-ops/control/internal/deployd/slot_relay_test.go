package deployd

import (
	"net/http"
	"strings"
	"testing"
)

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
