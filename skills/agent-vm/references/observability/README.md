# Agent VM observability bundle

This directory contains the portable Grafana source for the Agent VM observability contract. It is a parameterized baseline, not a deployment export.

## Render

Copy [the example configuration](observability.config.json.example), replace every placeholder with values owned by the deployment, and render the bundle into a staging directory:

```bash
bun skills/agent-vm/scripts/render_observability_dashboard.ts \
  --config /path/to/agent-vm-observability.json \
  --output-dir /path/to/deployment/grafana/generated \
  --force
```

The renderer writes `agent-vm-observability.json` and `agent-vm-observability.yml`. The JSON is the dashboard source; the YAML is a Grafana file provider definition whose `path` must be the path visible inside the Grafana container.

The deployment repository should review and commit the rendered files together with its Prometheus scrape configuration and read-only controller metrics bridge. The public repository does not receive those rendered files, datasource credentials, hostnames, collector paths, or live inventory.

## Configuration

| Field | Meaning |
| --- | --- |
| `dashboard_uid` | Stable Grafana dashboard UID for this deployment. |
| `dashboard_title` | Dashboard title shown in Grafana. |
| `grafana_org_id` | Numeric Grafana organization ID that owns the dashboard. |
| `provider_name` | Grafana provisioning provider name. |
| `dashboard_folder` | Grafana folder for the dashboard. |
| `prometheus_datasource_uid` | Existing Prometheus datasource UID. |
| `controller_job` | Prometheus `job` label for the read-only Agent VM controller bridge. |
| `dashboard_path` | Container-visible directory that Grafana scans for JSON dashboards. |
| `dashboard_filename` | Optional rendered filename; defaults to `agent-vm-observability.json`. |

The template uses a hidden `controller_job` variable so controller queries remain portable while the deployment supplies its actual job label. CPU capacity is read from `agent_vm_admission_cpu_capacity_vcpu`; memory and disk limits are rendered only from their authoritative metric families.

## Validate

The renderer validates both outputs before writing them. A previously rendered bundle can be checked directly:

```bash
bun skills/agent-vm/scripts/validate_observability_dashboard.ts \
  --dashboard /path/to/deployment/grafana/generated/agent-vm-observability.json \
  --provisioning /path/to/deployment/grafana/generated/agent-vm-observability.yml
```

The validator checks the required panel/metric contract, automatic refresh, datasource consistency, the controller variable, and the rule that packet-rate PromQL must not multiply by `8`.
