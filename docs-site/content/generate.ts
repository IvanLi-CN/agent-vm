import { mkdir, readFile, writeFile, readdir, rm } from 'node:fs/promises';
import { resolve, extname } from 'node:path';
import { realpath } from 'node:fs/promises';
import { repositoryIdentity } from './identity';
import { catalog } from './catalog';
import { prepareBody } from './prepare';
import { adaptLinks, contentDigest, headings } from './source';
const root = resolve(import.meta.dir, '../..'), out = resolve(import.meta.dir, '../.generated');
const documents = await Promise.all(catalog.map(async entry => {
  const text = await readFile(resolve(root, entry.path), 'utf8');
  return { ...entry, text, toc: headings(text) };
}));
const {revision,dirty,modified,version} = repositoryIdentity(root);
const snapshots: Record<string, string> = Object.fromEntries(documents.map(d => [d.path, d.text]));
const inputs: Record<string, string | Uint8Array> = { ...snapshots };
const contentDiagnostics: { path: string; message: string }[] = [];
for (const path of [resolve(out,'content'),resolve(root,'docs-site/public/assets/sources')]) await rm(path,{recursive:true,force:true});
await mkdir(resolve(out, 'content'), { recursive: true });
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
await writeFile(resolve(out, 'source.json'), JSON.stringify({ documents, identity: { revision, dirty, sourceId, version }, snapshots, contentDiagnostics }, null, 2));
console.log(`Documentation: ${documents.length} sources, ${contentDiagnostics.length} diagnostics; ${sourceId.slice(0, 12)}${dirty ? ' +modified' : ''}`);
if (process.argv.includes('--strict') && contentDiagnostics.length) {
  console.error(contentDiagnostics);
  process.exitCode = 1;
}
