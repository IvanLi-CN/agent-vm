import {execFileSync} from 'node:child_process';
export function repositoryIdentity(root:string) {
  const git=(...args:string[])=>execFileSync('git',args,{cwd:root,encoding:'utf8'}).trim();
  const revision = git('rev-parse','HEAD');
  const dirty = !!git('status','--porcelain','--untracked-files=normal');
  const description = git('describe','--tags','--match','v[0-9]*','--long','--always','--abbrev=7');
  const tagged = description.match(/^(v\d+\.\d+\.\d+(?:-[\w.-]+)?)-(\d+)-g[0-9a-f]+$/);
  const label = tagged ? tagged[1] + (tagged[2] !== '0' || dirty ? '-dev' : '') : 'dev';
  return {
    revision, dirty,
    version: { label, description: description + (dirty ? '-dirty' : '') },
    modified:(path:string)=>!!git('status','--porcelain','--untracked-files=all','--',path),
  };
}
