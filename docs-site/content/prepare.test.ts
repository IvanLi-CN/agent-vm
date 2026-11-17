import {test,expect} from 'bun:test';
import {prepareBody} from './prepare';
test('MD literal syntax is preserved while invalid MDX yields a source error',async()=>{
  const text='# 中文\n\n<div>unclosed';
  expect((await prepareBody('a.md',text)).text).toContain(text);
  const result=await prepareBody('a.mdx',text);
  expect(result.error).toBeDefined();
  expect(result.text).toContain('正文无法呈现');
});

test('existing title metadata is merged once without leaking YAML into the body',async()=>{
 const result=await prepareBody('a.mdx','---\ntitle: 已有标题\n---\n\n# 正文');
 expect(result.error).toBeUndefined();expect(result.text.match(/title:/g)).toHaveLength(1);expect(result.text).toContain('已有标题');
});
test('source syntax errors remain readable but compiler failures propagate', async () => {
  const malformed = await prepareBody('a.md', '---\ntitle: [\n---\n# 文档');
  expect(malformed.error).toBeDefined();
  const invalidMetadata = await prepareBody('a.md', '---\n- invalid\n---\n# 文档');
  expect(invalidMetadata.error).toBeDefined();
  const failure = new Error('Compiler unavailable');
  await expect(prepareBody('a.md', '# 文档', '文档', async () => { throw failure; })).rejects.toBe(failure);
});
