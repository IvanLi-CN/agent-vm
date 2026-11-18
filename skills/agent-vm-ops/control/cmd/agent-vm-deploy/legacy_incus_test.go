package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
)

func TestLegacyIncusObservationsUseOnlyFixedRequests(t *testing.T) {
	cfg := config.Config{Slot: "green", Project: "project-a", Network: "incusbr0", Pool: "pool-a", StoragePath: "/var/lib/incus/storage-pools/pool-a"}
	available := int64(4096)
	for _, tc := range []struct {
		args      []string
		operation string
		output    string
	}{
		{[]string{"query", "/1.0/networks/incusbr0/leases?project=default"}, "network-leases", `[{"address":"10.0.0.12"}]`},
		{[]string{"query", "/1.0/storage-pools/pool-a?project=default"}, "pool-capacity", `{"config":{"source":"/var/lib/incus/storage-pools/pool-a"},"driver":"dir","name":"pool-a"}`},
		{[]string{"query", "/1.0/storage-pools/pool-a/resources?project=project-a"}, "image-storage", `{"space_available":4096}`},
		{[]string{"operation", "list", "--all-projects", "--format=json"}, "incus-quiet", `[]`},
		{[]string{"query", "/1.0/networks/another/leases?project=default"}, "", ""},
		{[]string{"query", "/1.0/networks/incusbr0/leases?project=default&recursion=1"}, "", ""},
		{[]string{"query", "/1.0/storage-pools/pool-a/resources?project=another"}, "", ""},
		{[]string{"query", "-X", "DELETE", "/1.0/networks/incusbr0/leases?project=default"}, "", ""},
		{[]string{"--project", "project-a", "start", "instance-a"}, "", ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var output bytes.Buffer
			called := false
			handled, err := legacyIncusObservation(t.Context(), &output, cfg, tc.args, func(_ context.Context, request native.HostObservationRequest) (native.HostObservationResult, error) {
				called = true
				if tc.operation == "" || request.Slot != cfg.Slot || request.Operation != tc.operation {
					t.Fatalf("unexpected host request: %+v", request)
				}
				return native.HostObservationResult{Available: &available, Addresses: []string{"10.0.0.12"}, Quiet: true}, nil
			})
			if err != nil || handled != (tc.operation != "") || called != handled || strings.TrimSpace(output.String()) != tc.output {
				t.Fatalf("handled=%v called=%v output=%q error=%v", handled, called, &output, err)
			}
		})
	}
}

func TestLegacyIncusObservationDoesNotFallBackAfterMissingEvidence(t *testing.T) {
	var output bytes.Buffer
	errUnavailable := errors.New("no host evidence")
	handled, err := legacyIncusObservation(t.Context(), &output, config.Config{Slot: "blue"}, []string{"operation", "list", "--all-projects", "--format=json"}, func(context.Context, native.HostObservationRequest) (native.HostObservationResult, error) {
		return native.HostObservationResult{}, errUnavailable
	})
	if !handled || !errors.Is(err, errUnavailable) || output.Len() != 0 {
		t.Fatalf("handled=%v output=%q error=%v", handled, &output, err)
	}
}
