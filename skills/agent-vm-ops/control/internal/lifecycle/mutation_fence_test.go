package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

func TestMutationContextCancelsWhenControllerLeaseChanges(t *testing.T) {
	now := time.Now().UTC()
	gate := controllerrole.New("blue", func() time.Time { return now })
	if err := gate.Grant(controllerrole.Lease{
		Slot: "blue", Term: 1, LeaseID: "lease-1", Nonce: "nonce-1", Generation: 1,
		Expiry: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	service.SetMutationGate(gate.Valid)
	service.SetMutationFence(gate.Changes)
	fenced, stop, lost := service.mutationContext(context.Background())
	defer stop()

	gate.Fence()
	select {
	case <-fenced.Done():
	case <-time.After(time.Second):
		t.Fatal("mutation context did not cancel after controller fencing")
	}
	if !lost() {
		t.Fatal("mutation context did not record controller lease loss")
	}
}
