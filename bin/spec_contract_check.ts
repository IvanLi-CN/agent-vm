#!/usr/bin/env bun
/** Validate the structure of canonical topic requirement specifications. */

import { existsSync, lstatSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import { isDirectRegularFile, isPathInside, REPO_ROOT, readText } from "./repo_util.ts";

const TOP_LEVEL_RE = /^##\s+(.+?)\s*$/;
const REQ_RE = /\bREQ-[A-Z0-9][A-Z0-9_-]*\b/g;
const VER_RE = /\bVER-[A-Z0-9][A-Z0-9_-]*\b/g;
const NORMATIVE_RE = /\b(MUST|SHALL|SHOULD|MAY NOT|MUST NOT)\b|必须|不得|仅可|只能|应当|不可/i;
const UNRESOLVED_RE = /\b(TBD|TBA|TBC|TODO|TO BE DETERMINED|TO BE CONFIRMED|OPEN QUESTION)\b|待确认|待定|未决/i;
const FORBIDDEN_HEADINGS = [
  "acceptance checklist", "quality gates", "related prs", "related pr", "pull requests",
  "pr checklist", "related changes", "test commands", "verification commands", "risks",
  "open questions", "assumptions", "task orchestration", "execution steps", "current status",
  "milestones", "implementation status", "delivery status", "execution",
];

export function emit(path: string, code: string, message: string, line?: number): string {
  const suffix = line === undefined ? "" : ` line=${line}`;
  return `path=${path} code=${code} msg=${message}${suffix}`;
}

function matchesAll(pattern: RegExp, value: string): string[] {
  return [...value.matchAll(new RegExp(pattern.source, `${pattern.flags.replace("g", "")}g`))].map((match) => match[0]);
}

function sectionMap(lines: string[]): Map<string, { start: number; body: string[] }> {
  const sections = new Map<string, { start: number; body: string[] }>();
  let current: string | undefined;
  let start = 0;
  let body: string[] = [];
  for (const [index, line] of lines.entries()) {
    const match = TOP_LEVEL_RE.exec(line);
    if (match) {
      if (current !== undefined) {
        sections.set(current, { start, body });
      }
      current = match[1].trim();
      start = index + 1;
      body = [];
    } else if (current !== undefined) {
      body.push(line);
    }
  }
  if (current !== undefined) {
    sections.set(current, { start, body });
  }
  return sections;
}

export function check(path: string, root: string = REPO_ROOT): string[] {
  const absolute = resolve(path);
  const rootAbsolute = resolve(root);
  const relativePath = relative(rootAbsolute, absolute).split("\\").join("/");
  const errors: string[] = [];
  if (!isPathInside(rootAbsolute, absolute)) {
    return [emit(relativePath, "SPEC_PATH_INVALID", "Spec path must remain inside the repository root")];
  }
  if (!/^docs\/specs\/[a-z0-9]+(?:-[a-z0-9]+)*\/SPEC\.md$/.test(relativePath)) {
    return [emit(relativePath, "SPEC_PATH_INVALID", "strict checking only applies to canonical docs/specs/<slug>/SPEC.md")];
  }
  if (!existsSync(absolute) || lstatSync(absolute).isSymbolicLink() || !lstatSync(absolute).isFile()) {
    return [emit(relativePath, "SPEC_NOT_REGULAR_FILE", "Spec must be a regular file")];
  }

  const lines = readText(absolute).split("\n").map((line) => line.replace(/\r$/, ""));
  const sections = sectionMap(lines);
  for (const required of ["Context and Scope", "Requirements", "Verification", "Related ADRs"]) {
    if (!sections.has(required)) {
      errors.push(emit(relativePath, "SPEC_SECTION_MISSING", `missing required section: ## ${required}`));
    }
  }
  for (const [heading, section] of sections) {
    const normalized = heading.toLowerCase().trim();
    if (FORBIDDEN_HEADINGS.some((forbidden) => normalized === forbidden || normalized.startsWith(`${forbidden} `))) {
      errors.push(emit(relativePath, "SPEC_FORBIDDEN_SECTION", `task-state or execution section is not allowed in SPEC.md: ## ${heading}`, section.start));
    }
  }
  lines.forEach((line, index) => {
    if (/^\s*-\s*\[[ xX]\]/.test(line)) {
      errors.push(emit(relativePath, "SPEC_CHECKBOX_FORBIDDEN", "SPEC.md cannot contain checkbox progress", index + 1));
    }
  });

  const requirements = sections.get("Requirements")?.body.join("\n") ?? "";
  const verification = sections.get("Verification")?.body.join("\n") ?? "";
  const reqIds = [...new Set(matchesAll(REQ_RE, requirements))].sort();
  const verIds = [...new Set(matchesAll(VER_RE, verification))].sort();
  if (reqIds.length === 0) {
    errors.push(emit(relativePath, "SPEC_REQUIREMENTS_MISSING", "Requirements must declare stable REQ-* identifiers"));
  }
  if (verIds.length === 0) {
    errors.push(emit(relativePath, "SPEC_VERIFICATION_MISSING", "Verification must declare stable VER-* identifiers"));
  }
  if (requirements && !NORMATIVE_RE.test(requirements)) {
    errors.push(emit(relativePath, "SPEC_NORMATIVE_BOUNDARY_MISSING", "Requirements must contain normative MUST/SHALL or Chinese normative language"));
  }
  for (const reqId of reqIds) {
    const pattern = new RegExp(`(?:covers?|覆盖)\\s*[:=]\\s*[^\\n]*\\b${reqId}\\b`, "i");
    if (!pattern.test(verification)) {
      errors.push(emit(relativePath, "SPEC_TRACEABILITY_MISSING", `no VER-* verification explicitly covers ${reqId}`));
    }
  }
  for (const verId of verIds) {
    const occurrences = verification.match(new RegExp(`\\b${verId}\\b`, "g"))?.length ?? 0;
    if (occurrences < 1) {
      errors.push(emit(relativePath, "SPEC_VERIFICATION_INVALID", `verification id ${verId} is not readable`));
    }
  }

  const adrSection = sections.get("Related ADRs");
  if (adrSection) {
    const adrLines = adrSection.body.filter((line) => line.trim().length > 0);
    if (adrLines.length === 0) {
      errors.push(emit(relativePath, "SPEC_RELATED_ADRS_INVALID", "Related ADRs must be exactly None or contain ADR links"));
    } else if (!(adrLines.length === 1 && adrLines[0] === "None")) {
      const paths: string[] = [];
      for (const line of adrLines) {
        const match = /^-\s+\[[^\]]+\]\(\.\.\/\.\.\/(adr\/[0-9]{4}-[a-z0-9]+(?:-[a-z0-9]+)*\.md)\)$/.exec(line);
        if (!match) {
          errors.push(emit(relativePath, "SPEC_RELATED_ADRS_INVALID", `invalid Related ADRs entry: ${line}`));
          continue;
        }
        const target = join(rootAbsolute, "docs", match[1]);
        paths.push(`docs/${match[1]}`);
        if (!isPathInside(rootAbsolute, target) || !isDirectRegularFile(target)) {
          errors.push(emit(relativePath, "SPEC_RELATED_ADRS_INVALID", `missing ADR target: docs/${match[1]}`));
        }
      }
      if (JSON.stringify(paths) !== JSON.stringify([...new Set(paths)].sort())) {
        errors.push(emit(relativePath, "SPEC_RELATED_ADRS_INVALID", "Related ADRs must be path-sorted and duplicate-free"));
      }
    }
  }
  lines.forEach((line, index) => {
    if (UNRESOLVED_RE.test(line) && !line.trimStart().startsWith(">") && !line.trimStart().startsWith("<!--")) {
      errors.push(emit(relativePath, "SPEC_UNRESOLVED_DECISION", "unresolved decision markers do not belong in canonical SPEC.md", index + 1));
    }
  });
  return errors;
}

type ParsedArgs = { paths: string[]; root: string };
const USAGE = "usage: spec_contract_check.ts --path <path> [--path <path> ...] [--repo-root <path>]";

function requiredValue(args: string[], index: number): string {
  const value = args[index + 1];
  if (!value || value.startsWith("--")) {
    throw new Error(USAGE);
  }
  return value;
}

export function parseArgs(args: string[]): ParsedArgs {
  const paths: string[] = [];
  let root = REPO_ROOT;
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    if (arg === "--path") {
      paths.push(requiredValue(args, index));
      index += 1;
    } else if (arg.startsWith("--path=")) {
      const value = arg.slice("--path=".length);
      if (!value) throw new Error(USAGE);
      paths.push(value);
    } else if (arg === "--repo-root") {
      root = resolve(requiredValue(args, index));
      index += 1;
    } else if (arg.startsWith("--repo-root=")) {
      const value = arg.slice("--repo-root=".length);
      if (!value) throw new Error(USAGE);
      root = resolve(value);
    } else {
      throw new Error(USAGE);
    }
  }
  return { paths, root };
}

export function main(args: string[] = Bun.argv.slice(2)): number {
  if (args.includes("--help")) {
    console.log(USAGE);
    return 0;
  }
  let parsed: ParsedArgs;
  try {
    parsed = parseArgs(args);
  } catch (error) {
    console.error(`ERROR ${String(error)}`);
    return 2;
  }
  const paths = parsed.paths.map((path) => resolve(parsed.root, path));
  if (paths.length === 0) {
    console.error(USAGE);
    return 2;
  }
  const errors = paths.flatMap((path) => check(path, parsed.root));
  if (errors.length > 0) {
    errors.forEach((error) => console.log(error));
    return 1;
  }
  for (const path of parsed.paths) {
    console.log(`PASS path=${path} checks=ok`);
  }
  return 0;
}

if (import.meta.main) {
  process.exitCode = main();
}
