import { defineDocs, defineConfig } from 'fumadocs-mdx/config';
import { remarkMermaid } from './content/remark-mermaid';
export const docs = defineDocs({ dir: '.generated/content' });
export const excerpts = defineDocs({dir:'.generated/excerpts'});
export default defineConfig({ mdxOptions: { remarkPlugins: [remarkMermaid] } });
