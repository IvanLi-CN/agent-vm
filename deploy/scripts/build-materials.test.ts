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
    // BSD tar hides AppleDouble metadata in its own listing. Inspect raw
    // members with Python so a macOS-built archive remains valid on Linux.
    const listing=Bun.spawnSync(['python3','-c','import json,sys,tarfile; t=tarfile.open(sys.argv[1]); assert all(m.uid==0 and m.gid==0 and not m.pax_headers for m in t.getmembers()); print(json.dumps(t.getnames()))',output],{stdout:'pipe',stderr:'pipe'});
    expect(listing.exitCode).toBe(0);
    expect(JSON.parse(new TextDecoder().decode(listing.stdout)).sort()).toEqual(['README.md','materials-manifest.json']);
    const manifest=Bun.spawnSync(['tar','-xOf',output,'materials-manifest.json'],{stdout:'pipe',stderr:'pipe'});
    expect(JSON.parse(new TextDecoder().decode(manifest.stdout)).source_revision).toBe(revision);
    // Execute the documented extraction flags under a strict operator umask.
    const extracted=join(root,'extracted');mkdirSync(extracted);
    const extraction=Bun.spawnSync(['python3','-c',
      'import os,subprocess,sys; os.umask(0o077); subprocess.run(["tar","--extract","--file",sys.argv[1],"--directory",sys.argv[2],"--no-same-owner","--same-permissions","--keep-old-files"],check=True)',output,extracted],{stdout:'pipe',stderr:'pipe'});
    expect(extraction.exitCode).toBe(0);
    const permissions=Bun.spawnSync(['python3','-c',
      'import json,pathlib,sys; p=pathlib.Path(sys.argv[1]); m=json.loads((p/"materials-manifest.json").read_text()); assert all((p/f["path"]).stat().st_mode & 0o777 == f["mode"] for f in m["files"])',extracted],{stdout:'pipe',stderr:'pipe'});
    expect(permissions.exitCode).toBe(0);
    writeFileSync(join(source,'deploy','README.md'),'changed');
    expect(()=>buildMaterialsArchive(source,join(root,'dirty.tar'),revision)).toThrow('clean matching source revision');
  } finally {rmSync(root,{recursive:true,force:true});}
});

test('release materials project moved observability sources into the legacy package paths', async () => {
  const {buildMaterialsArchive}=await import('./build-materials.ts');
  const root=mkdtempSync(join(tmpdir(),'deployment-projection-'));
  try {
    const files: Array<[string, string]> = [
      ['deploy/scripts/keep.sh', '#!/bin/sh\n'],
      ['deploy/scripts/agent_vm_state_migration.ts', 'legacy migration\n'],
      ['deploy/scripts/render_observability_dashboard.ts', 'legacy renderer\n'],
      ['deploy/scripts/validate_observability_dashboard.ts', 'legacy validator\n'],
      ['deploy/observability/obsolete.txt', 'legacy asset\n'],
      ['skills/agent-vm-ops/scripts/agent_vm_state_migration.ts', 'ops migration\n'],
      ['skills/agent-vm-ops/scripts/render_observability_dashboard.ts', 'ops renderer\n'],
      ['skills/agent-vm-ops/scripts/validate_observability_dashboard.ts', 'ops validator\n'],
      ['skills/agent-vm-ops/references/observability.md', 'ops reference\n'],
      ['skills/agent-vm-ops/assets/observability/dashboard.json.template', 'ops dashboard\n'],
    ];
    for (const [relative, content] of files) {
      const path = join(root, relative);
      mkdirSync(path.slice(0, path.lastIndexOf('/')), { recursive: true });
      writeFileSync(path, content);
    }
    const git=(args:string[])=>{
      const result=Bun.spawnSync(['git','-C',root,...args],{stdout:'pipe',stderr:'pipe'});
      expect(result.exitCode).toBe(0);return new TextDecoder().decode(result.stdout).trim();
    };
    git(['init','--initial-branch=main']);git(['add','.']);
    git(['-c','user.name=Test','-c','user.email=test@example.invalid','commit','--signoff','-m','test: project deployment materials']);
    const revision=git(['rev-parse','HEAD']);
    const output=join(root,'materials.tar');
    buildMaterialsArchive(root,output,revision);
    const listing=Bun.spawnSync(['tar','-tf',output],{stdout:'pipe',stderr:'pipe'});
    expect(listing.exitCode).toBe(0);
    const members=new TextDecoder().decode(listing.stdout).trim().split('\n').filter(Boolean).sort();
    expect(members).toEqual([
      'materials-manifest.json',
      'observability/README.md',
      'observability/dashboard.json.template',
      'scripts/agent_vm_state_migration.ts',
      'scripts/keep.sh',
      'scripts/render_observability_dashboard.ts',
      'scripts/validate_observability_dashboard.ts',
    ]);
    const projected=Bun.spawnSync(['tar','-xOf',output,'scripts/render_observability_dashboard.ts'],{stdout:'pipe',stderr:'pipe'});
    expect(new TextDecoder().decode(projected.stdout)).toBe('ops renderer\n');
    const manifest=JSON.parse(new TextDecoder().decode(Bun.spawnSync(['tar','-xOf',output,'materials-manifest.json'],{stdout:'pipe',stderr:'pipe'}).stdout));
    expect(manifest.source_revision).toBe(revision);
    expect(manifest.files.map((file:{path:string})=>file.path).sort()).toEqual(members.filter((member)=>member!=='materials-manifest.json').sort());
  } finally {rmSync(root,{recursive:true,force:true});}
});
