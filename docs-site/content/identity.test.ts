import {test,expect} from 'bun:test';
import {mkdtemp,writeFile,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {execFileSync} from 'node:child_process';
import {repositoryIdentity} from './identity';
test('clean, modified and untracked source identities are truthful',async()=>{
  const root=await mkdtemp(join(tmpdir(),'docs-source-identity-'));
  const git=(...args:string[])=>execFileSync('git',args,{cwd:root,stdio:'pipe'});
  try {
    git('init');await writeFile(join(root,'a.md'),'source');git('add','a.md');
    git('-c','user.name=Documentation Test','-c','user.email=docs@example.invalid','-c','commit.gpgsign=false','commit','--signoff','-m','test: source fixture');
    const clean=repositoryIdentity(root);expect(clean.dirty).toBe(false);expect(clean.version.label).toBe('dev');
    git('tag','v1.2.3');expect(repositoryIdentity(root).version.label).toBe('v1.2.3');
    await writeFile(join(root,'a.md'),'modified');await writeFile(join(root,'untracked.md'),'new');
    const draft=repositoryIdentity(root);expect(draft.revision).toBe(clean.revision);
    expect(draft.dirty).toBe(true);expect(draft.version.label).toBe('v1.2.3-dev');expect(draft.version.description).toEndWith('-dirty');expect(draft.modified('a.md')).toBe(true);expect(draft.modified('untracked.md')).toBe(true);
    git('add','.');git('-c','user.name=Documentation Test','-c','user.email=docs@example.invalid','-c','commit.gpgsign=false','commit','--signoff','-m','test: advance source fixture');
    const ahead=repositoryIdentity(root);expect(ahead.dirty).toBe(false);expect(ahead.version.label).toBe('v1.2.3-dev');expect(ahead.version.description).toStartWith('v1.2.3-1-g');
  } finally {await rm(root,{recursive:true,force:true});}
});
