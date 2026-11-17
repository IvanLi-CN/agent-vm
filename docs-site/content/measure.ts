import { readFile, writeFile, rm } from 'node:fs/promises';
import { resolve } from 'node:path';
import { tmpdir, platform, arch, cpus } from 'node:os';
const root=resolve(import.meta.dir,'../..');
const port=process.env.DOCS_PORT;if(!port)throw new Error('DOCS_PORT must be leased before measuring');
async function build(label:string){const start=performance.now();const child=Bun.spawn(['bun','run','docs:build'],{cwd:root,stdout:Bun.file(resolve(tmpdir(),`agent-vm-docs-${label}.log`)),stderr:'inherit'});if(await child.exited)throw new Error(`${label} build failed`);return Math.round(performance.now()-start);}
// Only remove this subproject's known generated cache/output, never source files.
await rm(resolve(root,'docs-site/.source'),{recursive:true,force:true});
await rm(resolve(root,'docs-site/build'),{recursive:true,force:true});
const coldBuildMs=await build('cold');
const fullPrerenderMs=await build('prerender');
const dev=Bun.spawn(['bun','run','docs:dev'],{cwd:root,env:process.env,detached:true,stdout:Bun.file(resolve(tmpdir(),'agent-vm-docs-dev-measure.log')),stderr:'inherit'});
const url=`http://127.0.0.1:${port}`;
const path=resolve(root,'docs/specs/documentation-review/HISTORY.md'),original=await readFile(path,'utf8');
let singleDocumentRecompileMs=0;
try{
  let ready=false;
  for(let i=0;i<100;i++){try{if((await fetch(url)).ok){ready=true;break;}}catch{}await Bun.sleep(100);}
  if(!ready)throw new Error('Development server unavailable');
  const marker=`docs-recompile-probe-${Date.now()}`,start=performance.now();
  await writeFile(path,original+'\n\n'+marker+'\n');
  for(let i=0;i<100;i++){
    const response=await fetch(url+'/.generated/content/2.md?import');
    if(response.ok&&(await response.text()).includes(marker)){singleDocumentRecompileMs=Math.round(performance.now()-start);break;}
    await Bun.sleep(50);
  }
  if(!singleDocumentRecompileMs)throw new Error('Source change did not reach transformed module');
}finally{await writeFile(path,original);try{process.kill(-dev.pid,'SIGTERM');}catch{}await dev.exited;}
// Restore static output after the transient, fully restored source probe.
await build('restored');
const source=JSON.parse(await readFile(resolve(root,'docs-site/.generated/source.json'),'utf8'));
const result={measuredAt:new Date().toISOString(),environment:{bun:Bun.version,node:Bun.spawnSync(['node','--version']).stdout.toString().trim(),platform:platform(),arch:arch(),cpu:cpus()[0]?.model},source:source.identity,coldBuildMs,fullPrerenderMs,singleDocumentRecompileMs,method:'cold: cleared docs-site .source/build; full: repeat static prerender; edit: source write through generated MDX Vite module response; dependencies installed; no Go build'};
await writeFile(resolve(root,'docs-site/.generated/performance.json'),JSON.stringify(result,null,2));console.log(JSON.stringify(result,null,2));
