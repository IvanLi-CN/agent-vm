import { mkdir, readFile, writeFile, readdir, rm } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { realpath } from 'node:fs/promises';
import { repositoryIdentity } from './identity';
import { prepareBody } from './prepare';
import { trace } from './trace';
import { adaptLinks, contentDigest, headings } from './source';
const root = resolve(import.meta.dir, '../..'), out = resolve(import.meta.dir, '../.generated');
export const paths = ['docs/specs/documentation-review/SPEC.md', 'docs/specs/documentation-review/IMPLEMENTATION.md', 'docs/specs/documentation-review/HISTORY.md', 'docs/proposals/agent-vm-documentation-review.md', 'docs/proposals/agent-vm-documentation-source.md', 'docs/proposals/agent-vm-documentation-stack.md', 'GLOSSARY.md'];
const documents = await Promise.all(paths.map(async (path, index) => {
  const text = await readFile(resolve(root, path), 'utf8');
  return { path, text, slug: String(index), toc: headings(text) };
}));
const {revision,dirty,modified} = repositoryIdentity(root);
const result = trace(documents);
const snapshots: Record<string, string> = Object.fromEntries(documents.map(d => [d.path, d.text]));
const inputs: Record<string, string | Uint8Array> = { ...snapshots };
const contentDiagnostics: { path: string; message: string }[] = [];
for (const path of [resolve(out,'content'),resolve(out,'excerpts'),resolve(root,'docs-site/public/assets/sources')]) await rm(path,{recursive:true,force:true});
await mkdir(resolve(out, 'content'), { recursive: true });
await mkdir(resolve(out, 'excerpts'), { recursive: true });
await mkdir(resolve(root, 'docs-site/public/assets/sources'), { recursive: true });
for (const d of documents) {
  const adapted = await adaptLinks(d, documents, revision, root, async path => {
    const absolute = await realpath(resolve(root,path));
    if (!absolute.startsWith(root+'/')) throw new Error('Reference resolves outside repository');
    return readFile(absolute);
  }, modified);
  Object.assign(snapshots, adapted.snapshots);
  Object.assign(inputs, adapted.snapshots);
  for (const [name, bytes] of Object.entries(adapted.assets)) {
    inputs[`assets/${name}`] = bytes;
    await writeFile(resolve(root, 'docs-site/public/assets/sources', name), bytes);
  }
  for (const message of adapted.diagnostics) contentDiagnostics.push({ path: d.path, message });
  for (const ref of [...result.requirements,...result.requirements.flatMap(r=>[...r.design,...r.verification])].filter(ref=>ref.path===d.path)) {
    const lines=adapted.text.split('\n');
    const end=lines.findIndex((line,index)=>index>=ref.line&&/^#{1,6} /.test(line));
    const excerpt=lines.slice(ref.line,end<0?undefined:end).join('\n').replace(/^- (?:design-id|covers|trace-note):.*$/gm,'');
    const preparedExcerpt=await prepareBody(d.path,excerpt,ref.title);
    await writeFile(resolve(out,'excerpts',ref.id+'.md'),preparedExcerpt.text);
  }
  const prepared = await prepareBody(d.path, adapted.text, d.toc[0]?.title ?? d.path);
  if (prepared.error) contentDiagnostics.push({path:d.path,message:prepared.error});
  await writeFile(resolve(out, 'content', d.slug + extname(d.path)), prepared.text);
}
// Include all maintained rendering/configuration code, not machine paths or generated files.
async function addInputs(dir: string) {
  for (const entry of await readdir(resolve(root, dir), { withFileTypes: true })) {
    const path = `${dir}/${entry.name}`;
    if (['node_modules', '.generated', '.react-router', '.source', 'build', 'public', 'test-results', 'playwright-report', 'storybook-static'].includes(entry.name)) continue;
    if (entry.isDirectory()) await addInputs(path);
    else inputs[path] = await readFile(resolve(root, path));
  }
}
await addInputs('docs-site');
for (const path of ['bun.lock', 'package.json']) inputs[path] = await readFile(resolve(root, path));
const sourceId = contentDigest(inputs);
await writeFile(resolve(out, 'review.json'), JSON.stringify({ ...result, documents, identity: { revision, dirty, sourceId }, snapshots, contentDiagnostics }, null, 2));
console.log(`Documentation: ${documents.length} sources, ${result.requirements.length} requirements, ${result.diagnostics.length + contentDiagnostics.length} diagnostics; ${sourceId.slice(0, 12)}${dirty ? ' +modified' : ''}`);
if (process.argv.includes('--strict') && (result.diagnostics.length || contentDiagnostics.length)) {
  console.error(result.diagnostics, contentDiagnostics);
  process.exitCode = 1;
}
