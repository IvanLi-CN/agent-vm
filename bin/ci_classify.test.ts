import { expect, test } from 'bun:test';
import { classifyChanges } from './ci_classify.ts';
import { parse } from 'yaml';
import { readFileSync } from 'node:fs';

test('only isolated content changes may omit macOS payload', () => {
  expect(classifyChanges(['docs/example.md']).macos).toBe(false);
  expect(classifyChanges(['web/src/App.tsx']).macos).toBe(false);
  for (const files of [[], ['bun.lock'], ['package.json'], ['web/vite.config.ts'], ['skills/agent-vm/scripts/client.ts'], ['bin/test_runner.ts'], ['.github/workflows/ci.yml'], ['unknown'], ['deploy/assets/config.json.example'], ['docs/example.md', 'skills/example.ts']]) {
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

test('cache ownership preserves cold execution and isolates PR from writes', () => {
  for (const filename of ['ci.yml', 'ci-main.yml']) {
    const workflow = parse(readFileSync(new URL(`../.github/workflows/${filename}`, import.meta.url), 'utf8'));
    for (const [job, role] of [['validation', 'portable'], ['heavy', 'heavy']]) {
      const steps = workflow.jobs[job].steps;
      expect(steps.find((s: any) => s.uses === 'actions/setup-go@v5').with.cache).toBe(false);
      const restore = steps.find((s: any) => s.id === 'ci_cache');
      expect(restore.with.role).toBe(role);
      expect(steps.indexOf(restore)).toBeLessThan(steps.findIndex((s: any) => s.run === 'bun install --frozen-lockfile'));
      const finish = steps.find((s: any) => s.uses === './.github/actions/ci-cache-save');
      expect(finish.with.publish).toContain("github.event_name == 'push'");
      expect(finish.with.publish).toContain("github.ref == 'refs/heads/main'");
      expect(finish.with.publish).toContain("job.status == 'success'");
      expect(finish.with.role).toBe(role);
    }
  }
  const action = (name: string) => parse(readFileSync(new URL(`../.github/actions/ci-cache-${name}/action.yml`, import.meta.url), 'utf8'));
  const restore = action('restore').runs.steps;
  expect(restore.filter((s: any) => s.uses === 'actions/cache/restore@v4')).toHaveLength(3);
  expect(restore.find((s: any) => s.id === 'plan').env.SOURCE_HASH).toContain("'skills/**'");
  expect(restore.find((s: any) => s.id === 'plan').env.SOURCE_HASH).toContain("'web/**'");
  const save = action('save').runs.steps.filter((s: any) => s.uses === 'actions/cache/save@v4');
  for (const step of save) {
    expect(step.if).toContain("inputs.publish == 'true'");
    expect(step.if).toContain('!fromJSON(inputs.state)');
    if (step.id !== 'build') expect(step.if).toContain("inputs.role == 'portable'");
  }
});
