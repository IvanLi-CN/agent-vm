//go:build linux

package controllerrole

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestMaintenanceWitnessBridgesOwnerExitOnlyUntilOriginalExpiry(t *testing.T) {
	owner := exec.Command("/bin/sleep", "60")
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = owner.Process.Kill()
		_ = owner.Wait()
	}()
	now := time.Now()
	gate := New("blue", func() time.Time { return now })
	lease := Lease{Slot: "blue", Term: 5, LeaseID: "lease", Nonce: "nonce", Expiry: now.Add(time.Minute),
		OwnerPID: owner.Process.Pid, OwnerStartTime: ProcessStartToken(owner.Process.Pid)}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	pid, start := os.Getpid(), ProcessStartToken(os.Getpid())
	mismatched := lease
	mismatched.Nonce = "other"
	if err := gate.WitnessHandoff(mismatched, pid, start); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("mismatched lease: %v", err)
	}
	if err := gate.WitnessHandoff(lease, pid, start); err != nil {
		t.Fatal(err)
	}
	renewed := lease
	renewed.Expiry = lease.Expiry.Add(time.Minute)
	if err := gate.Grant(renewed); err != nil {
		t.Fatal(err)
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	if leaseAfterExit, ok := gate.Snapshot(); !ok || leaseAfterExit != renewed {
		t.Fatalf("original authority did not survive owner exit: lease=%+v live=%v", leaseAfterExit, ok)
	}
	now = lease.Expiry
	if gate.Valid() {
		t.Fatal("witness extended maintenance beyond the deadline")
	}
	successor := renewed
	successor.Term++
	successor.OwnerPID, successor.OwnerStartTime = pid, start
	if err := gate.Grant(successor); err != nil || !gate.Valid() {
		t.Fatalf("successor lease: %v", err)
	}
}
