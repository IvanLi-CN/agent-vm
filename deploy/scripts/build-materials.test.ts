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

test('release materials archive requires the exact clean revision', async () => {
  const {buildMaterialsArchive}=await import('./build-materials.ts');
  const root=mkdtempSync(join(tmpdir(),'deployment-archive-'));
  try {
    const source=join(root,'source');mkdirSync(join(source,'deploy'),{recursive:true});
    writeFileSync(join(source,'deploy','README.md'),'# 部署资料\n',{mode:0o644});
    const git=(args:string[])=>{
      const result=Bun.spawnSync(['git','-C',source,...args],{stdout:'pipe',stderr:'pipe'});
      expect(result.exitCode).toBe(0);return new TextDecoder().decode(result.stdout).trim();
    };
    git(['init','--initial-branch=main']);git(['add','deploy']);
    git(['-c','user.name=Test','-c','user.email=test@example.invalid','commit','--signoff','-m','test: materials fixture']);
    const revision=git(['rev-parse','HEAD']);
    const output=join(root,'materials.tar');
    buildMaterialsArchive(source,output,revision);
    const listing=Bun.spawnSync(['tar','-tf',output],{stdout:'pipe',stderr:'pipe'});
    expect(listing.exitCode).toBe(0);
    expect(new TextDecoder().decode(listing.stdout)).toContain('materials-manifest.json');
    const manifest=Bun.spawnSync(['tar','-xOf',output,'materials-manifest.json'],{stdout:'pipe',stderr:'pipe'});
    expect(JSON.parse(new TextDecoder().decode(manifest.stdout)).source_revision).toBe(revision);
    writeFileSync(join(source,'deploy','README.md'),'changed');
    expect(()=>buildMaterialsArchive(source,join(root,'dirty.tar'),revision)).toThrow('clean matching source revision');
  } finally {rmSync(root,{recursive:true,force:true});}
});
