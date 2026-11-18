import { test, expect } from 'bun:test';
import { existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { catalog } from './catalog';

const root = resolve(import.meta.dir, '../..');

test('catalog sources exist, use unique stable slugs and cover reader paths', () => {
  expect(catalog.length).toBeGreaterThan(40);
  expect(new Set(catalog.map(entry => entry.slug)).size).toBe(catalog.length);
  for (const entry of catalog) expect(existsSync(resolve(root, entry.path))).toBe(true);
  for (const section of ['开始使用', '使用指南', '部署运维', '开发文档', '文档站']) {
    expect(catalog.some(entry => entry.section === section)).toBe(true);
  }
  expect(catalog.map(entry => entry.path)).not.toContain('.agents/skills/documentation-review/SKILL.md');
});
