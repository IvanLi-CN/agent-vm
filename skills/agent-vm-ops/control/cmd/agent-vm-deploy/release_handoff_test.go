package main

import "testing"

func TestDedicatedUpdateReadyRejectsLegacyStatus(t *testing.T) {
	for _, status := range []string{
		`{"ok":true,"value":{}}`,
		`{"ok":true,"value":{"dedicated_service_account":false}}`,
		`{"ok":false,"value":{"dedicated_service_account":true}}`,
	} {
		if dedicatedUpdateReady([]byte(status)) {
			t.Fatalf("accepted status without dedicated service evidence: %s", status)
		}
	}
	if !dedicatedUpdateReady([]byte(`{"ok":true,"value":{"dedicated_service_account":true}}`)) {
		t.Fatal("rejected dedicated service status")
	}
}
