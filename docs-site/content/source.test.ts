import { test, expect } from 'bun:test';
import { adaptLinks, contentDigest, headings, sourceAnchorLine } from './source';
test('identity is order independent and changes with source or configuration', () => {
  expect(contentDigest({a:'文档',b:'config'})).toBe(contentDigest({b:'config',a:'文档'}));
  expect(contentDigest({a:'文档',b:'config'})).not.toBe(contentDigest({a:'文档修改',b:'config'}));
});
test('links use pages, fixed commits and modified snapshots without rewriting code', async () => {
  const text = '# 中文\n[page](other.md#章节)\n[dirty](draft.md)\n[clean][ref]\n\n[ref]: clean.md\n\n```md\n[fake](secret.md)\n```';
  const source = {path:'docs/a.md',text,slug:'0'};
  const output = await adaptLinks(source, [source,{path:'docs/other.md',text:'',slug:'1'}], 'abc', '/repo', async () => new TextEncoder().encode('draft'), p=>p==='docs/draft.md');
  expect(output.text).toContain('/docs/1#章节');
  expect(output.text).toContain('/source?path=docs%2Fdraft.md');
  expect(output.text).toContain('blob/abc/docs/clean.md');
  expect(output.text).toContain('[fake](secret.md)');
  expect(output.snapshots['docs/draft.md']).toBe('draft');
});
test('missing and escaping references are diagnosed without reading outside root', async () => {
  const source={path:'docs/a.md',text:'[escape](../../secret.md)\n[missing](absent.md)',slug:'0'};
  const reads:string[]=[];
  const output=await adaptLinks(source,[source],'abc','/repo',async p=>{reads.push(p);throw Error('missing')},()=>false);
  expect(output.diagnostics).toHaveLength(2);
  expect(reads).toEqual(['docs/absent.md']);
});
test('Chinese and duplicate heading anchors are deterministic',()=>{
  expect(headings('# 中文\n## 中文').map(h=>h.anchor)).toEqual(['中文','中文-1']);
});
test('modified snapshot fragments resolve to real source lines', async () => {
  const text = '# 中文\n\n## 中文\n\n<a id="details"></a>\n\n```html\n<a id="fake"></a>\n```';
  expect(sourceAnchorLine(text, '中文-1')).toBe(3);
  expect(sourceAnchorLine(text, 'details')).toBe(5);
  expect(sourceAnchorLine(text, 'fake')).toBeUndefined();
  const source = {path:'docs/a.md', text:'[章节](draft.md#%E4%B8%AD%E6%96%87-1)\n[显式](draft.md#details)', slug:'a'};
  const result = await adaptLinks(source, [source], 'abc', '/repo', async () => new TextEncoder().encode(text), () => true);
  expect(result.text).toContain('/source?path=docs%2Fdraft.md&line=3');
  expect(result.text).toContain('/source?path=docs%2Fdraft.md&line=5');
  expect(result.diagnostics).toEqual([]);
});
test('asset references use content-addressed outputs and retain bytes',async()=>{
 const source={path:'docs/a.md',text:'![图](image.svg)',slug:'0'};
 const bytes=new TextEncoder().encode('<svg/>');
 const result=await adaptLinks(source,[source],'abc','/repo',async()=>bytes,()=>false);
 expect(result.text).toContain('/assets/sources/');expect(Object.values(result.assets)[0]).toEqual(bytes);
});
test('MDX JSX remains executable source and inline Markdown links are adapted',async()=>{
 const source={path:'docs/a.mdx',text:'import X from "./component";\n\n<X />\n\n[正文](b.md)',slug:'0'};
 const result=await adaptLinks(source,[source,{path:'docs/b.md',text:'',slug:'1'}],'abc','/repo',async()=>new Uint8Array(),()=>false);
 expect(result.text).toContain('<X />');expect(result.text).toContain('/docs/1');
});
