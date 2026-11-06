import { test, expect } from 'bun:test';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, symlinkSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildMaterials } from './build-materials';
test('materials bind exact source revision and refuse symlinks before writing output', () => {
  const root=mkdtempSync(join(tmpdir(),'agent-vm-materials-'));
  try {
    const source=join(root,'source'); mkdirSync(source); writeFileSync(join(source,'example.json'),'{}');
    const output=join(root,'output'); buildMaterials(source,output,'a'.repeat(40));
    const manifest=JSON.parse(readFileSync(join(output,'materials-manifest.json'),'utf8'));
    expect(manifest.source_revision).toBe('a'.repeat(40));
    expect(manifest.files[0].sha256).toHaveLength(64);
    expect(() => buildMaterials(source,output,'a'.repeat(40))).toThrow();
    symlinkSync('/etc/passwd',join(source,'escape'));
    expect(() => buildMaterials(source,join(root,'rejected'),'a'.repeat(40))).toThrow();
    expect(existsSync(join(root,'rejected'))).toBe(false);
  } finally {rmSync(root,{recursive:true,force:true});}
});
