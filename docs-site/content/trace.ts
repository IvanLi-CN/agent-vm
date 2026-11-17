import { unified } from 'unified';
import remarkParse from 'remark-parse';
import remarkGfm from 'remark-gfm';
import remarkFrontmatter from 'remark-frontmatter';
import { toString } from 'mdast-util-to-string';
import GithubSlugger from 'github-slugger';
export type Ref = { id: string; title: string; path: string; anchor: string; line: number; covers: string[]; note?: string; excerpt?: string };
export type Diagnostic = { kind: 'missing' | 'broken' | 'unparseable'; message: string; path: string; line: number };
export function parseDocument(path: string, text: string) {
  const tree = unified().use(remarkParse).use(remarkGfm).use(remarkFrontmatter, ['yaml']).parse(text);
  const refs: Ref[] = [], diagnostics: Diagnostic[] = []; const slugger = new GithubSlugger();
  let current: Ref | undefined;
  for (const node of tree.children) {
    if (node.type === 'heading') {
      const title = toString(node), anchor = slugger.slug(title);
      const id = title.match(/^(REQ-[\w-]+|VER-[\w-]+)/)?.[1];
      current = { id: id ?? '', title, path, anchor, line: node.position?.start.line ?? 1, covers: [] };
      if (id) refs.push(current);
    } else if (node.type === 'list' && current) {
      for (const item of node.children) {
        const value = toString(item);
        if (value.startsWith('design-id:')) {
          const id = value.slice(10).trim();
          if (!/^DESIGN-[A-Z0-9-]+$/.test(id)) diagnostics.push({kind:'unparseable',message:'Invalid design-id',path,line:item.position?.start.line ?? 1});
          else if (current.id) diagnostics.push({kind:'unparseable',message:'Multiple IDs in one section',path,line:item.position?.start.line ?? 1});
          else { current.id=id; refs.push(current); }
        } else if (value.startsWith('covers:')) {
          const ids = value.slice(7).split(',').map(s=>s.trim()).filter(Boolean);
          if (!ids.length || ids.some(id=>!/^REQ-[A-Z0-9-]+$/.test(id))) diagnostics.push({kind:'unparseable',message:'Invalid covers declaration',path,line:item.position?.start.line ?? 1});
          else current.covers.push(...ids);
        } else if (value.startsWith('trace-note:')) current.note=value.slice(11).trim();
      }
    }
  }
  for (const ref of refs) {
    const lines = text.split('\n');
    const end = lines.findIndex((line, index) => index >= ref.line && /^#{1,6} /.test(line));
    ref.excerpt = lines.slice(ref.line, end < 0 ? undefined : end).join('\n').trim();
  }
  return {refs,diagnostics};
}
export function trace(documents: {path:string;text:string}[]) {
  const parsed=documents.map(d=>parseDocument(d.path,d.text));
  const refs=parsed.flatMap(d=>d.refs), diagnostics=parsed.flatMap(d=>d.diagnostics);
  const counts=new Map<string,number>(); refs.forEach(r=>counts.set(r.id,(counts.get(r.id)??0)+1));
  refs.forEach(r=>{ if(counts.get(r.id)!==1) diagnostics.push({kind:'unparseable',message:`Duplicate ID: ${r.id}`,path:r.path,line:r.line}); });
  const unique=refs.filter(r=>counts.get(r.id)===1);
  for(const r of unique) for(const id of r.covers) if(!unique.some(x=>x.id===id && x.id.startsWith('REQ-'))) diagnostics.push({kind:'broken',message:`${r.id} references unknown ${id}`,path:r.path,line:r.line});
  const requirements=unique.filter(r=>r.id.startsWith('REQ-')).map(r=>{
    const design=unique.filter(x=>x.id.startsWith('DESIGN-')&&x.covers.includes(r.id));
    const verification=unique.filter(x=>x.id.startsWith('VER-')&&x.covers.includes(r.id));
    if((!design.length||!verification.length)&&!r.note) diagnostics.push({kind:'missing',message:`${r.id}: missing ${!design.length?'design':'verification'}`,path:r.path,line:r.line});
    return {...r,design,verification};
  });
  return {requirements,diagnostics};
}
