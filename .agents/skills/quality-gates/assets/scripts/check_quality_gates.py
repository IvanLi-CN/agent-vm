#!/usr/bin/env python3
from __future__ import annotations
import argparse
import json
import re
from datetime import datetime
from dataclasses import dataclass, field, replace
from itertools import product
from pathlib import Path
from typing import Any
REVIEW_POLICY_WORKFLOW_NAMES = {"Review Policy", "review-policy"}
REQUIRED_REVIEW_POLICY_TRIGGER_TYPES = {
    "pull_request_target": {"opened", "reopened", "synchronize", "ready_for_review", "labeled", "unlabeled"},
    "pull_request_review": {"submitted", "dismissed", "edited"},
    "merge_group": {"checks_requested"},
}
DEFAULT_PULL_REQUEST_ACTIVITY_TYPES = {"opened", "synchronize", "reopened"}
PR_GATE_WORKFLOW_EVENTS = {"pull_request", "pull_request_target"}
# Workflows that can emit check contexts against commits that participate in merge gating.
CHECK_CONTEXT_WORKFLOW_EVENTS = PR_GATE_WORKFLOW_EVENTS | {"push", "merge_group"}
REPO_COLLABORATOR_ROLE_NAMES = {"admin", "maintain", "write", "triage", "read", "none"}
class JsonInputError(Exception):
    def __init__(self, path: Path, message: str):
        super().__init__(f"{path}: {message}")
        self.path = path
        self.message = message

class InputValidationError(Exception):
    pass

def load_json(path: Path) -> Any:
    try:
        raw = path.read_text(encoding="utf-8")
    except OSError as exc:
        reason = exc.strerror or str(exc)
        raise JsonInputError(path, f"failed to read file: {reason}") from None
    try:
        return json.loads(raw)
    except json.JSONDecodeError as exc:
        raise JsonInputError(
            path,
            f"invalid JSON: {exc.msg} (line {exc.lineno}, column {exc.colno})",
        ) from None
def strip_inline_comment(value: str) -> str:
    output: list[str] = []
    quote: str | None = None
    for index, char in enumerate(value):
        if quote is not None:
            output.append(char)
            if char == quote:
                quote = None
            continue
        if char in {'"', "'"}:
            quote = char
            output.append(char)
            continue
        if char == '#' and (index == 0 or value[index - 1].isspace()):
            break
        output.append(char)
    return ''.join(output).rstrip()
def parse_scalar(value: str) -> str:
    value = strip_inline_comment(value).strip()
    if not value:
        return ""
    if value[0] == value[-1] and value[0] in {'"', "'"}:
        inner = value[1:-1]
        # Best-effort YAML double-quoted scalar unescaping. This validator intentionally uses a
        # lightweight scanner instead of a full YAML parser, but we still want to accept common
        # escape sequences such as \" in job names and env JSON strings.
        if value[0] == '"':
            inner = inner.replace('\\"', '"').replace("\\\\", "\\")
        else:
            # YAML single-quoted scalars escape a single quote by doubling it.
            inner = inner.replace("''", "'")
        return inner
    return value

def split_mapping_line(normalized: str) -> tuple[str | None, str]:
    """Split a YAML `key: value` line and normalize quoted keys.

    The validator intentionally uses lightweight scanning instead of a full YAML
    parser. This helper keeps the scanners tolerant of `'key': value` forms.
    """
    if ":" not in normalized:
        return None, ""
    raw_key, tail = normalized.split(":", 1)
    key = parse_scalar(raw_key.strip())
    return (key if key else None), tail.strip()

def flow_brace_depth(value: str, depth: int = 0, quote: str | None = None) -> tuple[int, str | None]:
    """Track `{ ... }` flow-mapping balance for lightweight YAML scanning.

    GitHub Actions workflows often use multi-line flow mappings, e.g.
      jobs: {
        lint: { name: Lint, ... }
      }
    Our parser uses `parse_inline_mapping()` which expects the mapping to be on one line,
    so we collapse multi-line flow mappings into a single line before scanning.
    """
    for char in value:
        if quote is not None:
            if char == quote:
                quote = None
            continue
        if char in {'"', "'"}:
            quote = char
            continue
        if char == "{":
            depth += 1
            continue
        if char == "}":
            depth = max(0, depth - 1)
            continue
    return depth, quote

def collapse_multiline_flow_mappings(lines: list[str]) -> list[str]:
    """Collapse multi-line flow mappings (`{ ... }`) into a single logical line."""
    output: list[str] = []
    i = 0
    while i < len(lines):
        raw = lines[i]
        if not raw.strip() or raw.lstrip().startswith("#"):
            output.append(raw)
            i += 1
            continue
        indent_prefix = raw[: len(raw) - len(raw.lstrip(" "))]
        normalized = strip_inline_comment(raw.strip())
        if not normalized or ":" not in normalized:
            output.append(raw)
            i += 1
            continue

        head, tail = normalized.split(":", 1)
        tail = tail.strip()
        tail_token = tail.lstrip()
        if not (tail_token.startswith("{") or tail_token.startswith("&")):
            output.append(raw)
            i += 1
            continue

        depth, quote = flow_brace_depth(tail)
        if depth <= 0:
            output.append(raw)
            i += 1
            continue

        collected = tail
        j = i
        while depth > 0 and j + 1 < len(lines):
            j += 1
            nxt = lines[j]
            if not nxt.strip() or nxt.lstrip().startswith("#"):
                continue
            nxt_normalized = strip_inline_comment(nxt.strip())
            if not nxt_normalized:
                continue
            collected += " " + nxt_normalized.strip()
            depth, quote = flow_brace_depth(nxt_normalized, depth, quote)

        if depth != 0:
            # Unbalanced braces: keep the original line to avoid hiding syntax errors.
            output.append(raw)
            i += 1
            continue

        output.append(f"{indent_prefix}{head.strip()}: {collected.strip()}")
        i = j + 1
    return output

def read_yaml_lines(path: Path) -> list[str]:
    return collapse_multiline_flow_mappings(path.read_text(encoding="utf-8").splitlines())

ALWAYS_FALSE_JOB_IF_RE = re.compile(r"^\$\{\{\s*false\s*\}\}$", re.IGNORECASE)
def job_condition_is_always_false(condition: str | None) -> bool:
    if condition is None:
        return False
    token = parse_scalar(condition).strip()
    if not token:
        return False
    lowered = token.lower()
    return lowered == "false" or ALWAYS_FALSE_JOB_IF_RE.fullmatch(token) is not None
MATRIX_CONTEXT_RE = re.compile(r"\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*}}")
INPUT_CONTEXT_RE = re.compile(r"\$\{\{\s*inputs\.([A-Za-z0-9_-]+)\s*}}")
WORKFLOW_EXPRESSION_RE = re.compile(r"\$\{\{.*?\}\}")
@dataclass
class WorkflowJob:
    job_id: str
    name: str
    if_condition: str | None = None
    uses: str | None = None
    with_inputs: dict[str, str] = field(default_factory=dict)
    matrix: dict[str, list[str]] = field(default_factory=dict)
    matrix_include: list[dict[str, str]] = field(default_factory=list)
    matrix_exclude: list[dict[str, str]] = field(default_factory=list)
    matrix_expandable: bool = True
    has_matrix: bool = False
@dataclass
class WorkflowSpec:
    path: str
    workflow: str
    jobs: list[WorkflowJob]
@dataclass
class WorkflowInventory:
    path: str
    workflow: str
    jobs: list[str]
    remote_reusable_jobs: list[str] = field(default_factory=list)
    unresolved_matrix_jobs: list[str] = field(default_factory=list)
REQUIRED_REVIEW_POLICY_PERMISSIONS = {
    "contents": "read",
    "pull-requests": "read",
}
REQUIRED_REVIEW_POLICY_SCRIPT_MARKERS = (
    "resolveMergeGroupPullNumbers",
    "parsePullNumbersFromText",
    "context.payload.merge_group?.head_ref",
    "Merge queue member set could not be proven from GitHub-disclosed data",
    "listCommitAssociatedPullNumbers",
    "GET /repos/{owner}/{repo}/commits/{commit_sha}/pulls",
    "role_name",
    "github.rest.repos.getCollaboratorPermissionLevel",
    "github.rest.pulls.get",
    "github.paginate(github.rest.pulls.listReviews",
)
FORBIDDEN_REVIEW_POLICY_RUNTIME_MARKERS = (
    "actions/checkout@",
    "github.event.pull_request.head.sha",
    "github.event.pull_request.head.ref",
)
def dedupe_preserve_order(values: list[str]) -> list[str]:
    seen: set[str] = set()
    output: list[str] = []
    for value in values:
        if value and value not in seen:
            seen.add(value)
            output.append(value)
    return output
def dedupe_mapping_list(values: list[dict[str, str]]) -> list[dict[str, str]]:
    seen: set[tuple[tuple[str, str], ...]] = set()
    output: list[dict[str, str]] = []
    for value in values:
        key = tuple(sorted(value.items()))
        if value and key not in seen:
            seen.add(key)
            output.append(value)
    return output
def matrix_combo_matches(combo: dict[str, str], criteria: dict[str, str]) -> bool:
    return all(combo.get(key) == value for key, value in criteria.items())
def can_apply_matrix_include(base_combo: dict[str, str], include: dict[str, str]) -> bool:
    return all(key not in base_combo or base_combo[key] == value for key, value in include.items())
def expand_matrix_combinations(job: WorkflowJob) -> list[dict[str, str]]:
    if not job.matrix_expandable:
        return []
    base_axes = list(job.matrix.items())
    base_combos: list[dict[str, str]] = []
    if base_axes:
        axis_values = [values for _, values in base_axes]
        if any(not values for values in axis_values):
            return []
        for combo in product(*axis_values):
            base_combos.append({base_axes[index][0]: value for index, value in enumerate(combo)})
    filtered_base_combos = [
        combo
        for combo in base_combos
        if not any(matrix_combo_matches(combo, exclude) for exclude in job.matrix_exclude)
    ]
    expanded = [combo.copy() for combo in filtered_base_combos]
    for include in job.matrix_include:
        matched = False
        for index, base_combo in enumerate(filtered_base_combos):
            if can_apply_matrix_include(base_combo, include):
                expanded[index].update(include)
                matched = True
        if not matched:
            expanded.append(include.copy())
    return dedupe_mapping_list(expanded)
def local_reusable_workflow_ref(value: str | None) -> str | None:
    if not value:
        return None
    ref = parse_scalar(value).split("@", 1)[0]
    if ref.startswith("./"):
        ref = ref[2:]
    if not ref.startswith(".github/workflows/"):
        return None
    return str(Path(ref))
def is_remote_reusable_workflow_ref(value: str | None) -> bool:
    if not value:
        return False
    ref = parse_scalar(value).split("@", 1)[0]
    return "/.github/workflows/" in ref and not ref.startswith("./")
def apply_input_context(value: str | None, inputs: dict[str, str]) -> str | None:
    if value is None or not inputs:
        return value
    def replace_input(match: re.Match[str]) -> str:
        input_name = match.group(1)
        return inputs.get(input_name, match.group(0))
    return INPUT_CONTEXT_RE.sub(replace_input, value)

def apply_input_context_mapping(mapping: dict[str, str], inputs: dict[str, str]) -> dict[str, str]:
    if not inputs:
        return dict(mapping)
    # Preserve empty-string substitutions; only fall back when the input is unset (None).
    return {
        key: (resolved if (resolved := apply_input_context(value, inputs)) is not None else value)
        for key, value in mapping.items()
    }

def apply_inputs_to_job(job: WorkflowJob, inputs: dict[str, str]) -> WorkflowJob:
    if not inputs:
        return job
    return replace(
        job,
        name=(resolved if (resolved := apply_input_context(job.name, inputs)) is not None else job.name),
        uses=apply_input_context(job.uses, inputs),
        with_inputs=apply_input_context_mapping(job.with_inputs, inputs),
        matrix={
            key: [
                (resolved if (resolved := apply_input_context(value, inputs)) is not None else value)
                for value in values
            ]
            for key, values in job.matrix.items()
        },
        matrix_include=[apply_input_context_mapping(entry, inputs) for entry in job.matrix_include],
        matrix_exclude=[apply_input_context_mapping(entry, inputs) for entry in job.matrix_exclude],
    )

def apply_matrix_context(value: str | None, combo: dict[str, str]) -> str | None:
    if value is None or not combo:
        return value
    def replace_matrix(match: re.Match[str]) -> str:
        matrix_name = match.group(1)
        return combo.get(matrix_name, match.group(0))
    return MATRIX_CONTEXT_RE.sub(replace_matrix, value)

def apply_matrix_context_mapping(mapping: dict[str, str], combo: dict[str, str]) -> dict[str, str]:
    if not combo:
        return dict(mapping)
    # Preserve empty-string substitutions; only fall back when the matrix value is unset (None).
    return {
        key: (resolved if (resolved := apply_matrix_context(value, combo)) is not None else value)
        for key, value in mapping.items()
    }

def normalize_input_values(inputs: dict[str, str] | None) -> tuple[tuple[str, str], ...]:
    # Empty-string defaults are meaningful for workflow_call inputs; keep them in the cache key so
    # placeholder expansion can intentionally resolve to "" instead of being treated as "unset".
    return tuple(sorted((key, value) for key, value in (inputs or {}).items() if key and value is not None))

def expand_matrix_job_variants(job: WorkflowJob) -> list[WorkflowJob]:
    combos = expand_matrix_combinations(job)
    if not combos:
        # A matrix job with an explicitly empty axis (e.g. `python: []`) produces no runs on
        # GitHub Actions. Treat it as "missing" rather than silently inventing a single check.
        if job.has_matrix and any(not values for values in job.matrix.values()):
            return []
        return [job]
    variants: list[WorkflowJob] = []
    seen: set[tuple[str, tuple[tuple[str, str], ...], str | None]] = set()
    for combo in combos:
        matrix_keys = dedupe_preserve_order(MATRIX_CONTEXT_RE.findall(job.name))
        if matrix_keys:
            name = job.name
            for key in matrix_keys:
                value = combo.get(key)
                # Empty-string matrix values are valid (for example `suffix: ""`).
                if value is None:
                    return [job]
                name = re.sub(rf"\$\{{\{{\s*matrix\.{re.escape(key)}\s*}}}}", value, name)
        else:
            suffix = ", ".join(combo[key] for key in combo)
            name = f"{job.name} ({suffix})" if suffix else job.name
        variant = replace(
            job,
            name=name,
            uses=apply_matrix_context(job.uses, combo),
            with_inputs=apply_matrix_context_mapping(job.with_inputs, combo),
        )
        signature = (variant.name, tuple(sorted(variant.with_inputs.items())), variant.uses)
        if signature in seen:
            continue
        seen.add(signature)
        variants.append(variant)
    return variants or [job]
def apply_matrix_definition(job: WorkflowJob, key: str, tail: str) -> str | None:
    if key in {"include", "exclude"}:
        if tail:
            if tail == "[]":
                return None
            inline_entries = parse_inline_matrix_entry_list(tail)
            if inline_entries is None:
                job.matrix_expandable = False
            elif key == "include":
                job.matrix_include.extend(inline_entries)
            else:
                job.matrix_exclude.extend(inline_entries)
            return None
        return key
    if not key:
        return None
    if tail.startswith("${{"):
        job.matrix_expandable = False
        return None
    inline_items = parse_inline_string_list(tail, allow_empty=True)
    if tail:
        if inline_items:
            job.matrix[key] = [str(item) for item in inline_items]
        elif tail == "[]":
            job.matrix[key] = []
        else:
            job.matrix_expandable = False
        return None
    job.matrix.setdefault(key, [])
    return key

def scan_workflow_call_input_defaults(path: Path) -> dict[str, str]:
    defaults: dict[str, str] = {}
    in_on = False
    on_item_indent: int | None = None
    in_workflow_call = False
    workflow_call_item_indent: int | None = None
    in_inputs = False
    inputs_item_indent: int | None = None
    current_input: str | None = None
    input_field_indent: int | None = None
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        normalized = strip_inline_comment(raw.strip())
        if not normalized:
            continue
        if indent == 0:
            top_level = re.match(r'(["\']?)([^:]+)\1:\s*(.*)$', normalized)
            if top_level and top_level.group(2).strip() == "on":
                tail = top_level.group(3).strip()
                if tail:
                    inline_mapping = parse_inline_mapping(tail)
                    if inline_mapping:
                        workflow_call = inline_mapping.get("workflow_call")
                        if isinstance(workflow_call, str) and workflow_call:
                            workflow_call_mapping = parse_inline_mapping(workflow_call)
                            inputs = workflow_call_mapping.get("inputs") if workflow_call_mapping else None
                            if isinstance(inputs, str) and inputs:
                                inputs_mapping = parse_inline_mapping(inputs)
                                if inputs_mapping:
                                    for input_name, input_spec in inputs_mapping.items():
                                        if not input_name or not input_spec:
                                            continue
                                        input_mapping = parse_inline_mapping(input_spec)
                                        if input_mapping and "default" in input_mapping:
                                            defaults[input_name] = parse_scalar(input_mapping["default"])
                in_on = not tail
                on_item_indent = None
                in_workflow_call = False
                workflow_call_item_indent = None
                in_inputs = False
                inputs_item_indent = None
                current_input = None
                input_field_indent = None
                continue
            in_on = False
            on_item_indent = None
            in_workflow_call = False
            workflow_call_item_indent = None
            in_inputs = False
            inputs_item_indent = None
            current_input = None
            input_field_indent = None
            continue
        if not in_on:
            continue
        if on_item_indent is None:
            if indent <= 0:
                continue
            on_item_indent = indent
        if indent == on_item_indent:
            key, tail = split_mapping_line(normalized)
            if key == "workflow_call":
                # Support `workflow_call: { inputs: ... }` in addition to block-style maps.
                if tail:
                    workflow_call_mapping = parse_inline_mapping(tail)
                    inputs = workflow_call_mapping.get("inputs") if workflow_call_mapping else None
                    if isinstance(inputs, str) and inputs:
                        inputs_mapping = parse_inline_mapping(inputs)
                        if inputs_mapping:
                            for input_name, input_spec in inputs_mapping.items():
                                if not input_name or not input_spec:
                                    continue
                                input_mapping = parse_inline_mapping(input_spec)
                                if input_mapping and "default" in input_mapping:
                                    defaults[input_name] = parse_scalar(input_mapping["default"])
                    in_workflow_call = False
                    workflow_call_item_indent = None
                    in_inputs = False
                    inputs_item_indent = None
                    current_input = None
                    input_field_indent = None
                    continue
                in_workflow_call = True
                workflow_call_item_indent = None
                in_inputs = False
                inputs_item_indent = None
                current_input = None
                input_field_indent = None
                continue
            in_workflow_call = False
            workflow_call_item_indent = None
            in_inputs = False
            inputs_item_indent = None
            current_input = None
            input_field_indent = None
            continue
        if indent <= on_item_indent:
            in_workflow_call = False
            workflow_call_item_indent = None
            in_inputs = False
            inputs_item_indent = None
            current_input = None
            input_field_indent = None
            continue
        if not in_workflow_call:
            continue
        if workflow_call_item_indent is None:
            workflow_call_item_indent = indent
        if indent <= on_item_indent:
            in_workflow_call = False
            workflow_call_item_indent = None
            in_inputs = False
            inputs_item_indent = None
            current_input = None
            input_field_indent = None
            continue
        if indent == workflow_call_item_indent:
            key, tail = split_mapping_line(normalized)
            if key == "inputs":
                if tail:
                    inputs_mapping = parse_inline_mapping(tail)
                    if inputs_mapping:
                        for input_name, input_spec in inputs_mapping.items():
                            if not input_name or not input_spec:
                                continue
                            input_mapping = parse_inline_mapping(input_spec)
                            if input_mapping and "default" in input_mapping:
                                defaults[input_name] = parse_scalar(input_mapping["default"])
                    in_inputs = False
                    inputs_item_indent = None
                    current_input = None
                    input_field_indent = None
                    continue
                in_inputs = True
                inputs_item_indent = None
                current_input = None
                input_field_indent = None
                continue
            in_inputs = False
            inputs_item_indent = None
            current_input = None
            input_field_indent = None
            continue
        if indent <= workflow_call_item_indent:
            in_inputs = False
            inputs_item_indent = None
            current_input = None
            input_field_indent = None
            continue
        if not in_inputs:
            continue
        if inputs_item_indent is None:
            inputs_item_indent = indent
        if indent == inputs_item_indent and ":" in normalized:
            key, tail = normalized.split(":", 1)
            current_input = None
            input_field_indent = None
            input_name = parse_scalar(key.strip())
            tail = tail.strip()
            if input_name:
                if not tail:
                    current_input = input_name
                else:
                    inline_mapping = parse_inline_mapping(tail)
                    if inline_mapping and "default" in inline_mapping:
                        defaults[input_name] = parse_scalar(inline_mapping["default"])
            continue
        if current_input:
            if indent <= inputs_item_indent:
                current_input = None
                input_field_indent = None
                continue
            if input_field_indent is None:
                input_field_indent = indent
            if indent == input_field_indent:
                key, tail = split_mapping_line(normalized)
                if key == "default":
                    defaults[current_input] = parse_scalar(tail)
    return defaults

def scan_workflows(repo_root: Path) -> list[WorkflowInventory]:
    workflows_dir = repo_root / ".github" / "workflows"
    if not workflows_dir.exists():
        return []
    specs: dict[str, WorkflowSpec] = {}
    workflow_call_defaults: dict[str, dict[str, str]] = {}
    for path in sorted(list(workflows_dir.glob("*.yml")) + list(workflows_dir.glob("*.yaml"))):
        lines = read_yaml_lines(path)
        workflow_name = path.stem
        in_jobs = False
        job_indent: int | None = None
        in_strategy = False
        in_matrix = False
        current_matrix_key: str | None = None
        current_matrix_entry: dict[str, str] | None = None
        current_job: WorkflowJob | None = None
        job_key_indent: int | None = None
        job_step: int | None = None
        in_with = False
        jobs: list[WorkflowJob] = []
        for raw in lines:
            if not raw.strip() or raw.lstrip().startswith("#"):
                continue
            indent = len(raw) - len(raw.lstrip(" "))
            stripped = raw.strip()
            normalized = strip_inline_comment(stripped)
            if not normalized:
                continue
            if indent == 0:
                top_key, top_tail = split_mapping_line(normalized)
                if top_key == "name":
                    workflow_name = parse_scalar(top_tail) or workflow_name
                    continue
                if top_key == "jobs":
                    # Jobs can be defined as a block (`jobs:` followed by job ids) or as a flow
                    # mapping (`jobs: { lint: { name: Lint, ... } }`). Support both forms.
                    if top_tail:
                        inline_jobs = parse_inline_mapping(top_tail)
                        if inline_jobs is not None:
                            for inline_job_id, inline_job_spec in inline_jobs.items():
                                if not inline_job_id:
                                    continue
                                current_job = WorkflowJob(job_id=inline_job_id, name=inline_job_id)
                                jobs.append(current_job)
                                inline_job = parse_inline_mapping(inline_job_spec) if inline_job_spec else None
                                if inline_job is None:
                                    continue
                                name = inline_job.get("name")
                                if name:
                                    current_job.name = name or current_job.name
                                uses = inline_job.get("uses")
                                if uses:
                                    current_job.uses = uses
                                raw_if = inline_job.get("if")
                                if raw_if:
                                    current_job.if_condition = raw_if
                                raw_with = inline_job.get("with")
                                if raw_with:
                                    inline_with = parse_inline_mapping(raw_with)
                                    if inline_with is not None:
                                        current_job.with_inputs = inline_with
                                raw_strategy = inline_job.get("strategy")
                                if raw_strategy:
                                    inline_strategy = parse_inline_mapping(raw_strategy)
                                    if inline_strategy and "matrix" in inline_strategy:
                                        current_job.has_matrix = True
                                        inline_matrix = parse_inline_mapping(inline_strategy["matrix"])
                                        if inline_matrix is None:
                                            current_job.matrix_expandable = False
                                        else:
                                            for key, raw_value in inline_matrix.items():
                                                apply_matrix_definition(current_job, key, raw_value.strip())
                                    else:
                                        current_job.matrix_expandable = False
                        in_jobs = False
                        job_indent = None
                        current_job = None
                        job_key_indent = None
                        job_step = None
                    else:
                        in_jobs = True
                        job_indent = None
                        current_job = None
                        job_key_indent = None
                        job_step = None
                    in_strategy = False
                    in_matrix = False
                    in_with = False
                    current_matrix_key = None
                    current_matrix_entry = None
                    continue
                if in_jobs and top_key and top_key != "jobs":
                    in_jobs = False
                    job_indent = None
                    current_job = None
                    job_key_indent = None
                    job_step = None
                    in_strategy = False
                    in_matrix = False
                    in_with = False
                    current_matrix_key = None
                    current_matrix_entry = None
            if not in_jobs:
                continue
            if job_indent is None:
                if indent <= 0:
                    continue
                if normalized.startswith("- "):
                    continue
                if ":" not in normalized:
                    continue
                job_indent = indent
            if indent == job_indent:
                # Jobs can be declared in block style (`lint:`) or flow style
                # (`lint: { name: Lint, runs-on: ... }`). Handle both so we don't
                # incorrectly report drift when repositories use inline job maps.
                job_id, job_tail = split_mapping_line(normalized)
                if job_id:
                    current_job = WorkflowJob(job_id=job_id, name=job_id)
                    jobs.append(current_job)
                    in_strategy = False
                    in_matrix = False
                    in_with = False
                    current_matrix_key = None
                    current_matrix_entry = None
                    job_key_indent = None
                    job_step = None

                    if job_tail:
                        inline_job = parse_inline_mapping(job_tail)
                        if inline_job is not None:
                            name = inline_job.get("name")
                            if name:
                                current_job.name = name or current_job.name
                            uses = inline_job.get("uses")
                            if uses:
                                current_job.uses = uses
                            raw_if = inline_job.get("if")
                            if raw_if:
                                current_job.if_condition = raw_if
                            raw_with = inline_job.get("with")
                            if raw_with:
                                inline_with = parse_inline_mapping(raw_with)
                                if inline_with is not None:
                                    current_job.with_inputs = inline_with
                            raw_strategy = inline_job.get("strategy")
                            if raw_strategy:
                                inline_strategy = parse_inline_mapping(raw_strategy)
                                if inline_strategy and "matrix" in inline_strategy:
                                    current_job.has_matrix = True
                                    inline_matrix = parse_inline_mapping(inline_strategy["matrix"])
                                    if inline_matrix is None:
                                        current_job.matrix_expandable = False
                                    else:
                                        for key, raw_value in inline_matrix.items():
                                            apply_matrix_definition(current_job, key, raw_value.strip())
                                else:
                                    current_job.matrix_expandable = False
                    continue
            if current_job is None:
                continue
            if job_key_indent is None:
                if indent <= job_indent:
                    continue
                if normalized.startswith("- "):
                    continue
                if ":" not in normalized:
                    continue
                job_key_indent = indent
                job_step = job_key_indent - job_indent
                if job_step <= 0:
                    job_key_indent = None
                    job_step = None
                    continue
            if job_step is None:
                continue
            key_indent = job_key_indent
            sub_indent = key_indent + job_step
            matrix_key_indent = key_indent + job_step * 2
            matrix_list_indent = key_indent + job_step * 3
            matrix_list_field_indent = key_indent + job_step * 4
            if indent <= key_indent:
                reset_key, _reset_tail = split_mapping_line(normalized)
                if reset_key != "strategy":
                    in_strategy = False
                    in_matrix = False
                    current_matrix_key = None
                    current_matrix_entry = None
                if reset_key != "with":
                    in_with = False
            if indent == key_indent:
                key, tail = split_mapping_line(normalized)
                if key == "name":
                    current_job.name = parse_scalar(tail) or current_job.name
                    continue
                if key == "uses":
                    current_job.uses = parse_scalar(tail)
                    continue
                if key == "if":
                    current_job.if_condition = parse_scalar(tail)
                    continue
                if key == "with":
                    current_job.with_inputs = {}
                    in_with = True
                    if tail:
                        inline_mapping = parse_inline_mapping(tail)
                        if inline_mapping is None:
                            in_with = False
                        else:
                            current_job.with_inputs = inline_mapping
                            in_with = False
                    continue
            if in_with and indent == sub_indent and ":" in normalized:
                key, value = normalized.split(":", 1)
                parsed_key = parse_scalar(key.strip())
                parsed_value = parse_scalar(value)
                # Preserve empty-string inputs. An explicit `suffix: ""` is meaningful for
                # workflow_call placeholders and must not be dropped as falsy.
                if parsed_key and parsed_value is not None:
                    current_job.with_inputs[parsed_key] = parsed_value
                continue
            if indent == key_indent:
                key, tail = split_mapping_line(normalized)
                if key != "strategy":
                    continue
                in_strategy = not tail
                in_matrix = False
                current_matrix_key = None
                current_matrix_entry = None
                if tail:
                    inline_strategy = parse_inline_mapping(tail)
                    if inline_strategy and "matrix" in inline_strategy:
                        current_job.has_matrix = True
                        inline_matrix = parse_inline_mapping(inline_strategy["matrix"])
                        if inline_matrix is None:
                            current_job.matrix_expandable = False
                        else:
                            for key, raw_value in inline_matrix.items():
                                apply_matrix_definition(current_job, key, raw_value.strip())
                    else:
                        current_job.matrix_expandable = False
                    in_strategy = False
                continue
            if not in_strategy:
                continue
            if indent == sub_indent:
                key, tail = split_mapping_line(normalized)
                if key != "matrix":
                    continue
                current_job.has_matrix = True
                in_matrix = True
                current_matrix_key = None
                current_matrix_entry = None
                if tail:
                    inline_mapping = parse_inline_mapping(tail)
                    if inline_mapping is None:
                        current_job.matrix_expandable = False
                        in_matrix = False
                    else:
                        for key, raw_value in inline_mapping.items():
                            maybe_key = apply_matrix_definition(current_job, key, raw_value.strip())
                            if maybe_key in {"include", "exclude"}:
                                current_matrix_key = maybe_key
                        if current_matrix_key not in {"include", "exclude"}:
                            in_matrix = False
                            current_matrix_key = None
                    continue
                continue
            if in_matrix and indent <= sub_indent:
                in_matrix = False
                current_matrix_key = None
                current_matrix_entry = None
            if not in_matrix:
                continue
            if indent == matrix_key_indent and ":" in normalized:
                key, tail = normalized.split(":", 1)
                current_matrix_entry = None
                current_matrix_key = apply_matrix_definition(current_job, key.strip(), tail.strip())
                continue
            if indent == matrix_list_indent and normalized.startswith("- "):
                if current_matrix_key in {"include", "exclude"}:
                    current_matrix_entry = {}
                    item = normalized[2:].strip()
                    if item:
                        inline_mapping = parse_inline_mapping(item)
                        if inline_mapping is not None:
                            current_matrix_entry.update(inline_mapping)
                        else:
                            if ":" not in item:
                                current_job.matrix_expandable = False
                                current_matrix_entry = None
                                continue
                            item_key, raw_value = item.split(":", 1)
                            item_key = item_key.strip()
                            raw_value_token = strip_inline_comment(raw_value).strip()
                            if not item_key or raw_value_token == "":
                                current_job.matrix_expandable = False
                                current_matrix_entry = None
                                continue
                            item_value = parse_scalar(raw_value)
                            if item_value == "" and raw_value_token not in {'""', "''"}:
                                current_job.matrix_expandable = False
                                current_matrix_entry = None
                                continue
                            current_matrix_entry[item_key] = item_value
                    if current_matrix_key == "include":
                        current_job.matrix_include.append(current_matrix_entry)
                    else:
                        current_job.matrix_exclude.append(current_matrix_entry)
                    continue
                if current_matrix_key is None:
                    current_job.matrix_expandable = False
                    continue
                current_job.matrix.setdefault(current_matrix_key, []).append(parse_scalar(normalized[2:]))
                continue
            if indent >= matrix_list_field_indent and ":" in normalized and current_matrix_key in {"include", "exclude"}:
                if current_matrix_entry is None:
                    current_job.matrix_expandable = False
                    continue
                item_key, raw_value = normalized.split(":", 1)
                item_key = item_key.strip()
                raw_value_token = strip_inline_comment(raw_value).strip()
                if not item_key or raw_value_token == "":
                    current_job.matrix_expandable = False
                    continue
                item_value = parse_scalar(raw_value)
                if item_value == "" and raw_value_token not in {'""', "''"}:
                    current_job.matrix_expandable = False
                    continue
                current_matrix_entry[item_key] = item_value
                continue
            if indent >= matrix_list_indent and ":" in normalized:
                current_job.matrix_expandable = False
        rel_path = str(path.relative_to(repo_root))
        specs[rel_path] = WorkflowSpec(path=rel_path, workflow=workflow_name, jobs=jobs)
        workflow_call_defaults[rel_path] = scan_workflow_call_input_defaults(path)
    resolved_checks: dict[tuple[str, tuple[tuple[str, str], ...]], list[str]] = {}
    resolved_remote_reusable_jobs: dict[tuple[str, tuple[tuple[str, str], ...]], list[str]] = {}
    resolved_unresolved_matrix_jobs: dict[tuple[str, tuple[tuple[str, str], ...]], list[str]] = {}
    def resolve_checks(
        spec_path: str,
        input_values: dict[str, str] | None = None,
        stack: tuple[tuple[str, tuple[tuple[str, str], ...]], ...] = (),
    ) -> list[str]:
        input_key = normalize_input_values(input_values)
        cache_key = (spec_path, input_key)
        cached = resolved_checks.get(cache_key)
        if cached is not None:
            return cached
        if cache_key in stack:
            return []
        spec = specs[spec_path]
        resolved_inputs = dict(input_key)
        checks: list[str] = []
        remote_reusable_jobs: list[str] = []
        unresolved_matrix_jobs: list[str] = []
        next_stack = stack + (cache_key,)
        for raw_job in spec.jobs:
            job = apply_inputs_to_job(raw_job, resolved_inputs)
            if job_condition_is_always_false(job.if_condition):
                continue
            if job.has_matrix and not job.matrix_expandable:
                unresolved_matrix_jobs.append(job.name)
                continue
            for variant in expand_matrix_job_variants(job):
                reusable_path = local_reusable_workflow_ref(variant.uses)
                if reusable_path:
                    # Fail closed for local reusable workflows: if we cannot resolve the local
                    # workflow into leaf check names (due to a missing file, missing jobs, or an
                    # unresolvable chain), do not treat the caller job name as a valid check.
                    if reusable_path not in specs:
                        continue
                    child_inputs = dict(workflow_call_defaults.get(reusable_path, {}))
                    child_inputs.update(variant.with_inputs)
                    child_inputs = apply_input_context_mapping(child_inputs, child_inputs)
                    child_input_key = normalize_input_values(child_inputs)
                    child_checks = resolve_checks(reusable_path, child_inputs, next_stack)
                    child_cache_key = (reusable_path, child_input_key)
                    child_remote_reusable_jobs = resolved_remote_reusable_jobs.get(child_cache_key, [])
                    child_unresolved_matrix_jobs = resolved_unresolved_matrix_jobs.get(child_cache_key, [])
                    if child_checks:
                        checks.extend(f"{variant.name} / {child_check}" for child_check in child_checks)
                    if child_remote_reusable_jobs:
                        remote_reusable_jobs.extend(f"{variant.name} / {child_job}" for child_job in child_remote_reusable_jobs)
                    if child_unresolved_matrix_jobs:
                        unresolved_matrix_jobs.extend(f"{variant.name} / {child_job}" for child_job in child_unresolved_matrix_jobs)
                    if child_checks or child_remote_reusable_jobs or child_unresolved_matrix_jobs:
                        continue
                    continue
                if is_remote_reusable_workflow_ref(variant.uses):
                    remote_reusable_jobs.append(variant.name)
                    continue
                if WORKFLOW_EXPRESSION_RE.search(variant.name):
                    unresolved_matrix_jobs.append(variant.name)
                    continue
                checks.append(variant.name)
        # Preserve raw occurrences so we can detect duplicate emitted check contexts.
        resolved_checks[cache_key] = list(checks)
        resolved_remote_reusable_jobs[cache_key] = dedupe_preserve_order(remote_reusable_jobs)
        resolved_unresolved_matrix_jobs[cache_key] = dedupe_preserve_order(unresolved_matrix_jobs)
        return resolved_checks[cache_key]
    def root_cache_key(spec_path: str) -> tuple[str, tuple[tuple[str, str], ...]]:
        return (spec_path, normalize_input_values(None))
    return [
        WorkflowInventory(
            path=spec.path,
            workflow=spec.workflow,
            jobs=resolve_checks(spec.path),
            remote_reusable_jobs=resolved_remote_reusable_jobs.get(root_cache_key(spec.path), []),
            unresolved_matrix_jobs=resolved_unresolved_matrix_jobs.get(root_cache_key(spec.path), []),
        )
        for spec in specs.values()
    ]
def workflow_lookup(inventory: list[WorkflowInventory]) -> dict[str, set[str]]:
    return {item.workflow: set(item.jobs) for item in inventory}
def remote_reusable_lookup(inventory: list[WorkflowInventory]) -> dict[str, set[str]]:
    return {item.workflow: set(item.remote_reusable_jobs) for item in inventory}
def unresolved_matrix_lookup(inventory: list[WorkflowInventory]) -> dict[str, set[str]]:
    return {item.workflow: set(item.unresolved_matrix_jobs) for item in inventory if item.unresolved_matrix_jobs}
def all_check_names(inventory: list[WorkflowInventory]) -> set[str]:
    names: set[str] = set()
    for item in inventory:
        names.update(item.jobs)
    return names
def duplicated_check_contexts(inventory: list[WorkflowInventory]) -> dict[str, list[str]]:
    """Detect duplicate emitted check contexts in the repository workflow inventory.

    GitHub required checks are keyed by the check context name. If multiple jobs emit the
    same context (either across workflows or within a single workflow), GitHub cannot
    reliably enforce a specific one or disambiguate which check the ruleset refers to.
    Fail closed and require the repository to rename the checks.
    """
    occurrences: dict[str, dict[str, int]] = {}
    for item in inventory:
        label = f"{item.workflow} ({item.path})"
        for check in item.jobs:
            per_label = occurrences.setdefault(check, {})
            per_label[label] = per_label.get(label, 0) + 1

    duplicates: dict[str, list[str]] = {}
    for check, per_label in occurrences.items():
        if len(per_label) == 1 and next(iter(per_label.values())) == 1:
            continue
        rendered: list[str] = []
        for label, count in sorted(per_label.items()):
            rendered.append(f"{label} x{count}" if count > 1 else label)
        duplicates[check] = rendered
    return duplicates
def worsen_exit_code(current: int, candidate: int) -> int:
    return candidate if candidate > current else current
def normalize_env_value(value: Any) -> Any:
    if value is None:
        return None
    if isinstance(value, list):
        if all(isinstance(item, str) for item in value):
            return sorted(item.strip() for item in value)
        return value
    if isinstance(value, str):
        stripped = value.strip()
        lowered = stripped.lower()
        if lowered in {"true", "false"}:
            return lowered == "true"
        if stripped.startswith("["):
            candidates = [stripped]
            # Lightweight YAML parsing means we may see escaped JSON strings such as:
            #   "[\"admin\", \"maintain\"]"
            # Strip the backslash escapes and try again before giving up.
            if '\\"' in stripped:
                candidates.append(stripped.replace('\\"', '"'))
            data = None
            for candidate in candidates:
                try:
                    data = json.loads(candidate)
                except json.JSONDecodeError:
                    continue
                break
            if data is None:
                return stripped
            return normalize_env_value(data)
        return stripped
    return value

def normalize_mapping_values(value: dict[str, Any]) -> dict[str, Any]:
    return {
        key: normalize_env_value(raw_value)
        for key, raw_value in sorted(value.items())
        if normalize_env_value(raw_value) is not None
    }

def scan_top_level_mapping_anchors(path: Path) -> dict[str, dict[str, str]]:
    """Collect top-level mapping anchors like `x-perms: &ro` for alias resolution.

    The validator is intentionally a lightweight YAML scanner, but repositories often
    use extension keys (`x-...`) to define anchors and then reference them via `*alias`.
    """
    anchors: dict[str, dict[str, str]] = {}
    in_anchor = False
    anchor_indent: int | None = None
    current_anchor_name: str | None = None
    current_mapping: dict[str, str] = {}
    def flush() -> None:
        nonlocal in_anchor, anchor_indent, current_anchor_name, current_mapping
        if current_anchor_name:
            anchors[current_anchor_name] = dict(current_mapping)
        in_anchor = False
        anchor_indent = None
        current_anchor_name = None
        current_mapping = {}

    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        normalized = strip_inline_comment(raw.strip())
        if not normalized:
            continue
        if indent == 0:
            if in_anchor:
                flush()
            key, tail = split_mapping_line(normalized)
            if key is None or not tail.startswith("&"):
                continue
            anchor_token = tail[1:].strip()
            anchor_name, _sep, remainder = anchor_token.partition(" ")
            remainder = remainder.strip()
            if not anchor_name:
                continue
            current_anchor_name = anchor_name
            current_mapping = {}
            if remainder:
                inline_mapping = parse_inline_mapping(remainder)
                if inline_mapping is not None:
                    anchors[anchor_name] = dict(inline_mapping)
                    current_anchor_name = None
                    current_mapping = {}
                else:
                    in_anchor = True
                    anchor_indent = None
            else:
                in_anchor = True
                anchor_indent = None
            continue
        if in_anchor and ":" in normalized:
            if anchor_indent is None:
                if indent > 0:
                    anchor_indent = indent
            if anchor_indent is not None and indent == anchor_indent:
                key, value = normalized.split(":", 1)
                parsed_key = parse_scalar(key.strip())
                if parsed_key:
                    current_mapping[parsed_key] = parse_scalar(value)
    else:
        if in_anchor:
            flush()
    return anchors

def scan_top_level_env(path: Path) -> dict[str, str]:
    env: dict[str, str] = {}
    anchors = scan_top_level_mapping_anchors(path)
    in_env = False
    env_indent: int | None = None
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        stripped = raw.strip()
        normalized = strip_inline_comment(stripped)
        if not normalized:
            continue
        if indent == 0:
            key, tail = split_mapping_line(normalized)
            if key != "env":
                if in_env:
                    break
                continue
            if tail:
                if tail.startswith("*"):
                    alias = tail[1:].strip()
                    env = dict(anchors.get(alias, {}))
                    in_env = False
                    env_indent = None
                    continue
                # Support YAML anchors like `env: &review_env` by treating them as block mappings.
                if tail.startswith("&"):
                    _anchor, _sep, remainder = tail[1:].strip().partition(" ")
                    remainder = remainder.strip()
                    env = {}
                    if remainder:
                        inline_mapping = parse_inline_mapping(remainder)
                        if inline_mapping is not None:
                            env.update(inline_mapping)
                            in_env = False
                            env_indent = None
                        else:
                            in_env = True
                            env_indent = None
                    else:
                        in_env = True
                        env_indent = None
                    continue
                inline_mapping = parse_inline_mapping(tail)
                if inline_mapping is not None:
                    env.update(inline_mapping)
                in_env = False
                env_indent = None
            else:
                in_env = True
                env_indent = None
            continue
        if in_env and ":" in normalized:
            if env_indent is None:
                if indent <= 0:
                    continue
                env_indent = indent
            if indent != env_indent:
                continue
            key, value = normalized.split(":", 1)
            parsed_key = parse_scalar(key.strip())
            if parsed_key:
                env[parsed_key] = parse_scalar(value)
    return env
def scan_top_level_permissions(path: Path) -> dict[str, str]:
    permissions: dict[str, str] = {}
    anchors = scan_top_level_mapping_anchors(path)
    in_permissions = False
    permissions_indent: int | None = None
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        stripped = raw.strip()
        normalized = strip_inline_comment(stripped)
        if not normalized:
            continue
        if indent == 0:
            key, tail = split_mapping_line(normalized)
            if key != "permissions":
                if in_permissions:
                    break
                continue
            if tail:
                if tail.startswith("*"):
                    alias = tail[1:].strip()
                    permissions = dict(anchors.get(alias, {}))
                    in_permissions = False
                    permissions_indent = None
                    continue
                # Support YAML anchors like `permissions: &ro` by treating them as block mappings.
                if tail.startswith("&"):
                    _anchor, _sep, remainder = tail[1:].strip().partition(" ")
                    remainder = remainder.strip()
                    permissions = {}
                    if remainder:
                        inline_mapping = parse_inline_mapping(remainder)
                        if inline_mapping is not None:
                            permissions = dict(inline_mapping)
                            in_permissions = False
                            permissions_indent = None
                        else:
                            in_permissions = True
                            permissions_indent = None
                    else:
                        in_permissions = True
                        permissions_indent = None
                    continue
                inline_mapping = parse_inline_mapping(tail)
                if inline_mapping is not None:
                    permissions = dict(inline_mapping)
                in_permissions = False
                permissions_indent = None
            else:
                permissions = {}
                in_permissions = True
                permissions_indent = None
            continue
        if in_permissions and ":" in normalized:
            if permissions_indent is None:
                if indent <= 0:
                    continue
                permissions_indent = indent
            if indent != permissions_indent:
                continue
            key, value = normalized.split(":", 1)
            parsed_key = parse_scalar(key.strip())
            if parsed_key:
                permissions[parsed_key] = parse_scalar(value)
    return permissions
def scan_job_permissions(path: Path) -> dict[str, dict[str, str]]:
    permissions_by_job: dict[str, dict[str, str]] = {}
    in_jobs = False
    in_permissions = False
    permission_anchors: dict[str, dict[str, str]] = scan_top_level_mapping_anchors(path)
    in_permission_anchor = False
    anchor_indent: int | None = None
    current_anchor_name: str | None = None
    current_anchor_permissions: dict[str, str] = {}
    current_job_id: str | None = None
    current_job_name: str | None = None
    current_permissions: dict[str, str] = {}
    current_permissions_declared = False
    job_indent: int | None = None
    job_key_indent: int | None = None
    job_step: int | None = None
    permissions_indent: int | None = None
    def flush_anchor() -> None:
        nonlocal in_permission_anchor, current_anchor_name, current_anchor_permissions
        if current_anchor_name:
            permission_anchors[current_anchor_name] = dict(current_anchor_permissions)
        in_permission_anchor = False
        nonlocal anchor_indent
        anchor_indent = None
        current_anchor_name = None
        current_anchor_permissions = {}
    def flush() -> None:
        if current_job_name and current_permissions_declared:
            permissions_by_job[current_job_name] = dict(current_permissions)
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        stripped = raw.strip()
        normalized = strip_inline_comment(stripped)
        if not normalized:
            continue
        if in_permission_anchor:
            if ":" in normalized:
                if anchor_indent is None:
                    if indent > 0:
                        anchor_indent = indent
                if anchor_indent is not None and indent == anchor_indent:
                    anchor_key, anchor_value = normalized.split(":", 1)
                    parsed_key = parse_scalar(anchor_key.strip())
                    if parsed_key:
                        current_anchor_permissions[parsed_key] = parse_scalar(anchor_value)
                    continue
            if indent == 0:
                flush_anchor()
        if indent == 0:
            key, tail = split_mapping_line(normalized)
            if key == "permissions" and tail.startswith("&"):
                anchor_token = tail[1:].strip()
                anchor_name, _sep, remainder = anchor_token.partition(" ")
                remainder = remainder.strip()
                if anchor_name:
                    if remainder.startswith("{") and remainder.endswith("}"):
                        inline_mapping = parse_inline_mapping(remainder)
                        if inline_mapping is not None:
                            permission_anchors[anchor_name] = dict(inline_mapping)
                    else:
                        in_permission_anchor = True
                        anchor_indent = None
                        current_anchor_name = anchor_name
                        current_anchor_permissions = {}
                continue
            if key == "jobs":
                if tail:
                    inline_jobs = parse_inline_mapping(tail)
                    if inline_jobs is not None:
                        for job_id, job_spec in inline_jobs.items():
                            if not job_id:
                                continue
                            job_name = job_id
                            inline_job = parse_inline_mapping(job_spec) if job_spec else None
                            if inline_job is not None:
                                name = inline_job.get("name")
                                if name:
                                    job_name = name or job_name
                                if "permissions" in inline_job:
                                    raw_permissions = inline_job.get("permissions", "")
                                    if raw_permissions.startswith("*"):
                                        alias = raw_permissions[1:].strip()
                                        permissions_by_job[job_name] = dict(permission_anchors.get(alias, {}))
                                    else:
                                        inline_permissions = parse_inline_mapping(raw_permissions) if raw_permissions else None
                                        permissions_by_job[job_name] = dict(inline_permissions) if inline_permissions is not None else {}
                    break
                in_jobs = True
                job_indent = None
                current_job_id = None
                current_job_name = None
                current_permissions = {}
                current_permissions_declared = False
                in_permissions = False
                permissions_indent = None
                job_key_indent = None
                job_step = None
                continue
        if in_jobs and indent == 0:
            flush()
            break
        if not in_jobs:
            continue
        if job_indent is None:
            if indent <= 0:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_indent = indent
        if indent == job_indent:
            job_id, job_tail = split_mapping_line(normalized)
            if job_id:
                flush()
                current_job_id = job_id
                current_job_name = job_id
                current_permissions = {}
                current_permissions_declared = False
                in_permissions = False
                permissions_indent = None
                job_key_indent = None
                job_step = None
                if job_tail:
                    inline_job = parse_inline_mapping(job_tail)
                    if inline_job is not None:
                        name = inline_job.get("name")
                        if name:
                            current_job_name = name or current_job_name
                        if "permissions" in inline_job:
                            current_permissions = {}
                            current_permissions_declared = True
                            raw_permissions = inline_job.get("permissions", "")
                            if raw_permissions.startswith("*"):
                                alias = raw_permissions[1:].strip()
                                current_permissions = dict(permission_anchors.get(alias, {}))
                            else:
                                inline_permissions = parse_inline_mapping(raw_permissions) if raw_permissions else None
                                if inline_permissions is not None:
                                    current_permissions = dict(inline_permissions)
                continue
        if current_job_id is None:
            continue
        if job_key_indent is None:
            if indent <= job_indent:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_key_indent = indent
            job_step = job_key_indent - job_indent
            if job_step <= 0:
                job_key_indent = None
                job_step = None
                continue
        if job_step is None:
            continue
        if in_permissions and indent <= job_key_indent:
            in_permissions = False
            permissions_indent = None
        if indent == job_key_indent:
            key, tail = split_mapping_line(normalized)
            if key == "name":
                current_job_name = parse_scalar(tail) or current_job_name
                continue
            if key != "permissions":
                continue
            current_permissions = {}
            current_permissions_declared = True
            if tail:
                if tail.startswith("*"):
                    alias = tail[1:].strip()
                    current_permissions = dict(permission_anchors.get(alias, {}))
                    in_permissions = False
                    permissions_indent = None
                else:
                    inline_mapping = parse_inline_mapping(tail)
                    if inline_mapping is not None:
                        current_permissions = dict(inline_mapping)
                    in_permissions = False
                    permissions_indent = None
            else:
                in_permissions = True
                permissions_indent = None
            continue
        if in_permissions and ":" in normalized:
            if permissions_indent is None:
                if indent > job_key_indent:
                    permissions_indent = indent
            if permissions_indent is not None and indent == permissions_indent:
                key, value = normalized.split(":", 1)
                parsed_key = parse_scalar(key.strip())
                if parsed_key:
                    current_permissions[parsed_key] = parse_scalar(value)
            continue
    else:
        if in_permission_anchor:
            flush_anchor()
        flush()
    return permissions_by_job
def scan_job_env(path: Path) -> dict[str, dict[str, str]]:
    env_by_job: dict[str, dict[str, str]] = {}
    in_jobs = False
    in_env = False
    current_job_id: str | None = None
    current_job_name: str | None = None
    current_env: dict[str, str] = {}
    env_anchors: dict[str, dict[str, str]] = scan_top_level_mapping_anchors(path)
    current_env_anchor: str | None = None
    job_indent: int | None = None
    job_key_indent: int | None = None
    job_step: int | None = None
    env_item_indent: int | None = None
    def flush_env_anchor() -> None:
        nonlocal current_env_anchor
        if current_env_anchor:
            env_anchors[current_env_anchor] = dict(current_env)
            current_env_anchor = None
    def flush() -> None:
        if current_job_name and current_env:
            env_by_job[current_job_name] = dict(current_env)
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        stripped = raw.strip()
        normalized = strip_inline_comment(stripped)
        if not normalized:
            continue
        if indent == 0:
            key, tail = split_mapping_line(normalized)
            if key == "jobs":
                if tail:
                    inline_jobs = parse_inline_mapping(tail)
                    if inline_jobs is not None:
                        for job_id, job_spec in inline_jobs.items():
                            if not job_id:
                                continue
                            job_name = job_id
                            inline_job = parse_inline_mapping(job_spec) if job_spec else None
                            if inline_job is None:
                                continue
                            name = inline_job.get("name")
                            if name:
                                job_name = name or job_name
                            raw_env = inline_job.get("env")
                            if raw_env:
                                inline_env = parse_inline_mapping(raw_env)
                                if inline_env:
                                    env_by_job[job_name] = dict(inline_env)
                    break
                in_jobs = True
                job_indent = None
                current_job_id = None
                current_job_name = None
                current_env = {}
                in_env = False
                env_item_indent = None
                job_key_indent = None
                job_step = None
                continue
        if in_jobs and indent == 0:
            flush_env_anchor()
            flush()
            break
        if not in_jobs:
            continue
        if job_indent is None:
            if indent <= 0:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_indent = indent
        if indent == job_indent:
            job_id, job_tail = split_mapping_line(normalized)
            if job_id:
                flush_env_anchor()
                flush()
                current_job_id = job_id
                current_job_name = job_id
                current_env = {}
                in_env = False
                env_item_indent = None
                current_env_anchor = None
                job_key_indent = None
                job_step = None
                if job_tail:
                    inline_job = parse_inline_mapping(job_tail)
                    if inline_job is not None:
                        name = inline_job.get("name")
                        if name:
                            current_job_name = name or current_job_name
                        raw_env = inline_job.get("env")
                        if raw_env:
                            inline_env = parse_inline_mapping(raw_env)
                            if inline_env:
                                current_env.update(inline_env)
                continue
        if current_job_id is None:
            continue
        if job_key_indent is None:
            if indent <= job_indent:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_key_indent = indent
            job_step = job_key_indent - job_indent
            if job_step <= 0:
                job_key_indent = None
                job_step = None
                continue
        if job_step is None:
            continue
        if in_env and indent <= job_key_indent:
            flush_env_anchor()
            in_env = False
            env_item_indent = None
        if indent == job_key_indent:
            key, tail = split_mapping_line(normalized)
            if key == "name":
                current_job_name = parse_scalar(tail) or current_job_name
                continue
            if key != "env":
                continue
            current_env = {}
            current_env_anchor = None
            if tail:
                if tail.startswith("&"):
                    anchor_token = tail[1:].strip()
                    anchor_name, _sep, remainder = anchor_token.partition(" ")
                    remainder = remainder.strip()
                    if anchor_name:
                        current_env_anchor = anchor_name
                    if remainder:
                        inline_mapping = parse_inline_mapping(remainder)
                        if inline_mapping is not None:
                            current_env.update(inline_mapping)
                            flush_env_anchor()
                            in_env = False
                            env_item_indent = None
                        else:
                            in_env = True
                            env_item_indent = None
                    else:
                        in_env = True
                        env_item_indent = None
                    continue
                if tail.startswith("*"):
                    alias = tail[1:].strip()
                    current_env.update(env_anchors.get(alias, {}))
                    in_env = False
                    env_item_indent = None
                    continue
                inline_mapping = parse_inline_mapping(tail)
                if inline_mapping:
                    current_env.update(inline_mapping)
                in_env = False
                env_item_indent = None
            else:
                in_env = True
                env_item_indent = None
            continue
        if in_env and ":" in normalized:
            if env_item_indent is None:
                if indent > job_key_indent:
                    env_item_indent = indent
            if env_item_indent is not None and indent == env_item_indent:
                key, value = normalized.split(":", 1)
                parsed_key = parse_scalar(key.strip())
                if parsed_key:
                    current_env[parsed_key] = parse_scalar(value)
            continue
    else:
        flush_env_anchor()
        flush()
    return env_by_job
def scan_job_blocks(path: Path) -> dict[str, str]:
    blocks: dict[str, str] = {}
    in_jobs = False
    current_job_id: str | None = None
    current_job_name: str | None = None
    current_lines: list[str] = []
    job_indent: int | None = None
    job_key_indent: int | None = None
    job_step: int | None = None
    def flush() -> None:
        if current_job_name and current_lines:
            blocks[current_job_name] = "\n".join(current_lines)
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        stripped = raw.strip()
        normalized = strip_inline_comment(stripped)
        if not normalized:
            continue
        if indent == 0:
            key, tail = split_mapping_line(normalized)
            if key == "jobs":
                if tail:
                    inline_jobs = parse_inline_mapping(tail)
                    if inline_jobs is not None:
                        for job_id, job_spec in inline_jobs.items():
                            if not job_id:
                                continue
                            job_name = job_id
                            inline_job = parse_inline_mapping(job_spec) if job_spec else None
                            if inline_job is not None:
                                name = inline_job.get("name")
                                if name:
                                    job_name = name or job_name
                            blocks[job_name] = f"{job_id}: {job_spec}"
                    break
                in_jobs = True
                job_indent = None
                current_job_id = None
                current_job_name = None
                current_lines = []
                job_key_indent = None
                job_step = None
                continue
        if in_jobs and indent == 0:
            flush()
            break
        if not in_jobs:
            continue
        if job_indent is None:
            if indent <= 0:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_indent = indent
        if indent == job_indent:
            job_id, job_tail = split_mapping_line(normalized)
            if job_id:
                flush()
                current_job_id = job_id
                current_job_name = job_id
                current_lines = [raw]
                job_key_indent = None
                job_step = None
                if job_tail:
                    inline_job = parse_inline_mapping(job_tail)
                    if inline_job is not None:
                        name = inline_job.get("name")
                        if name:
                            current_job_name = name or current_job_name
                continue
        if current_job_id is None:
            continue
        current_lines.append(raw)
        if job_key_indent is None:
            if indent <= job_indent:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_key_indent = indent
            job_step = job_key_indent - job_indent
            if job_step <= 0:
                job_key_indent = None
                job_step = None
                continue
        if job_key_indent is not None and indent == job_key_indent:
            key, tail = split_mapping_line(normalized)
            if key == "name":
                current_job_name = parse_scalar(tail) or current_job_name
    else:
        flush()
    return blocks
def scan_review_policy_runtime_contract(path: Path, check_name: str) -> dict[str, Any]:
    job_block = scan_job_blocks(path).get(check_name, "")
    runtime_block = "\n".join(
        line
        for line in job_block.splitlines()
        if not line.strip().startswith("#") and not line.strip().startswith("//")
    )
    # Review-policy enforcement depends on actions/github-script. Repositories may format the
    # workflow using block style or flow style mappings; a substring match is the most tolerant.
    uses_github_script = "actions/github-script@" in runtime_block
    workflow_permissions = scan_top_level_permissions(path)
    job_permissions_by_name = scan_job_permissions(path)
    job_permissions_declared = check_name in job_permissions_by_name
    job_permissions = job_permissions_by_name.get(check_name, {})
    effective_permissions = job_permissions if job_permissions_declared else workflow_permissions
    return {
        "workflow_permissions": workflow_permissions,
        "job_permissions": job_permissions,
        "permissions": effective_permissions,
        "uses_github_script": uses_github_script,
        "logic_markers": {marker: marker in runtime_block for marker in REQUIRED_REVIEW_POLICY_SCRIPT_MARKERS},
        "forbidden_markers": {marker: marker in runtime_block for marker in FORBIDDEN_REVIEW_POLICY_RUNTIME_MARKERS},
    }
def split_top_level_items(value: str) -> list[str]:
    items: list[str] = []
    current: list[str] = []
    quote: str | None = None
    brace_depth = 0
    bracket_depth = 0
    for char in value:
        if quote is not None:
            current.append(char)
            if char == quote:
                quote = None
            continue
        if char in {'"', "'"}:
            quote = char
            current.append(char)
            continue
        if char == '{':
            brace_depth += 1
            current.append(char)
            continue
        if char == '}':
            brace_depth = max(0, brace_depth - 1)
            current.append(char)
            continue
        if char == '[':
            bracket_depth += 1
            current.append(char)
            continue
        if char == ']':
            bracket_depth = max(0, bracket_depth - 1)
            current.append(char)
            continue
        if char == ',' and brace_depth == 0 and bracket_depth == 0:
            item = ''.join(current).strip()
            if item:
                items.append(item)
            current = []
            continue
        current.append(char)
    item = ''.join(current).strip()
    if item:
        items.append(item)
    return items
def parse_inline_mapping(value: str) -> dict[str, str] | None:
    value = value.strip()
    if not value.startswith('{') or not value.endswith('}'):
        return None
    mapping: dict[str, str] = {}
    for item in split_top_level_items(value[1:-1]):
        if ':' not in item:
            return None
        key, raw_value = item.split(':', 1)
        raw_value_token = strip_inline_comment(raw_value).strip()
        if raw_value_token == "":
            return None
        parsed_key = parse_scalar(key)
        parsed_value = parse_scalar(raw_value)
        if not parsed_key:
            return None
        if parsed_value == "" and raw_value_token not in {'""', "''"}:
            return None
        mapping[parsed_key] = parsed_value
    return mapping
def parse_inline_matrix_entry_list(value: str) -> list[dict[str, str]] | None:
    value = value.strip()
    if not value.startswith('[') or not value.endswith(']'):
        return None
    entries: list[dict[str, str]] = []
    for item in split_top_level_items(value[1:-1]):
        mapping = parse_inline_mapping(item)
        if mapping is None:
            return None
        entries.append(mapping)
    return entries
def parse_inline_string_list(value: str, *, allow_empty: bool = False) -> list[str]:
    value = value.strip()
    if not value:
        return []
    if value.startswith('[') and value.endswith(']'):
        items: list[str] = []
        for raw_item in split_top_level_items(value[1:-1]):
            parsed = parse_scalar(raw_item)
            if parsed == "" and not allow_empty:
                continue
            items.append(parsed)
        return items
    item = parse_scalar(value)
    if item == "" and not allow_empty:
        return []
    return [item]
def record_inline_trigger_types(event_name: str, raw_value: str, event_types: dict[str, set[str]]) -> None:
    inline_mapping = parse_inline_mapping(raw_value)
    if inline_mapping is not None:
        # Inline mapping without `types` (for example `{ branches: [main] }`) is valid and means
        # the workflow triggers for all activity types. Do not record a synthetic "type" entry.
        if "types" in inline_mapping:
            items = parse_inline_string_list(inline_mapping["types"])
            if items:
                event_types.setdefault(event_name, set()).update(items)
        return
    items = parse_inline_string_list(raw_value)
    if items:
        event_types.setdefault(event_name, set()).update(items)

def scan_top_level_triggers(path: Path) -> tuple[set[str], dict[str, set[str]]]:
    events: set[str] = set()
    event_types: dict[str, set[str]] = {}
    in_on = False
    on_item_indent: int | None = None
    current_event: str | None = None
    in_types = False
    event_body_indent: int | None = None
    types_list_indent: int | None = None
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        stripped = raw.strip()
        normalized = strip_inline_comment(stripped)
        if not normalized:
            continue
        if indent == 0:
            top_level = re.match(r'(["\']?)([^:]+)\1:\s*(.*)$', normalized)
            if top_level and top_level.group(2).strip() == "on":
                tail = top_level.group(3).strip()
                if tail:
                    inline_mapping = parse_inline_mapping(tail)
                    if inline_mapping:
                        for event_name, raw_value in inline_mapping.items():
                            event_name = parse_scalar(event_name)
                            if not event_name:
                                continue
                            events.add(event_name)
                            record_inline_trigger_types(event_name, raw_value, event_types)
                    else:
                        events.update(parse_inline_string_list(tail))
                    in_on = False
                else:
                    in_on = True
                    on_item_indent = None
                current_event = None
                in_types = False
                event_body_indent = None
                types_list_indent = None
                continue
            if in_on:
                break
            continue
        if not in_on:
            continue
        if on_item_indent is None:
            if indent <= 0:
                continue
            on_item_indent = indent
        if indent == on_item_indent and normalized.startswith("- "):
            current_event = parse_scalar(normalized[2:])
            if current_event:
                events.add(current_event)
            in_types = False
            event_body_indent = None
            types_list_indent = None
            continue
        if indent == on_item_indent and normalized.endswith(":"):
            current_event = parse_scalar(normalized[:-1].strip())
            if current_event:
                events.add(current_event)
            in_types = False
            event_body_indent = None
            types_list_indent = None
            continue
        if indent == on_item_indent and ":" in normalized:
            current_event, tail = normalized.split(":", 1)
            current_event = parse_scalar(current_event.strip())
            tail = tail.strip()
            if current_event:
                events.add(current_event)
            if current_event and tail:
                record_inline_trigger_types(current_event, tail, event_types)
            in_types = False
            event_body_indent = None
            types_list_indent = None
            continue
        if current_event and indent > on_item_indent:
            if event_body_indent is None:
                if ":" in normalized and not normalized.startswith("- "):
                    event_body_indent = indent
            if event_body_indent is not None and indent == event_body_indent:
                key, tail = split_mapping_line(normalized)
                if key != "types":
                    continue
                items = parse_inline_string_list(tail)
                if items:
                    event_types.setdefault(current_event, set()).update(items)
                    in_types = False
                    types_list_indent = None
                else:
                    event_types.setdefault(current_event, set())
                    in_types = True
                    types_list_indent = None
                continue
        if current_event and in_types:
            if event_body_indent is not None and indent <= event_body_indent:
                in_types = False
                types_list_indent = None
                continue
            if types_list_indent is None:
                if indent > (event_body_indent or on_item_indent):
                    types_list_indent = indent
            if types_list_indent is not None and indent == types_list_indent and normalized.startswith("- "):
                item = parse_scalar(normalized[2:])
                if item:
                    event_types.setdefault(current_event, set()).add(item)
                continue
    return events, event_types
def scan_job_names(path: Path) -> dict[str, str]:
    jobs: dict[str, str] = {}
    in_jobs = False
    job_indent: int | None = None
    current_job: str | None = None
    job_key_indent: int | None = None
    job_step: int | None = None
    for raw in read_yaml_lines(path):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        stripped = raw.strip()
        normalized = strip_inline_comment(stripped)
        if not normalized:
            continue
        if indent == 0:
            key, tail = split_mapping_line(normalized)
            if key == "jobs":
                if tail:
                    inline_jobs = parse_inline_mapping(tail)
                    if inline_jobs is not None:
                        for job_id, job_spec in inline_jobs.items():
                            if not job_id:
                                continue
                            job_name = job_id
                            inline_job = parse_inline_mapping(job_spec) if job_spec else None
                            if inline_job is not None:
                                name = inline_job.get("name")
                                if name:
                                    job_name = name or job_name
                            jobs[job_id] = job_name
                    break
                in_jobs = True
                job_indent = None
                current_job = None
                job_key_indent = None
                job_step = None
                continue
        if in_jobs and indent == 0:
            break
        if not in_jobs:
            continue
        if job_indent is None:
            if indent <= 0:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_indent = indent
        if indent == job_indent:
            job_id, job_tail = split_mapping_line(normalized)
            if job_id:
                current_job = job_id
                jobs[job_id] = job_id
                job_key_indent = None
                job_step = None
                if job_tail:
                    inline_job = parse_inline_mapping(job_tail)
                    if inline_job is not None:
                        name = inline_job.get("name")
                        if name:
                            jobs[job_id] = name or jobs[job_id]
                continue
        if current_job is None:
            continue
        if job_key_indent is None:
            if indent <= job_indent:
                continue
            if normalized.startswith("- "):
                continue
            if ":" not in normalized:
                continue
            job_key_indent = indent
            job_step = job_key_indent - job_indent
            if job_step <= 0:
                job_key_indent = None
                job_step = None
                continue
        if job_key_indent is not None and indent == job_key_indent:
            key, tail = split_mapping_line(normalized)
            if key == "name":
                jobs[current_job] = parse_scalar(tail) or jobs[current_job]
    return jobs
def expected_workflow_shapes(data: dict[str, Any]) -> tuple[set[str], set[str], set[str]]:
    workflow_names: set[str] = set()
    job_names: set[str] = set()
    check_names: set[str] = set()
    expected_pr_workflows = data.get("expected_pr_workflows") if isinstance(data.get("expected_pr_workflows"), list) else []
    for item in expected_pr_workflows:
        if not isinstance(item, dict):
            continue
        workflow_name = item.get("workflow")
        jobs = item.get("jobs") if isinstance(item.get("jobs"), list) else []
        if not isinstance(workflow_name, str) or not workflow_name:
            continue
        workflow_names.add(workflow_name)
        for job in jobs:
            if isinstance(job, str) and job:
                job_names.add(job)
                check_names.add(job)
    return workflow_names, job_names, check_names
def declared_check_names(data: dict[str, Any]) -> set[str]:
    names: set[str] = set()
    for key in ("required_checks", "informational_checks"):
        values = data.get(key) if isinstance(data.get(key), list) else []
        for item in values:
            if isinstance(item, str) and item:
                names.add(item)
    return names
def expected_workflow_checks_missing_declaration(
    expected_workflow_checks: set[str],
    declared_checks: set[str],
) -> list[str]:
    return sorted(check for check in expected_workflow_checks if check not in declared_checks)
def expected_check_workflows(data: dict[str, Any]) -> dict[str, set[str]]:
    mapping: dict[str, set[str]] = {}
    expected_pr_workflows = data.get("expected_pr_workflows") if isinstance(data.get("expected_pr_workflows"), list) else []
    for item in expected_pr_workflows:
        if not isinstance(item, dict):
            continue
        workflow_name = item.get("workflow")
        jobs = item.get("jobs") if isinstance(item.get("jobs"), list) else []
        if not isinstance(workflow_name, str) or not workflow_name:
            continue
        for job in jobs:
            if isinstance(job, str) and job:
                mapping.setdefault(job, set()).add(workflow_name)
    return mapping
def remote_reusable_job_matches(check_name: str, remote_reusable_jobs: set[str]) -> bool:
    reusable_prefix, sep, _leaf = check_name.rpartition(" / ")
    return bool(sep) and reusable_prefix in remote_reusable_jobs

def matches_remote_reusable_check(
    check_name: str,
    remote_reusable_jobs: set[str],
    github_checks: set[str] | None = None,
) -> bool:
    if not remote_reusable_job_matches(check_name, remote_reusable_jobs):
        return False
    if github_checks is None:
        return False
    return check_name in github_checks

def unresolved_matrix_job_matches(check_name: str, job_name: str) -> bool:
    if check_name == job_name:
        return True
    placeholder_pattern = re.escape(job_name)
    placeholder_pattern = re.sub(r"\\\$\\\{\\\{.*?\\\}\\\}", r".+?", placeholder_pattern)
    if placeholder_pattern != re.escape(job_name) and re.fullmatch(placeholder_pattern, check_name):
        return True
    stem = re.sub(r"\$\{\{.*?\}\}", "", job_name)
    stem = re.sub(r"\(\s*\)", "", stem)
    stem = re.sub(r"\s+", " ", stem).strip().strip("-/:, ")
    if not stem:
        return False
    return (
        check_name == stem
        or check_name.startswith(f"{stem} (")
    )

def matches_unresolved_matrix_check(
    check_name: str,
    unresolved_matrix_jobs: set[str],
    github_checks: set[str] | None = None,
) -> bool:
    if not any(unresolved_matrix_job_matches(check_name, job_name) for job_name in unresolved_matrix_jobs):
        return False
    if github_checks is None:
        return True
    return check_name in github_checks

def matches_github_aligned_check(
    check_name: str,
    remote_reusable_jobs: set[str],
    unresolved_matrix_jobs: set[str],
    github_checks: set[str] | None,
) -> bool:
    if github_checks is None:
        return False
    return matches_remote_reusable_check(check_name, remote_reusable_jobs, github_checks) or matches_unresolved_matrix_check(check_name, unresolved_matrix_jobs, github_checks)
def required_checks_missing_expected_mapping(
    required_checks: list[str],
    expected_workflow_checks: set[str],
    inventory_check_names: set[str],
    remote_reusable_jobs_by_workflow: dict[str, set[str]],
    unresolved_matrix_jobs_by_workflow: dict[str, set[str]],
) -> list[str]:
    missing: set[str] = set()
    for check in required_checks:
        if check in expected_workflow_checks:
            continue
        if check in inventory_check_names:
            missing.add(check)
            continue
        # Remote reusable leaf checks and unresolved/dynamic matrix jobs are still workflow-backed
        # even though local scanning cannot prove the final leaf name. Require explicit mapping in
        # expected_pr_workflows when the repo workflow inventory indicates such a check could exist.
        if any(remote_reusable_job_matches(check, jobs) for jobs in remote_reusable_jobs_by_workflow.values()):
            missing.add(check)
            continue
        if any(matches_unresolved_matrix_check(check, jobs) for jobs in unresolved_matrix_jobs_by_workflow.values()):
            missing.add(check)
    return sorted(missing)
def is_utc_timestamp(value: Any) -> bool:
    if not isinstance(value, str) or not value:
        return False
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return False
    offset = parsed.utcoffset()
    if offset is None:
        return False
    return offset.total_seconds() == 0
def validate_waivers(waivers: list[Any]) -> list[str]:
    errors: list[str] = []
    for index, waiver in enumerate(waivers):
        prefix = f"waivers[{index}]"
        if not isinstance(waiver, dict):
            errors.append(f"{prefix} must be an object")
            continue
        for key in ("check_name", "scope", "reason", "approved_by"):
            value = waiver.get(key)
            if not isinstance(value, str) or not value:
                errors.append(f"{prefix}.{key} must be a non-empty string")
        if not is_utc_timestamp(waiver.get("approved_at")):
            errors.append(f"{prefix}.approved_at must be an ISO-8601 UTC timestamp")
        expires_at = waiver.get("expires_at")
        if expires_at is not None and not is_utc_timestamp(expires_at):
            errors.append(f"{prefix}.expires_at must be an ISO-8601 UTC timestamp when set")
        ticket = waiver.get("ticket")
        if ticket is not None and (not isinstance(ticket, str) or not ticket):
            errors.append(f"{prefix}.ticket must be a non-empty string when set")
    return errors
def review_policy_workflow_names(data: dict[str, Any]) -> set[str]:
    names = set(REVIEW_POLICY_WORKFLOW_NAMES)
    expected = expected_review_policy_env(data)
    if expected is None:
        return names
    check_name = expected.get("REVIEW_POLICY_CHECK_NAME")
    if not isinstance(check_name, str) or not check_name:
        return names
    for item in data.get("expected_pr_workflows", []):
        if not isinstance(item, dict):
            continue
        workflow_name = item.get("workflow")
        jobs = item.get("jobs") if isinstance(item.get("jobs"), list) else []
        if isinstance(workflow_name, str) and any(
            isinstance(job, str) and job == check_name
            for job in jobs
        ):
            names.add(workflow_name)
    return names


def pick_unique_review_policy_candidate(candidates: list[WorkflowInventory]) -> WorkflowInventory | None:
    if len(candidates) == 1:
        return candidates[0]
    if len(candidates) <= 1:
        return None
    preferred = [
        item
        for item in candidates
        if item.workflow in REVIEW_POLICY_WORKFLOW_NAMES or Path(item.path).stem in REVIEW_POLICY_WORKFLOW_NAMES
    ]
    if len(preferred) == 1:
        return preferred[0]
    return None
def find_review_policy_workflow(repo_root: Path, inventory: list[WorkflowInventory], data: dict[str, Any]) -> Path | None:
    expected = expected_review_policy_env(data)
    check_name = expected.get("REVIEW_POLICY_CHECK_NAME") if isinstance(expected, dict) else None

    def prefer_pr_gate(candidates: list[WorkflowInventory]) -> list[WorkflowInventory]:
        pr_candidates = [
            item
            for item in candidates
            if scan_top_level_triggers(repo_root / item.path)[0] & PR_GATE_WORKFLOW_EVENTS
        ]
        return pr_candidates or candidates

    # Prefer a dedicated review-policy workflow by well-known name/path over any
    # declaration-driven mapping. Otherwise, ambiguous declarations can bind this
    # check to unrelated workflows that happen to reuse the same job name.
    dedicated_candidates = [
        item
        for item in inventory
        if item.workflow in REVIEW_POLICY_WORKFLOW_NAMES or Path(item.path).stem in REVIEW_POLICY_WORKFLOW_NAMES
    ]
    if dedicated_candidates:
        picked = pick_unique_review_policy_candidate(prefer_pr_gate(dedicated_candidates))
        if picked is not None:
            return repo_root / picked.path
        return None

    explicit_names: set[str] = set()
    if isinstance(check_name, str) and check_name:
        for item in data.get("expected_pr_workflows", []):
            if not isinstance(item, dict):
                continue
            workflow_name = item.get("workflow")
            jobs = item.get("jobs") if isinstance(item.get("jobs"), list) else []
            if isinstance(workflow_name, str) and any(isinstance(job, str) and job == check_name for job in jobs):
                explicit_names.add(workflow_name)
    explicit_candidates = [item for item in inventory if item.workflow in explicit_names]
    if explicit_candidates:
        picked = pick_unique_review_policy_candidate(prefer_pr_gate(explicit_candidates))
        if picked is not None:
            return repo_root / picked.path
        return None

    candidate_names = review_policy_workflow_names(data)
    named_candidates = [item for item in inventory if item.workflow in candidate_names]
    if named_candidates:
        picked = pick_unique_review_policy_candidate(prefer_pr_gate(named_candidates))
        if picked is not None:
            return repo_root / picked.path
        return None
    return None
def expected_review_policy_contract(data: dict[str, Any]) -> dict[str, Any] | None:
    policy = data.get("policy")
    if not isinstance(policy, dict):
        return None
    review = policy.get("review_policy")
    if not isinstance(review, dict):
        return None
    enforcement = review.get("enforcement") if isinstance(review.get("enforcement"), dict) else {}
    if enforcement.get("mode") != "github-native":
        return None
    exempt_permissions = review.get("exempt_author_permissions") if isinstance(review.get("exempt_author_permissions"), list) else []
    allowed_permissions = review.get("allowed_reviewer_permissions") if isinstance(review.get("allowed_reviewer_permissions"), list) else []
    return {
        "mode": "github-native",
        "required_approving_review_count": review.get("required_approvals"),
        "exempt_repository_owner": review.get("exempt_repository_owner", False),
        "exempt_author_permissions": sorted({item for item in exempt_permissions if isinstance(item, str) and item}),
        "allowed_reviewer_permissions": sorted({item for item in allowed_permissions if isinstance(item, str) and item}),
        "bypass_mode": enforcement.get("bypass_mode"),
    }

def find_legacy_review_policy_workflows(repo_root: Path, inventory: list[WorkflowInventory]) -> list[dict[str, Any]]:
    matches: list[dict[str, Any]] = []
    for item in inventory:
        workflow_path = repo_root / item.path
        workflow_env = scan_top_level_env(workflow_path)
        job_env = scan_job_env(workflow_path)
        workflow_events, _workflow_event_types = scan_top_level_triggers(workflow_path)
        job_names = {name for name in scan_job_names(workflow_path).values() if name}
        env_keys = set(workflow_env)
        for values in job_env.values():
            env_keys.update(values)
        reasons: list[str] = []
        lowered_workflow = item.workflow.lower()
        lowered_path = item.path.lower()
        if "review policy" in lowered_workflow or "review-policy" in lowered_path:
            reasons.append("name")
        if "pull_request_review" in workflow_events:
            reasons.append("trigger:pull_request_review")
        if any(key.startswith("REVIEW_POLICY_") for key in env_keys):
            reasons.append("env:review_policy")
        if "Review Policy Gate" in job_names:
            reasons.append("job:Review Policy Gate")
        if len(reasons) >= 2:
            matches.append(
                {
                    "workflow": item.workflow,
                    "path": item.path,
                    "reasons": sorted(set(reasons)),
                }
            )
    return matches

def compare_review_policy_contract(
    repo_root: Path,
    inventory: list[WorkflowInventory],
    data: dict[str, Any],
    github_branch_protection: dict[str, Any] | None,
) -> dict[str, Any] | None:
    expected = expected_review_policy_contract(data)
    if expected is None:
        return None
    mismatches: list[dict[str, Any]] = []
    legacy_workflows = find_legacy_review_policy_workflows(repo_root, inventory)
    if legacy_workflows:
        mismatches.append(
            {
                "key": "legacy_workflows",
                "expected": [],
                "actual": [item["path"] for item in legacy_workflows],
            }
        )
    checked = github_branch_protection is not None
    actual_review_policy = None
    if checked:
        actual_review_policy = github_branch_protection.get("review_policy")
        if actual_review_policy is None:
            mismatches.append({"key": "github_review_policy", "expected": "present", "actual": None})
        else:
            expected_bypass_permissions = expected["exempt_author_permissions"]
            actual_bypass_permissions = actual_review_policy.get("bypass_permissions")
            if actual_review_policy.get("required_approving_review_count") != expected["required_approving_review_count"]:
                mismatches.append(
                    {
                        "key": "required_approving_review_count",
                        "expected": expected["required_approving_review_count"],
                        "actual": actual_review_policy.get("required_approving_review_count"),
                    }
                )
            if actual_review_policy.get("bypass_mode") != expected["bypass_mode"]:
                mismatches.append(
                    {
                        "key": "bypass_mode",
                        "expected": expected["bypass_mode"],
                        "actual": actual_review_policy.get("bypass_mode"),
                    }
                )
            if actual_bypass_permissions != expected_bypass_permissions:
                mismatches.append(
                    {
                        "key": "bypass_permissions",
                        "expected": expected_bypass_permissions,
                        "actual": actual_bypass_permissions,
                    }
                )
    return {
        "mode": expected["mode"],
        "checked": checked,
        "expected": expected,
        "actual": actual_review_policy,
        "legacy_workflows": legacy_workflows,
        "mismatches": mismatches,
    }
def validate_declaration(data: Any) -> list[str]:
    errors: list[str] = []
    if not isinstance(data, dict):
        return ["declaration must be a JSON object"]
    if data.get("schema_version") != 1:
        errors.append("schema_version must equal 1")
    policy = data.get("policy")
    if not isinstance(policy, dict):
        errors.append("policy must be an object")
        policy = {}
    if policy.get("baseline_policy") != "explicit-waiver-required":
        errors.append("policy.baseline_policy must equal explicit-waiver-required")
    if policy.get("require_signed_commits") is not True:
        errors.append("policy.require_signed_commits must be true")
    branch_protection = policy.get("branch_protection")
    if not isinstance(branch_protection, dict):
        errors.append("policy.branch_protection must be an object")
    else:
        branches = branch_protection.get("protected_branches")
        if not isinstance(branches, list) or not branches or not all(isinstance(item, str) and item for item in branches):
            errors.append("policy.branch_protection.protected_branches must be a non-empty string list")
        elif any(item.startswith("<") and item.endswith(">") for item in branches):
            errors.append("policy.branch_protection.protected_branches must not contain placeholder values")
        if branch_protection.get("require_pull_request") is not True:
            errors.append("policy.branch_protection.require_pull_request must be true")
        if branch_protection.get("disallow_direct_pushes") is not True:
            errors.append("policy.branch_protection.disallow_direct_pushes must be true")
    review = policy.get("review_policy")
    if review is not None:
        if not isinstance(review, dict):
            errors.append("policy.review_policy must be an object when provided")
            review = {}
        if review.get("mode") != "conditional-required":
            errors.append("policy.review_policy.mode must equal conditional-required")
        if not isinstance(review.get("required_approvals"), int) or review.get("required_approvals", 0) < 1:
            errors.append("policy.review_policy.required_approvals must be an integer >= 1")
        if not isinstance(review.get("exempt_repository_owner"), bool):
            errors.append("policy.review_policy.exempt_repository_owner must be a boolean")
        for key in ("exempt_author_permissions", "allowed_reviewer_permissions"):
            value = review.get(key)
            if not isinstance(value, list) or not value or not all(isinstance(item, str) and item for item in value):
                errors.append(f"policy.review_policy.{key} must be a non-empty string list")
                continue
            invalid = sorted({item for item in value if item not in REPO_COLLABORATOR_ROLE_NAMES})
            if invalid:
                errors.append(
                    f"policy.review_policy.{key} contains unsupported permission values: {', '.join(invalid)}"
                )
        enforcement = review.get("enforcement")
        if not isinstance(enforcement, dict):
            errors.append("policy.review_policy.enforcement must be an object")
        else:
            if enforcement.get("mode") != "github-native":
                errors.append("policy.review_policy.enforcement.mode must equal github-native")
            if enforcement.get("bypass_mode") != "pull-request-only":
                errors.append("policy.review_policy.enforcement.bypass_mode must equal pull-request-only")
            if enforcement.get("check_name") is not None:
                errors.append("policy.review_policy.enforcement.check_name is unsupported when mode is github-native")
    for key in ("required_checks", "informational_checks", "waivers", "expected_pr_workflows"):
        value = data.get(key)
        if not isinstance(value, list):
            errors.append(f"{key} must be a list")
    for key in ("required_checks", "informational_checks"):
        values = data.get(key) if isinstance(data.get(key), list) else []
        if any(not isinstance(item, str) or not item for item in values):
            errors.append(f"{key} entries must be non-empty strings")
    waivers = data.get("waivers") if isinstance(data.get("waivers"), list) else []
    errors.extend(validate_waivers(waivers))
    expected_pr_workflows = data.get("expected_pr_workflows") if isinstance(data.get("expected_pr_workflows"), list) else []
    required_checks = data.get("required_checks") if isinstance(data.get("required_checks"), list) else []
    informational_checks = data.get("informational_checks") if isinstance(data.get("informational_checks"), list) else []
    required_set = {item for item in required_checks if isinstance(item, str) and item}
    informational_set = {item for item in informational_checks if isinstance(item, str) and item}
    overlap = sorted(required_set & informational_set)
    if overlap:
        errors.append("informational_checks must not overlap required_checks: " + ", ".join(overlap))
    legacy_review_policy_checks = sorted(
        {
            item
            for item in required_set | informational_set
            if "review policy" in item.lower()
        }
    )
    if legacy_review_policy_checks:
        errors.append(
            "required_checks and informational_checks must not declare workflow-backed review-policy checks; use optional GitHub-native policy.review_policy instead: "
            + ", ".join(legacy_review_policy_checks)
        )
    legacy_review_policy_workflows = sorted(
        {
            item.get("workflow")
            for item in expected_pr_workflows
            if isinstance(item, dict)
            and isinstance(item.get("workflow"), str)
            and "review policy" in item.get("workflow").lower()
        }
    )
    if legacy_review_policy_workflows:
        errors.append(
            "expected_pr_workflows must not declare workflow-backed review policy entries; use optional GitHub-native policy.review_policy instead: "
            + ", ".join(legacy_review_policy_workflows)
        )
    expected_workflow_names, _expected_job_names, expected_workflow_checks = expected_workflow_shapes(data)
    declared_checks = declared_check_names(data)
    missing_declared_checks = expected_workflow_checks_missing_declaration(expected_workflow_checks, declared_checks)
    if missing_declared_checks:
        errors.append(
            "expected_pr_workflows.jobs must each be listed in required_checks or informational_checks using the final GitHub check name; local reusable workflows expand to composite names like 'Caller / Child': "
            + ", ".join(missing_declared_checks)
        )
    for item in expected_pr_workflows:
        if not isinstance(item, dict):
            errors.append("expected_pr_workflows entries must be objects")
            continue
        if not isinstance(item.get("workflow"), str) or not item.get("workflow"):
            errors.append("expected_pr_workflows.workflow must be a non-empty string")
        jobs = item.get("jobs")
        if not isinstance(jobs, list) or not jobs or not all(isinstance(job, str) and job for job in jobs):
            errors.append("expected_pr_workflows.jobs must be a non-empty string list")

    unknown_waivers = sorted(
        {
            waiver.get("check_name")
            for waiver in waivers
            if isinstance(waiver, dict)
            and isinstance(waiver.get("check_name"), str)
            and waiver.get("check_name")
            and waiver.get("check_name") not in required_set
        }
    )
    if unknown_waivers:
        errors.append("waivers.check_name must match an entry in required_checks: " + ", ".join(unknown_waivers))
    return errors

def resolve_repo_relative_path(repo_root: Path, raw_path: str) -> Path:
    """Resolve an input path against repo_root unless it is already absolute.

    This keeps the CLI consistent: when --repo-root points at a target repository,
    auxiliary inputs (such as GitHub snapshots) should be resolvable from any cwd.
    """
    path = Path(raw_path).expanduser()
    if path.is_absolute():
        return path
    return (repo_root / path).resolve()


def load_github_required_checks(args: argparse.Namespace, repo_root: Path) -> list[str] | None:
    if args.github_required_checks_file:
        data = load_json(resolve_repo_relative_path(repo_root, args.github_required_checks_file))
        if isinstance(data, dict):
            data = data.get("required_checks")
        if not isinstance(data, list) or not all(isinstance(item, str) for item in data):
            raise InputValidationError("--github-required-checks-file must contain a JSON string array or an object with required_checks")
        return data
    if args.github_required_check:
        return args.github_required_check
    return None
def load_github_branch_protection(args: argparse.Namespace, repo_root: Path) -> dict[str, Any] | None:
    if not args.github_branch_protection_file:
        return None
    data = load_json(resolve_repo_relative_path(repo_root, args.github_branch_protection_file))
    if isinstance(data, dict) and isinstance(data.get("branch_protection"), dict):
        data = data["branch_protection"]
    if not isinstance(data, dict):
        raise InputValidationError("--github-branch-protection-file must contain a JSON object or an object with branch_protection")
    branches = data.get("protected_branches")
    if not isinstance(branches, list) or not all(isinstance(item, str) and item for item in branches):
        raise InputValidationError("github branch protection must include protected_branches as a string list")
    if not isinstance(data.get("require_pull_request"), bool):
        raise InputValidationError("github branch protection must include require_pull_request as a boolean")
    if not isinstance(data.get("disallow_direct_pushes"), bool):
        raise InputValidationError("github branch protection must include disallow_direct_pushes as a boolean")
    if not isinstance(data.get("require_signed_commits"), bool):
        raise InputValidationError("github branch protection must include require_signed_commits as a boolean")
    review_policy = data.get("review_policy")
    if review_policy is not None:
        if not isinstance(review_policy, dict):
            raise InputValidationError("github branch protection review_policy must be an object when provided")
        if not isinstance(review_policy.get("required_approving_review_count"), int) or review_policy.get("required_approving_review_count", -1) < 0:
            raise InputValidationError("github branch protection review_policy.required_approving_review_count must be an integer >= 0")
        bypass_permissions = review_policy.get("bypass_permissions")
        if not isinstance(bypass_permissions, list) or not all(isinstance(item, str) and item in REPO_COLLABORATOR_ROLE_NAMES for item in bypass_permissions):
            raise InputValidationError("github branch protection review_policy.bypass_permissions must be a string list of supported permission values")
        if not isinstance(review_policy.get("bypass_mode"), str) or not review_policy.get("bypass_mode"):
            raise InputValidationError("github branch protection review_policy.bypass_mode must be a non-empty string")
        review_policy = {
            "required_approving_review_count": review_policy["required_approving_review_count"],
            "bypass_permissions": sorted(set(bypass_permissions)),
            "bypass_mode": review_policy["bypass_mode"],
        }
    return {
        "protected_branches": branches,
        "require_pull_request": data["require_pull_request"],
        "disallow_direct_pushes": data["disallow_direct_pushes"],
        "require_signed_commits": data["require_signed_commits"],
        "review_policy": review_policy,
    }
def build_error_report(
    repo_root: Path,
    declaration_path: Path,
    message: str,
    *,
    github_checks: list[str] | None = None,
    github_branch_protection: dict[str, Any] | None = None,
) -> dict[str, Any]:
    return {
        "repo_root": str(repo_root),
        "declaration": str(declaration_path),
        "declaration_errors": [message],
        "workflow_inventory": [],
        "workflow_inventory_errors": [],
        "required_checks_not_in_expected_pr_workflows": [],
        "required_checks_requiring_github_alignment": [],
        "required_checks_missing_from_workflow_inventory": [],
        "expected_pr_workflow_checks_missing_from_declaration": [],
        "workflow_backed_required_checks_not_in_expected_pr_workflows": [],
        "expected_pr_workflow_drift": [],
        "expected_pr_workflow_trigger_drift": [],
        "expected_pr_workflow_merge_group_drift": [],
        "review_policy_alignment": None,
        "github_required_checks": github_checks,
        "github_alignment": None,
        "github_branch_protection": github_branch_protection,
        "branch_protection_alignment": None,
        "status": "invalid",
    }
def build_report(
    repo_root: Path,
    declaration_path: Path,
    _strict: bool,
    github_checks: list[str] | None,
    github_branch_protection: dict[str, Any] | None,
    *,
    allow_unchecked_branch_protection: bool = False,
) -> tuple[int, dict[str, Any]]:
    try:
        data = load_json(declaration_path)
    except JsonInputError as exc:
        return (
            2,
            build_error_report(
                repo_root,
                declaration_path,
                f"invalid JSON input {exc.path}: {exc.message}",
                github_checks=github_checks,
                github_branch_protection=github_branch_protection,
            ),
        )
    errors = validate_declaration(data)
    inventory = scan_workflows(repo_root)
    inventory_errors: list[str] = []
    workflow_name_to_paths: dict[str, list[str]] = {}
    workflow_path_map: dict[str, str] = {}
    for item in inventory:
        workflow_name_to_paths.setdefault(item.workflow, []).append(item.path)
        workflow_path_map.setdefault(item.workflow, item.path)
    for workflow_name, paths in sorted(workflow_name_to_paths.items()):
        if len(paths) > 1:
            inventory_errors.append(
                f"workflow name {workflow_name} is duplicated across: {', '.join(sorted(paths))}"
            )
    workflow_map = workflow_lookup(inventory)
    remote_reusable_map = remote_reusable_lookup(inventory)
    unresolved_matrix_map = unresolved_matrix_lookup(inventory)
    check_context_inventory = [
        item
        for item in inventory
        if scan_top_level_triggers(repo_root / item.path)[0] & CHECK_CONTEXT_WORKFLOW_EVENTS
    ]
    duplicate_contexts = duplicated_check_contexts(check_context_inventory)
    for check_name, labels in sorted(duplicate_contexts.items()):
        inventory_errors.append(
            f"check context {check_name} is duplicated across workflows: {', '.join(labels)}"
        )
    github_checks_set = set(github_checks) if github_checks is not None else None
    required_checks = [item for item in data.get("required_checks", []) if isinstance(item, str)] if isinstance(data, dict) else []
    required_checks_set = set(required_checks)
    declared_checks = declared_check_names(data) if isinstance(data, dict) else set()
    expected_workflow_names, _expected_job_names, expected_workflow_checks = expected_workflow_shapes(data) if isinstance(data, dict) else (set(), set(), set())
    expected_check_to_workflows = expected_check_workflows(data) if isinstance(data, dict) else {}
    # Treat the repo workflow inventory (all workflows) as the workflow-backed namespace.
    # A required check that matches any local workflow job name must be mapped through
    # expected_pr_workflows and validated for PR/merge-queue semantics.
    inventory_check_names = all_check_names(inventory)
    workflow_backed_required_checks = sorted(check for check in required_checks if check in expected_workflow_checks)
    required_checks_without_expected_mapping = required_checks_missing_expected_mapping(
        required_checks,
        expected_workflow_checks,
        inventory_check_names,
        remote_reusable_map,
        unresolved_matrix_map,
    )
    workflow_backed_required_check_name_drift = required_checks_without_expected_mapping
    missing_expected_mapping_set = set(required_checks_without_expected_mapping)
    required_checks_requiring_github_alignment = sorted(
        {
            check
            for check in workflow_backed_required_checks
            if github_checks_set is None
            and any(
                remote_reusable_job_matches(check, remote_reusable_map.get(workflow_name, set()))
                or matches_unresolved_matrix_check(check, unresolved_matrix_map.get(workflow_name, set()))
                for workflow_name in expected_check_to_workflows.get(check, set())
            )
        }
        | {
            # External statuses cannot be proven locally; without GitHub required-check input,
            # treat them as unresolved drift rather than silently accepting them as "ok".
            check
            for check in required_checks
            if github_checks_set is None
            and check not in expected_workflow_checks
            and check not in inventory_check_names
            and check not in missing_expected_mapping_set
        }
    )
    missing_required_jobs = sorted(
        check
        for check in workflow_backed_required_checks
        if check not in expected_workflow_checks
        or (
            check not in inventory_check_names
            and not any(
                matches_remote_reusable_check(check, remote_reusable_map.get(workflow_name, set()), github_checks_set)
                or (github_checks_set is None and remote_reusable_job_matches(check, remote_reusable_map.get(workflow_name, set())))
                or matches_unresolved_matrix_check(check, unresolved_matrix_map.get(workflow_name, set()), github_checks_set)
                for workflow_name in expected_check_to_workflows.get(check, set())
            )
        )
    )
    expected_workflow_checks_without_declaration = expected_workflow_checks_missing_declaration(expected_workflow_checks, declared_checks)
    missing_workflows: list[dict[str, Any]] = []
    expected_pr_workflow_trigger_drift: list[dict[str, Any]] = []
    expected_pr_workflow_merge_group_drift: list[dict[str, Any]] = []
    declared_branch_protection = None
    declared_require_signed_commits = None
    review_policy_alignment = None
    if isinstance(data, dict):
        for item in data.get("expected_pr_workflows", []):
            if not isinstance(item, dict):
                continue
            workflow_name = item.get("workflow")
            jobs = item.get("jobs") if isinstance(item.get("jobs"), list) else []
            existing = workflow_map.get(workflow_name)
            if existing is None:
                missing_workflows.append({"workflow": workflow_name, "missing_jobs": jobs})
                continue
            workflow_path = workflow_path_map.get(workflow_name)
            if isinstance(workflow_path, str) and workflow_path:
                workflow_events, _workflow_event_types = scan_top_level_triggers(repo_root / workflow_path)
                if not (workflow_events & PR_GATE_WORKFLOW_EVENTS):
                    expected_pr_workflow_trigger_drift.append(
                        {
                            "workflow": workflow_name,
                            "path": workflow_path,
                            "actual_events": sorted(workflow_events),
                            "required_events_any_of": sorted(PR_GATE_WORKFLOW_EVENTS),
                        }
                    )
                required_jobs_missing_merge_group = sorted(
                    job
                    for job in jobs
                    if isinstance(job, str)
                    and job in required_checks_set
                )
                if (
                    required_jobs_missing_merge_group
                    and (workflow_events & PR_GATE_WORKFLOW_EVENTS)
                    and "merge_group" not in workflow_events
                ):
                    expected_pr_workflow_merge_group_drift.append(
                        {
                            "workflow": workflow_name,
                            "path": workflow_path,
                            "required_jobs": required_jobs_missing_merge_group,
                            "actual_events": sorted(workflow_events),
                            "missing_event": "merge_group",
                        }
                    )
            missing_jobs = [
                job
                for job in jobs
                if job not in existing
                and not matches_github_aligned_check(
                    job,
                    remote_reusable_map.get(workflow_name, set()),
                    unresolved_matrix_map.get(workflow_name, set()),
                    github_checks_set,
                )
            ]
            if missing_jobs:
                missing_workflows.append({"workflow": workflow_name, "missing_jobs": missing_jobs})
        if isinstance(data.get("policy"), dict):
            if isinstance(data["policy"].get("branch_protection"), dict):
                declared_branch_protection = data["policy"]["branch_protection"]
            if isinstance(data["policy"].get("require_signed_commits"), bool):
                declared_require_signed_commits = data["policy"]["require_signed_commits"]
        review_policy_alignment = compare_review_policy_contract(repo_root, inventory, data, github_branch_protection)
    github_drift = None
    branch_protection_drift = None
    local_inventory_drift = bool(
        required_checks_without_expected_mapping
        or required_checks_requiring_github_alignment
        or missing_required_jobs
        or expected_workflow_checks_without_declaration
        or missing_workflows
        or expected_pr_workflow_trigger_drift
        or expected_pr_workflow_merge_group_drift
        or (review_policy_alignment is not None and review_policy_alignment["mismatches"])
    )
    exit_code = 0
    if errors:
        exit_code = worsen_exit_code(exit_code, 2)
    if inventory_errors:
        exit_code = worsen_exit_code(exit_code, 2)
    if github_checks is not None:
        declared = set(required_checks)
        actual = set(github_checks)
        github_drift = {
            "missing_on_github": sorted(declared - actual),
            "unexpected_on_github": sorted(actual - declared),
        }
        if github_drift["missing_on_github"] or github_drift["unexpected_on_github"]:
            exit_code = worsen_exit_code(exit_code, 1)
    if declared_branch_protection is not None:
        if github_branch_protection is None:
            branch_protection_drift = {
                "checked": False,
                "missing_input": "--github-branch-protection-file",
                "missing_on_github": None,
                "unexpected_on_github": None,
                "require_pull_request_matches": None,
                "disallow_direct_pushes_matches": None,
                "require_signed_commits_matches": None,
            }
            # Default fail-closed: declaring branch protection without GitHub evidence is unresolved drift.
            if not allow_unchecked_branch_protection:
                exit_code = worsen_exit_code(exit_code, 1)
        else:
            declared_branches = set(declared_branch_protection.get("protected_branches", []))
            actual_branches = set(github_branch_protection.get("protected_branches", []))
            branch_protection_drift = {
                "checked": True,
                "missing_on_github": sorted(declared_branches - actual_branches),
                "unexpected_on_github": sorted(actual_branches - declared_branches),
                "require_pull_request_matches": github_branch_protection.get("require_pull_request") == declared_branch_protection.get("require_pull_request"),
                "disallow_direct_pushes_matches": github_branch_protection.get("disallow_direct_pushes") == declared_branch_protection.get("disallow_direct_pushes"),
                "require_signed_commits_matches": github_branch_protection.get("require_signed_commits") == declared_require_signed_commits,
            }
            if (
                branch_protection_drift["missing_on_github"]
                or branch_protection_drift["unexpected_on_github"]
                or not branch_protection_drift["require_pull_request_matches"]
                or not branch_protection_drift["disallow_direct_pushes_matches"]
                or not branch_protection_drift["require_signed_commits_matches"]
            ):
                exit_code = worsen_exit_code(exit_code, 1)
    if local_inventory_drift:
        exit_code = worsen_exit_code(exit_code, 1)
    report = {
        "repo_root": str(repo_root),
        "declaration": str(declaration_path),
        "declaration_errors": errors,
        "workflow_inventory": [
            {
                "path": item.path,
                "workflow": item.workflow,
                "jobs": item.jobs,
                "remote_reusable_jobs": item.remote_reusable_jobs,
                "unresolved_matrix_jobs": item.unresolved_matrix_jobs,
            }
            for item in inventory
        ],
        "workflow_inventory_errors": inventory_errors,
        "required_checks_not_in_expected_pr_workflows": required_checks_without_expected_mapping,
        "required_checks_requiring_github_alignment": required_checks_requiring_github_alignment,
        "required_checks_missing_from_workflow_inventory": missing_required_jobs,
        "expected_pr_workflow_checks_missing_from_declaration": expected_workflow_checks_without_declaration,
        "workflow_backed_required_checks_not_in_expected_pr_workflows": workflow_backed_required_check_name_drift,
        "expected_pr_workflow_drift": missing_workflows,
        "expected_pr_workflow_trigger_drift": expected_pr_workflow_trigger_drift,
        "expected_pr_workflow_merge_group_drift": expected_pr_workflow_merge_group_drift,
        "review_policy_alignment": review_policy_alignment,
        "github_required_checks": github_checks,
        "github_alignment": github_drift,
        "github_branch_protection": github_branch_protection,
        "branch_protection_alignment": branch_protection_drift,
        "status": "invalid" if exit_code == 2 else "drift" if exit_code == 1 else "ok",
    }
    return exit_code, report
def main() -> int:
    parser = argparse.ArgumentParser(description="Validate a quality-gates declaration against repo workflow inventory and optional GitHub required checks/branch protection state.")
    parser.add_argument("--repo-root", default=".")
    parser.add_argument("--declaration", default=".github/quality-gates.json")
    parser.add_argument("--strict", action="store_true", help="Retained for compatibility; local workflow drift now fails by default.")
    parser.add_argument("--github-required-check", action="append", default=[], help="Repeatable exact GitHub required check names to compare against the declaration.")
    parser.add_argument("--github-required-checks-file", help="JSON file containing a string array of GitHub required checks or an object with required_checks.")
    parser.add_argument("--github-branch-protection-file", help="JSON file containing branch protection state or an object with branch_protection.")
    parser.add_argument(
        "--allow-unchecked-branch-protection",
        action="store_true",
        help="Allow branch_protection alignment to remain unchecked when no GitHub snapshot is provided (still reported, but does not fail the report).",
    )
    args = parser.parse_args()
    repo_root = Path(args.repo_root).expanduser().resolve()
    declaration_path = Path(args.declaration)
    if not declaration_path.is_absolute():
        declaration_path = (repo_root / declaration_path).resolve()
    if not declaration_path.exists():
        report = build_error_report(repo_root, declaration_path, f"declaration not found: {declaration_path}")
        print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
        return 2
    try:
        github_checks = load_github_required_checks(args, repo_root)
        github_branch_protection = load_github_branch_protection(args, repo_root)
    except (JsonInputError, InputValidationError) as exc:
        report = build_error_report(repo_root, declaration_path, str(exc))
        if isinstance(exc, JsonInputError):
            report["declaration_errors"] = [f"invalid JSON input {exc.path}: {exc.message}"]
        print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
        return 2
    exit_code, report = build_report(
        repo_root,
        declaration_path,
        args.strict,
        github_checks,
        github_branch_protection,
        allow_unchecked_branch_protection=args.allow_unchecked_branch_protection,
    )
    print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
    return exit_code
if __name__ == "__main__":
    raise SystemExit(main())
