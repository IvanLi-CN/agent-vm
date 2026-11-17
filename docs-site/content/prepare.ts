import { compile } from '@mdx-js/mdx';
import gfm from 'remark-gfm';
import { unified } from 'unified';
import parse from 'remark-parse';
import frontmatter from 'remark-frontmatter';
import {parse as parseYaml, stringify as yaml} from 'yaml';
export function titleMetadata(text:string,fallback:string) {
  const tree=unified().use(parse).use(frontmatter,['yaml']).parse(text);
  const first=tree.children[0];
  const metadata=first?.type==='yaml'?parseYaml(first.value):{};
  if(metadata!==null&&(typeof metadata!=='object'||Array.isArray(metadata)))throw new Error('Frontmatter must be an object');
  const body=first?.type==='yaml'?text.slice(first.position?.end.offset??0).replace(/^\r?\n/,''):text;
  return {body,prefix:`---\n${yaml({...metadata,title:metadata?.title??fallback})}---\n\n`};
}
// A single source syntax failure produces an explicit error page; toolchain
// failures still fail normally in Vite/Fumadocs.
export async function prepareBody(path: string, text: string, title=path) {
  try {
    const {body,prefix}=titleMetadata(text,title);
    await compile(body, { format: path.endsWith('.mdx') ? 'mdx' : 'md', remarkPlugins: [gfm] });
    return { text:prefix+body };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    return { error: message, text: `---\ntitle: ${JSON.stringify(title)}\n---\n\n# 正文无法呈现\n\n此来源存在语法问题。请使用页面上方的“查看构建来源”检查实际正文。\n\n来源：\`${path}\`\n\n\`\`\`text\n${message.replaceAll('```', '~~~')}\n\`\`\`\n` };
  }
}
