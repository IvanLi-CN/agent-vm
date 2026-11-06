package deployd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

const PhaseBootstrap = "bootstrap"

type bootstrapRuntime interface {
	CheckBootstrap(context.Context) error
	PrepareBootstrap(context.Context, string) error
}

// Bootstrap establishes the first controller through the same durable journal
// and role channel used for upgrades. Missing active links never authorize a
// normal stage, nor does an installation record confer a controller role.
func (manager *Manager) Bootstrap(ctx context.Context, id, bundle string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !regexp.MustCompile(`^install-[a-f0-9]{32}$`).MatchString(id) || !filepath.IsAbs(bundle) {
		return nil, errors.New("bootstrap requires an installation transaction ID and absolute bundle")
	}
	existing, err := manager.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Bundle != bundle || existing.PreviousSlot != "none" {
			return nil, errors.New("installation transaction identity conflicts")
		}
		if existing.State == "finalized" {
			return existing, nil
		}
		return nil, fmt.Errorf("transaction %s requires explicit recover; bootstrap will not replay effects", id)
	}
	runtime, ok := manager.Runtime.(bootstrapRuntime)
	if !ok {
		return nil, errors.New("bootstrap runtime unavailable")
	}
	if err := manager.ensureNoActiveTransaction(ctx); err != nil {
		return nil, err
	}
	if err := runtime.CheckBootstrap(ctx); err != nil {
		return nil, err
	}
	now := manager.Clock().UTC()
	tx := store.Transaction{ID: id, TargetSlot: "blue", PreviousSlot: "none", Bundle: bundle, Phase: PhaseBootstrap, State: "active", Generation: now.UnixNano(), CreatedAt: now, UpdatedAt: now}
	if err := manager.Store.Create(ctx, tx, map[string]any{"fresh_install": true}); err != nil {
		return nil, err
	}
	if err := runtime.PrepareBootstrap(ctx, bundle); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseBootstrap, err)
	}
	return manager.finishBootstrap(ctx, tx)
}

func (manager *Manager) finishBootstrap(ctx context.Context, tx store.Transaction) (*store.Transaction, error) {
	id, bundle := tx.ID, tx.Bundle
	fail := func(phase string, cause error) (*store.Transaction, error) {
		return nil, manager.effectFailure(ctx, id, phase, cause)
	}
	if err := manager.Runtime.Verify(ctx, "blue", bundle); err != nil {
		return fail(PhaseBootstrap, err)
	}
	snapshot, err := manager.observe(ctx, id)
	if err != nil {
		return fail(PhaseBootstrap, err)
	}
	target := snapshot.Slots["blue"]
	other := snapshot.Slots["green"]
	if !target.ProcessUp || !target.ProcessReady || target.ControllerOwned || target.TrafficAccepting || target.ReleaseID == "" || target.SourceRevision == "" || target.StateEpoch == "" || len(target.StateCapabilities) == 0 || other.ProcessUp || other.ControllerOwned || other.TrafficAccepting {
		return fail(PhaseBootstrap, errors.New("initial standby identity and isolation were not verified"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseBootstrap, PhaseControllerHandoff, "verified", map[string]any{"release_id": target.ReleaseID, "source_revision": target.SourceRevision, "state_epoch": target.StateEpoch, "state_capabilities": target.StateCapabilities}); err != nil {
		return nil, err
	}
	lease, err := manager.newLease(ctx, "blue", tx.Generation)
	if err != nil {
		return fail(PhaseControllerHandoff, err)
	}
	if err := manager.Runtime.GrantController(ctx, "blue", lease); err != nil {
		return fail(PhaseControllerHandoff, err)
	}
	if err := manager.waitForReady(ctx, id, "blue"); err != nil {
		return fail(PhaseControllerHandoff, err)
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseControllerHandoff, PhaseTrafficEnable, "controller_granted", map[string]any{"term": lease.Term}); err != nil {
		return nil, err
	}
	if err := manager.Runtime.EnableTraffic(ctx, "blue"); err != nil {
		return fail(PhaseTrafficEnable, err)
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseTrafficEnable, PhaseActiveSwitch, "traffic_enabled", nil); err != nil {
		return nil, err
	}
	if err := manager.Runtime.SwitchActive(ctx, "blue"); err != nil {
		return fail(PhaseActiveSwitch, err)
	}
	snapshot, err = manager.observe(ctx, id)
	current := snapshot.Slots["blue"]
	if err != nil || !validObservation(snapshot, manager.Clock().UTC()) || snapshot.ActiveSlot != "blue" || !current.Ready || !current.ControllerOwned || !current.TrafficAccepting || current.ReleaseID != target.ReleaseID || current.SourceRevision != target.SourceRevision || current.StateEpoch == "" || !sameStateCapabilities(current.StateCapabilities, target.StateCapabilities) {
		return fail(PhaseActiveSwitch, errors.New("initial active controller was not verified"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseActiveSwitch, PhaseFinalize, "installed", map[string]any{"business_authority": "rqlite"}); err != nil {
		return nil, err
	}
	manager.startupFenced = false
	return manager.Store.Get(ctx, id)
}

func (runtime *HostRuntime) CheckBootstrapRecovery(context.Context) error {
	for _, path := range []string{runtime.Config.ActivePath, runtime.Config.Migration.LegacyStateDB} {
		if path == "" {
			return errors.New("bootstrap requires fixed legacy state path")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("bootstrap refuses an active link or legacy state")
		}
	}
	return nil
}
func (runtime *HostRuntime) CheckBootstrap(ctx context.Context) error {
	if err := runtime.CheckBootstrapRecovery(ctx); err != nil {
		return err
	}
	for _, slot := range []string{"blue", "green"} {
		if !runtime.systemdSlotStopped(ctx, slot) {
			return errors.New("bootstrap requires both installed slot units stopped")
		}
	}
	return nil
}

func (runtime *HostRuntime) PrepareBootstrap(ctx context.Context, bundle string) error {
	return runtime.prepareBootstrap(ctx, bundle, false)
}
func (runtime *HostRuntime) ResumeBootstrap(ctx context.Context, bundle string) error {
	return runtime.prepareBootstrap(ctx, bundle, true)
}
func (runtime *HostRuntime) prepareBootstrap(ctx context.Context, bundle string, resume bool) error {
	if err := runtime.CheckBootstrap(ctx); err != nil {
		return err
	}
	if err := runtime.stageBundle("blue", bundle); err != nil {
		return err
	}
	binary := filepath.Join(runtime.Config.ReleaseRoot, "slots", "blue", "bin", "agent-vm-control")
	// The business service owns schema creation; Deployd never opens business state.
	args := []string{"initialize-state"}
	if resume {
		args = append(args, "--resume")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), "SSL_CERT_FILE=/etc/agent-vm-native/tls/rqlite/server.crt")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initialize fresh business authority: %w", err)
	}
	if err := runtime.systemctl(ctx, "start", "agent-vm-native@blue.service"); err != nil {
		return err
	}
	identity, err := runtime.waitSlotIdentity(ctx, "blue", processIdentity{}, false)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	runtime.stagedProcess["blue"] = identity
	runtime.mu.Unlock()
	return runtime.discardStagedBackup("blue")
}

func (manager *Manager) resumeBootstrap(ctx context.Context, tx store.Transaction) (*store.Transaction, error) {
	if tx.PreviousSlot != "none" || !regexp.MustCompile(`^install-[a-f0-9]{32}$`).MatchString(tx.ID) {
		return nil, errors.New("transaction is not an initial installation")
	}
	runtime, ok := manager.Runtime.(interface {
		ResumeBootstrap(context.Context, string) error
		CheckBootstrapRecovery(context.Context) error
	})
	if !ok {
		return nil, errors.New("bootstrap recovery runtime unavailable")
	}
	if err := runtime.CheckBootstrapRecovery(ctx); err != nil {
		return nil, err
	}
	// Recovery is explicit, uses the original identity, and stops both processes
	// before validating the existing authority. It never restores a database.
	cause := errors.New("explicit initial installation recovery")
	if err := manager.fenceSlots(ctx, cause); err != cause {
		return nil, err
	}
	for _, slot := range []string{"blue", "green"} {
		if err := manager.Runtime.Stop(ctx, slot); err != nil {
			return nil, err
		}
	}
	if err := runtime.ResumeBootstrap(ctx, tx.Bundle); err != nil {
		return nil, manager.effectFailure(ctx, tx.ID, tx.Phase, err)
	}
	if _, err := manager.Store.Transition(ctx, tx.ID, tx.Phase, PhaseBootstrap, "active", "bootstrap_revalidated", "deployd", nil, manager.Clock().UTC()); err != nil {
		return nil, manager.effectFailure(ctx, tx.ID, tx.Phase, err)
	}
	tx.Phase = PhaseBootstrap
	return manager.finishBootstrap(ctx, tx)
}
