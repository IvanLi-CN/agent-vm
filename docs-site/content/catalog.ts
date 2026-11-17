// Reader-facing titles and navigation are independent from repository filenames.
export const catalog = [
  { path: 'README.md', slug: 'overview', title: '项目概览', section: '开始使用' },
  { path: 'docs/specs/agent-vm/contracts/cli.md', slug: 'cli', title: 'CLI 使用与命令', section: '开始使用' },
  { path: 'docs/architecture/system.md', slug: 'architecture', title: '系统架构', section: '开发文档' },
  { path: 'GLOSSARY.md', slug: 'glossary', title: '领域术语', section: '开发文档' },
  { path: 'docs/proposals/agent-vm-documentation-stack.md', slug: 'docs-site-design', title: '文档站工程方案', section: '文档工程' },
  { path: 'docs/proposals/agent-vm-documentation-source.md', slug: 'docs-site-content', title: '内容来源与适配', section: '文档工程' },
  { path: 'docs/specs/documentation-review/SPEC.md', slug: 'docs-site-requirements', title: '文档站需求与验收', section: '文档工程' },
  { path: 'docs/specs/documentation-review/IMPLEMENTATION.md', slug: 'docs-site-implementation', title: '实现与验证', section: '文档工程' },
  { path: 'docs/proposals/agent-vm-documentation-review.md', slug: 'docs-site-scope', title: '范围说明', section: '文档工程' },
  { path: 'docs/specs/documentation-review/HISTORY.md', slug: 'docs-site-background', title: '主题背景', section: '文档工程' },
] as const;
