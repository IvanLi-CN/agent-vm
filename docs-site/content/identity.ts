import {execFileSync} from 'node:child_process';
export function repositoryIdentity(root:string) {
  const git=(...args:string[])=>execFileSync('git',args,{cwd:root,encoding:'utf8'}).trim();
  return {
    revision:git('rev-parse','HEAD'),
    dirty:!!git('status','--porcelain','--untracked-files=normal'),
    modified:(path:string)=>!!git('status','--porcelain','--untracked-files=all','--',path),
  };
}
