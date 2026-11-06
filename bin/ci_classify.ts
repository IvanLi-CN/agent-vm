#!/usr/bin/env bun
import { appendFileSync } from 'node:fs';

export function classifyChanges(files: string[]) {
  // Only content-only Web files and prose are proven independent of the POSIX
  // tooling. Shared manifests, tests, scripts and unrecognised paths run full.
  const safe = files.length > 0 && files.every(file =>
    /^(docs\/.*\.(md|png|svg)|README\.md|GLOSSARY\.md)$/.test(file) ||
    /^web\/src\/.*\.(tsx?|css)$/.test(file));
  return { full: !safe, macos: !safe, reason: safe ? 'content-only-change' : 'shared-platform-or-unknown-change' };
}

if (import.meta.main) {
  const base = process.env.GITHUB_BASE_SHA;
  const head = process.env.GITHUB_SHA || 'HEAD';
  let result = { full: true, macos: true, reason: 'classification-failed' };
  if (base && /^[a-f0-9]{40,64}$/i.test(base) && /^[a-f0-9]{40,64}$/i.test(head)) {
    const diff = Bun.spawnSync(['git', 'diff', '--name-only', '-z', `${base}...${head}`], { stdout: 'pipe', stderr: 'pipe' });
    if (diff.exitCode === 0) result = classifyChanges(new TextDecoder().decode(diff.stdout).split('\0').filter(Boolean));
  }
  if (result.reason === 'classification-failed') console.error('::warning::Cannot classify diff; running complete validation on both platforms.');
  if (process.env.GITHUB_OUTPUT) appendFileSync(process.env.GITHUB_OUTPUT, Object.entries(result).map(([k,v]) => `${k}=${v}\n`).join(''));
  console.log(JSON.stringify(result));
}
