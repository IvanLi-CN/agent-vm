package scheduler

import (
	"context"
	"time"

	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/protocol"
)

type Scheduler struct {
	Service *lifecycle.Service
	wake    chan struct{}
}

func New(service *lifecycle.Service) *Scheduler {
	scheduler := &Scheduler{Service: service, wake: make(chan struct{}, 1)}
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
	if err := scheduler.Service.StartupReconcile(ctx); err == nil {
		scheduler.Service.MarkReady()
	}
	go scheduler.sampleLoop(ctx, "admin-metrics", "capacity", 10*time.Second)
	go scheduler.sampleLoop(ctx, "admin-status", "inventory", 30*time.Second)
	go scheduler.sampleLoop(ctx, "doctor", "health", 15*time.Second)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	ageTicker := time.NewTicker(ageInterval)
	defer ageTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !scheduler.Service.Ready() {
				if err := scheduler.Service.StartupReconcile(ctx); err == nil {
					scheduler.Service.MarkReady()
				}
				continue
			}
			_, _ = scheduler.Service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "timer-reconcile", "apply": true})
		case <-ageTicker.C:
			if !scheduler.Service.Ready() {
				continue
			}
			_, _ = scheduler.Service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "timer-age-gc", "apply": true})
		case <-scheduler.wake:
			if !scheduler.Service.Ready() {
				if err := scheduler.Service.StartupReconcile(ctx); err == nil {
					scheduler.Service.MarkReady()
				}
				continue
			}
			_, _ = scheduler.Service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "timer-reconcile", "apply": true})
		}
	}
}

func (scheduler *Scheduler) sampleLoop(ctx context.Context, action, source string, interval time.Duration) {
	scheduler.sample(ctx, action, source)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scheduler.sample(ctx, action, source)
		}
	}
}

func (scheduler *Scheduler) sample(ctx context.Context, action, source string) {
	_, err := scheduler.Service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": action})
	if err == nil || scheduler.Service.Snapshots == nil {
		return
	}
	control := protocol.AsError(err)
	_ = scheduler.Service.Snapshots.Publish(source, map[string]any{
		"up":          false,
		"observed_at": time.Now().UTC().Format(time.RFC3339Nano),
		"error":       map[string]any{"code": control.Code, "message": control.Message},
	})
}
