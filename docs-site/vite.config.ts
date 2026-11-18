import { defineConfig, type Plugin } from 'vite';
import { reactRouter } from '@react-router/dev/vite';
import tailwind from '@tailwindcss/vite';
import { fumadocsMdx } from 'fumadocs-mdx/vite';
import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
function sourceWatch(): Plugin {
  return { name: 'documentation-source-watch', configureServer(server) {
    const root = resolve(import.meta.dirname, '..');
    const sourceDirs = ['docs', 'deploy', 'images'].map(path => resolve(root, path));
    const rootFiles = ['README.md', 'PRODUCT.md', 'CONTRIBUTING.md', 'GLOSSARY.md', 'DESIGN.md'].map(path => resolve(root, path));
    const siteFiles = [resolve(root, 'docs-site/content/catalog.ts'), resolve(root, 'docs-site/app/page-tree.ts')];
    const watched = [...sourceDirs, ...rootFiles, ...siteFiles];
    server.watcher.add(watched);
    let timer: ReturnType<typeof setTimeout>;
    server.watcher.on('all', (_, path) => {
      const sourceChanged = sourceDirs.some(dir => path === dir || path.startsWith(dir + '/')) || rootFiles.includes(path);
      const siteChanged = siteFiles.includes(path);
      if (!sourceChanged && !siteChanged) return;
      clearTimeout(timer);
      timer = setTimeout(() => {
        try {
          if (sourceChanged || siteChanged && path.endsWith('catalog.ts')) execFileSync('bun', ['content/generate.ts'], { cwd: import.meta.dirname, stdio: 'inherit' });
          server.ws.send({ type: 'full-reload' });
        }
        catch (error) { server.config.logger.error(String(error)); }
      }, 100);
    });
    server.httpServer?.once('close', () => clearTimeout(timer));
  } };
}
export default defineConfig({ plugins: [sourceWatch(), tailwind(), fumadocsMdx({ macro: false }), reactRouter()], server: { port: Number(process.env.DOCS_PORT ?? 0), strictPort: true } });
