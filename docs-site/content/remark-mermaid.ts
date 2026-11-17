import { visit } from 'unist-util-visit';
import type { Root } from 'mdast';
export function remarkMermaid() {
  return (tree: Root) => visit(tree, 'code', (node, index, parent) => {
    if (node.lang !== 'mermaid' || index === undefined || !parent) return;
    (parent.children as unknown[])[index] = { type: 'mdxJsxFlowElement', name: 'Mermaid', attributes: [{ type: 'mdxJsxAttribute', name: 'code', value: node.value }], children: [] };
  });
}
