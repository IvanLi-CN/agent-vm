import { expect, test } from 'bun:test';
import { classifyChanges } from './ci_classify.ts';
import { parse } from 'yaml';
import { readFileSync } from 'node:fs';

test('only isolated content changes may omit macOS payload', () => {
  expect(classifyChanges(['docs/example.md']).macos).toBe(false);
  expect(classifyChanges(['web/src/App.tsx']).macos).toBe(false);
  for (const files of [[], ['bun.lock'], ['package.json'], ['web/vite.config.ts'], ['skills/agent-vm/scripts/client.ts'], ['bin/test_runner.ts'], ['.github/workflows/ci.yml'], ['unknown'], ['docs/example.md', 'skills/example.ts']]) {
    expect(classifyChanges(files).macos).toBe(true);
  }
});

test('workloads are parallel and stable gates fail closed on all dependencies', () => {
  for (const filename of ['ci.yml', 'ci-main.yml']) {
    const workflow = parse(readFileSync(new URL(`../.github/workflows/${filename}`, import.meta.url), 'utf8'));
    const { validation, heavy } = workflow.jobs;
    expect(validation.needs).toEqual(heavy.needs);
    expect(JSON.stringify(validation.needs || [])).not.toContain('heavy');
    const gate = workflow.jobs['pr-gate'] || workflow.jobs['main-gate'];
    expect(gate.if).toBe('always()');
    expect(gate.needs).toContain('validation');
    expect(gate.needs).toContain('heavy');
    expect(gate.steps[0].run).toContain("== 'success'");
    for (const step of validation.steps.filter((s: any) => ['Run isolated rqlite integration tests', 'Build product packages'].includes(s.name))) {
      expect(step.if).toContain("matrix.os == 'ubuntu-latest'");
    }
  }
});
