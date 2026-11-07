package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

type Scheduler struct {
	Service *lifecycle.Service
	Gate    LeaseGate
	Startup func(context.Context) error
	wake    chan struct{}
}

type LeaseGate interface {
	Valid() bool
}

func New(service *lifecycle.Service, gate LeaseGate) *Scheduler {
	return NewWithStartup(service, gate, nil)
}

// NewWithStartup adds controller-gated startup work that must complete before
// lifecycle admission is marked ready. The callback runs only after the
// scheduler has observed a valid controller lease.
func NewWithStartup(service *lifecycle.Service, gate LeaseGate, startup func(context.Context) error) *Scheduler {
	scheduler := &Scheduler{Service: service, Gate: gate, Startup: startup, wake: make(chan struct{}, 1)}
	service.SetWake(scheduler.Wake)
	return scheduler
}

func (scheduler *Scheduler) Wake() {
	select {
	case scheduler.wake <- struct{}{}:
	default:
	}
}

func (scheduler *Scheduler) Run(ctx context.Context) error {
	return scheduler.run(ctx, time.Second, 15*time.Minute)
}

func (scheduler *Scheduler) run(ctx context.Context, reconcileInterval, ageInterval time.Duration) error {
	if scheduler.Service.Snapshots != nil {
		go scheduler.sampleObservationLoop(ctx)
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	ageTicker := time.NewTicker(ageInterval)
	defer ageTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !scheduler.controllerLeaseValid() {
				scheduler.Service.MarkNotReady()
				continue
			}
			if !scheduler.Service.Ready() {
				if err := scheduler.startupReconcile(ctx); err == nil {
					scheduler.Service.MarkReady()
				} else {
					slog.Warn("startup reconcile failed", "error", err)
				}
				continue
			}
			if err := scheduler.Service.RunMaintenance(ctx, "reconcile"); err != nil {
				if protocol.AsError(err).Code == "controller_lease_required" {
					scheduler.Service.MarkNotReady()
				} else {
					slog.Warn("scheduled reconcile failed", "error", err)
				}
			}
		case <-ageTicker.C:
			if !scheduler.controllerLeaseValid() || !scheduler.Service.Ready() {
				continue
			}
			if err := scheduler.Service.RunMaintenance(ctx, "age-gc"); err != nil {
				if protocol.AsError(err).Code == "controller_lease_required" {
					scheduler.Service.MarkNotReady()
				} else {
					slog.Warn("scheduled age-gc failed", "error", err)
				}
			}
		case <-scheduler.wake:
			if !scheduler.controllerLeaseValid() {
				scheduler.Service.MarkNotReady()
				continue
			}
			if !scheduler.Service.Ready() {
				if err := scheduler.startupReconcile(ctx); err == nil {
					scheduler.Service.MarkReady()
				} else {
					slog.Warn("startup reconcile failed", "error", err)
				}
				continue
			}
			if err := scheduler.Service.RunMaintenance(ctx, "reconcile"); err != nil {
				if protocol.AsError(err).Code == "controller_lease_required" {
					scheduler.Service.MarkNotReady()
				} else {
					slog.Warn("wake reconcile failed", "error", err)
				}
			}
		}
	}
}

func (scheduler *Scheduler) startupReconcile(ctx context.Context) error {
	if scheduler.Startup != nil {
		if err := scheduler.Startup(ctx); err != nil {
			return err
		}
	}
	return scheduler.Service.StartupReconcile(ctx)
}

func (scheduler *Scheduler) controllerLeaseValid() bool {
	return scheduler.Gate != nil && scheduler.Gate.Valid()
}

// Sample related read-only lanes once, instead of competing for the remote
// mirror with three independent epoch-synchronizing observation loops.
func (scheduler *Scheduler) sampleObservationLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		sampleCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		lanes, err := scheduler.Service.DeploymentObservation(sampleCtx)
		cancel()
		for _, source := range []string{"capacity", "inventory", "health"} {
			value := lanes[source]
			if err != nil {
				control := protocol.AsError(err)
				value = map[string]any{"up": false, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "error": map[string]any{"code": control.Code, "message": control.Message}}
			}
			_ = scheduler.Service.Snapshots.Publish(source, value)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
