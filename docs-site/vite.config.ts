import { defineConfig, type Plugin } from 'vite';
import { reactRouter } from '@react-router/dev/vite';
import tailwind from '@tailwindcss/vite';
import { fumadocsMdx } from 'fumadocs-mdx/vite';
import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
function sourceWatch(): Plugin {
  return { name: 'documentation-source-watch', configureServer(server) {
    const root = resolve(import.meta.dirname, '..');
    server.watcher.add([resolve(root, 'docs'), resolve(root, 'GLOSSARY.md')]);
    let timer: ReturnType<typeof setTimeout>;
    server.watcher.on('all', (_, path) => {
      if (!path.startsWith(resolve(root, 'docs') + '/') && path !== resolve(root, 'GLOSSARY.md')) return;
      clearTimeout(timer);
      timer = setTimeout(() => {
        try { execFileSync('bun', ['content/generate.ts'], { cwd: import.meta.dirname, stdio: 'inherit' }); server.ws.send({ type: 'full-reload' }); }
        catch (error) { server.config.logger.error(String(error)); }
      }, 100);
    });
    server.httpServer?.once('close', () => clearTimeout(timer));
  } };
}
export default defineConfig({ plugins: [sourceWatch(), tailwind(), fumadocsMdx({ macro: false }), reactRouter()], server: { port: Number(process.env.DOCS_PORT ?? 0), strictPort: true } });
