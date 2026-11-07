package observe

import (
	"errors"
	"strconv"
	"strings"
)

// ParseIncusMetrics accepts only bounded Incus sample families. Comments are
// discarded; malformed, duplicate or foreign samples reject the entire batch.
func ParseIncusMetrics(text string) (map[string]string, error) {
	if len(text) > 2*1024*1024 {
		return nil, errors.New("Incus metrics are too large")
	}
	families := map[string]string{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		quoted, escaped, depth, split := false, false, 0, -1
		for i, c := range line {
			if escaped {
				escaped = false
				continue
			}
			if quoted && c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				quoted = !quoted
				continue
			}
			if !quoted {
				if c == '{' {
					depth++
				}
				if c == '}' {
					depth--
				}
				if depth < 0 || depth > 1 {
					return nil, errors.New("invalid Incus metric labels")
				}
				if (c == ' ' || c == '\t') && depth == 0 {
					split = i
					break
				}
			}
		}
		if quoted || depth != 0 || split < 1 {
			return nil, errors.New("invalid Incus sample")
		}
		key := line[:split]
		name := strings.SplitN(key, "{", 2)[0]
		values := strings.Fields(line[split:])
		if !strings.HasPrefix(name, "incus_") || !metricName.MatchString(name) || len(values) != 1 || seen[key] {
			return nil, errors.New("invalid or duplicate Incus metric")
		}
		if _, err := strconv.ParseFloat(values[0], 64); err != nil {
			return nil, errors.New("invalid Incus metric value")
		}
		seen[key] = true
		families[name] += line + "\n"
	}
	if len(families) == 0 {
		return nil, errors.New("Incus metrics are empty")
	}
	return families, nil
}
