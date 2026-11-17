import type { Item, Root } from 'fumadocs-core/page-tree';
import { catalog } from '../content/catalog';

function pages(section: string): Item[] {
  return catalog.filter(doc => doc.section === section).map(doc => ({
    type: 'page', name: doc.title, url: '/docs/' + doc.slug,
  }));
}

export const pageTree: Root = {
  name: 'Agent VM 文档',
  children: [
    { type: 'folder', name: '开始使用', children: pages('开始使用') },
    { type: 'folder', name: '开发文档', children: [
      ...pages('开发文档'),
      { type: 'folder', name: '文档站', children: pages('文档工程') },
    ] },
  ],
};
