package lifecycle

import (
	"context"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"time"
)

type deploymentObservationKey struct{}

// DeploymentObservation collects the three read-only lanes against one state
// view, so scheduler cursor commits cannot tear an otherwise idle snapshot.
func (s *Service) DeploymentObservation(ctx context.Context) (map[string]any, error) {
	ctx = context.WithValue(ctx, deploymentObservationKey{}, true)
	result := map[string]any{}
	err := s.Store.ReadSnapshot(ctx, func(ctx context.Context) error {
		for _, lane := range []struct {
			name string
			read func(context.Context) (map[string]any, error)
		}{
			{"capacity", s.adminMetrics},
			{"inventory", func(ctx context.Context) (map[string]any, error) { return s.adminStatus(ctx) }},
			{"health", s.doctor},
		} {
			value, err := lane.read(ctx)
			if err != nil {
				return err
			}
			result[lane.name] = value
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	capacity := result["capacity"].(map[string]any)
	capacity["incus_metrics_up"] = false
	if probe, ok := s.Guest.(native.MetricsProbe); ok {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		text, err := probe.PrometheusMetrics(probeCtx)
		cancel()
		if err == nil {
			capacity["incus_metrics"] = text
			capacity["incus_metrics_up"] = true
		}
	}
	return result, nil
}
