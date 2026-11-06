package lifecycle

import "context"

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
	return result, nil
}
