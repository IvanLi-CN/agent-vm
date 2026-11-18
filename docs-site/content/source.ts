import { createHash } from 'node:crypto';
import { dirname, relative, resolve, extname } from 'node:path';
import { unified } from 'unified';
import parse from 'remark-parse';
import mdx from 'remark-mdx';
import gfm from 'remark-gfm';
import frontmatter from 'remark-frontmatter';
import { visit } from 'unist-util-visit';
import GithubSlugger from 'github-slugger';
import { toString } from 'mdast-util-to-string';

export type Source = { path: string; text: string; slug: string };
export function contentDigest(inputs: Record<string, string | Uint8Array>) {
  const hash = createHash('sha256');
  for (const path of Object.keys(inputs).sort()) {
    const bytes = Buffer.from(inputs[path]!);
    hash.update(`${path.length}:${path}:${bytes.length}:`);
    hash.update(bytes);
  }
  return hash.digest('hex');
}
export function headings(text: string) {
  const tree = unified().use(parse).use(gfm).parse(text);
  const slugger = new GithubSlugger();
  return tree.children.flatMap(node => node.type === 'heading' ? [{ title: toString(node), anchor: slugger.slug(toString(node)), depth: node.depth, line: node.position?.start.line }] : []);
}
export function sourceAnchorLine(text: string, anchor: string) {
  const heading = headings(text).find(item => item.anchor === anchor);
  if (heading) return heading.line;
  const tree = unified().use(parse).use(gfm).use(frontmatter, ['yaml']).parse(text);
  let line: number | undefined;
  visit(tree, 'html', node => {
    for (const match of node.value.matchAll(/\b(?:id|name)\s*=\s*(["'])(.*?)\1/g)) {
      if (match[2] === anchor && line === undefined) line = (node.position?.start.line ?? 1) + node.value.slice(0, match.index).split('\n').length - 1;
    }
  });
  return line;
}
// Change URL fields in the syntax tree by their original spans. This preserves MD/MDX
// bodies verbatim, including JSX, explicit anchors, code fences and reference links.
export async function adaptLinks(source: Source, sources: Source[], revision: string, root: string,
  read: (path: string) => Promise<Uint8Array>, isModified: (path: string) => boolean) {
  const processor = unified().use(parse).use(gfm).use(frontmatter, ['yaml']);
  if (source.path.endsWith('.mdx')) processor.use(mdx);
  let tree;
  try { tree = processor.parse(source.text); } catch { return {text:source.text,snapshots:{},assets:{},diagnostics:['Cannot parse source syntax']}; }
  const edits: { start: number; end: number; value: string }[] = [];
  const snapshots: Record<string, string> = {};
  const assets: Record<string, Uint8Array> = {};
  const diagnostics: string[] = [];
  const nodes: any[] = [];
  visit(tree, node => { if (['link', 'image', 'definition'].includes(node.type)) nodes.push(node); });
  for (const node of nodes) {
    const target: string = node.url;
    if (!target || /^(?:[a-z][a-z0-9+.-]*:|#|\/)/i.test(target)) continue;
    const [file, suffix = ''] = target.split(/(?=[#?])/s, 2);
    let path: string;
    try { path = relative(root, resolve(dirname(resolve(root, source.path)), decodeURIComponent(file!))); }
    catch { diagnostics.push(`Invalid URL: ${target}`); continue; }
    if (path.startsWith('../') || path === '..' || path.startsWith('/')) { diagnostics.push(`Outside repository: ${target}`); continue; }
    let bytes: Uint8Array;
    try { bytes = await read(path); } catch { diagnostics.push(`Missing reference: ${path}`); continue; }
    const page = sources.find(s => s.path === path);
    let url: string;
    if (page) {
      url = `/docs/${page.slug}${suffix}`;
      if (suffix.startsWith('#')) {
        let anchor = suffix.slice(1);
        try { anchor = decodeURIComponent(anchor); } catch { /* Retain the literal malformed fragment. */ }
        if (sourceAnchorLine(page.text, anchor) === undefined) diagnostics.push(`Missing page anchor: ${path}#${anchor}`);
      }
    }
    else if (!['.md', '.mdx'].includes(extname(path))) {
      const asset = `${contentDigest({ [path]: bytes }).slice(0, 16)}${extname(path)}`;
      assets[asset] = bytes;
      url = `/assets/sources/${asset}${suffix}`;
    } else if (isModified(path)) {
      snapshots[path] = new TextDecoder().decode(bytes);
      url = `/source?path=${encodeURIComponent(path)}`;
      if (suffix.startsWith('#')) {
        let anchor = suffix.slice(1);
        try { anchor = decodeURIComponent(anchor); } catch { /* Retain the literal malformed fragment. */ }
        const line = sourceAnchorLine(snapshots[path]!, anchor);
        if (line !== undefined) url += `&line=${line}`;
        else diagnostics.push(`Missing source anchor: ${path}#${anchor}`);
      }
    } else url = `https://github.com/IvanLi-CN/agent-vm/blob/${revision}/${path.split('/').map(encodeURIComponent).join('/')}${suffix}`;
    const start = node.position.start.offset, end = node.position.end.offset;
    const span = source.text.slice(start, end);
    // Markdown destination can use angle brackets. Do not touch labels with the same text.
    const offset = node.type === 'definition' ? span.indexOf(':') + 1 : span.lastIndexOf('](') + 2;
    const index = span.indexOf(target, offset);
    if (index >= 0) edits.push({ start: start + index, end: start + index + target.length, value: url });
    else diagnostics.push(`Cannot adapt reference: ${target}`);
  }
  let text = source.text;
  for (const edit of edits.sort((a, b) => b.start - a.start)) text = text.slice(0, edit.start) + edit.value + text.slice(edit.end);
  return { text, snapshots, assets, diagnostics };
}
