import type { Item, Node, Root } from 'fumadocs-core/page-tree';
import { catalog } from '../content/catalog';

function pages(section: string): Item[] {
  return catalog.filter(doc => doc.section === section).map(doc => ({
    type: 'page', name: doc.title, url: '/docs/' + doc.slug,
  }));
}

function folder(name: string, section: string): Node {
  return { type: 'folder', name, children: pages(section) };
}

export const pageTree: Root = {
  name: 'Agent VM 文档',
  children: [
    folder('开始使用', '开始使用'),
    folder('使用指南', '使用指南'),
    folder('部署运维', '部署运维'),
    { type: 'folder', name: '开发文档', children: [
      ...pages('开发文档'),
      folder('文档站', '文档站'),
    ] },
  ],
};
