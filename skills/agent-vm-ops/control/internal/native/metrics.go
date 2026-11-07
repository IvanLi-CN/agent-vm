package native

import (
	"context"
	"errors"
	"net/url"
)

// MetricsProbe reads Incus-owned instance metrics without granting mutation authority.
type MetricsProbe interface {
	PrometheusMetrics(context.Context) (string, error)
}

func (c *IncusClient) PrometheusMetrics(ctx context.Context) (string, error) {
	endpoint := "/1.0/metrics?" + url.Values{"project": {c.Project}}.Encode()
	result, err := c.Runner.Run(ctx, []string{"incus", "query", endpoint}, nil)
	if err != nil {
		return "", wrapCommandError(result, err)
	}
	if result.StdoutTruncated || result.StderrTruncated {
		return "", errors.New("Incus metrics exceeded diagnostic limit")
	}
	return string(result.Stdout), nil
}
