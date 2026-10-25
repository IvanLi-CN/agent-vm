# Agent VM observability bundle

This directory contains the portable Grafana source for the Agent VM observability contract. It is a parameterized baseline, not a deployment export.

## Render

Copy [the example configuration](observability.config.json.example), replace every placeholder with values owned by the deployment, and render the bundle into a bundle root:

```bash
bun skills/agent-vm/scripts/render_observability_dashboard.ts \
  --config /path/to/agent-vm-observability.json \
  --output-dir /path/to/deployment/grafana/generated \
  --force
```

The renderer writes the parameterized capacity dashboard, inventory dashboard, and Grafana file-provider definition. Dashboard JSON files are published below `generated/current`, which is replaced as one atomic symlink; previous releases remain below `generated/.releases`. The deployment implementation must set `dashboard_path` to the container-visible `current` directory and copy the provisioning YAML as one atomic file replacement. This prevents Grafana from observing a half-published dashboard bundle.

The deployment repository should review and commit the rendered files together with its Prometheus scrape configuration and read-only controller metrics bridge. The public repository does not receive those rendered files, datasource credentials, hostnames, collector paths, or live inventory.

If provisioning activation fails and the previous `current` link cannot be restored, the renderer removes the stable link and retains the staged release for recovery rather than serving a mixed dashboard/provisioning pair.

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
| `dashboard_path` | Container-visible `current` directory that Grafana scans for JSON dashboards; it must point at the stable active-bundle path, not the release parent. |
| `dashboard_filename` | Optional rendered filename; defaults to `agent-vm-observability.json`. |
| `inventory_dashboard_uid` | Optional stable UID for the inventory dashboard; defaults to `<dashboard_uid>-inventory`. |
| `inventory_dashboard_title` | Optional inventory dashboard title; defaults to `<dashboard_title> / Inventory & Operations`. |
| `inventory_dashboard_filename` | Optional inventory filename; defaults to `agent-vm-observability-inventory.json`. |

The template uses a hidden `controller_job` variable so controller queries remain portable while the deployment supplies its actual job label. CPU capacity is read from `agent_vm_admission_cpu_capacity_vcpu`; memory and disk limits are rendered only from their authoritative metric families.

The capacity dashboard separates logical admission from runtime pressure: its summary shows logical RAM remaining, CT-local `MemAvailable`, pressure/OOM state, and compact CPU/disk readiness; the policy snapshot below shows the default request, slot estimates, and task-state counts. The inventory dashboard exposes every lifecycle state through the state filter, shows active and transitional/failed rows by default, and keeps cached/absent history collapsed until selected. Inventory fields are joined by a `task_id|state` snapshot key so a lifecycle transition cannot combine resource data from different states.

## Validate

The renderer validates both dashboards before writing them. A previously rendered bundle can be checked directly:

```bash
bun skills/agent-vm/scripts/validate_observability_dashboard.ts \
  --dashboard /path/to/deployment/grafana/generated/current/agent-vm-observability.json \
  --provisioning /path/to/deployment/grafana/generated/agent-vm-observability.yml
```

Use the inventory profile when checking `agent-vm-observability-inventory.json`:

```bash
bun skills/agent-vm/scripts/validate_observability_dashboard.ts \
  --dashboard /path/to/deployment/grafana/generated/current/agent-vm-observability-inventory.json \
  --provisioning /path/to/deployment/grafana/generated/agent-vm-observability.yml \
  --profile inventory
```

The validator checks the selected dashboard profile's panel/metric contract, automatic refresh, datasource consistency, the controller variable, and the rule that packet-rate PromQL must not multiply by `8`.
