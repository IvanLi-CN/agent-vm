#!/usr/bin/env bun
/** Produce revision-bound deployment materials without changing product bundles. */
import { createHash } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync, lstatSync, mkdtempSync, rmSync } from 'node:fs';
import { join, resolve, relative, isAbsolute, dirname } from 'node:path';

type MaterialEntry = { source: string; path: string; content?: Uint8Array; mode?: number };
type MaterialFile = { path: string; sha256: string; mode: number };

function collectMaterialEntries(directory: string, prefix: string, entries: MaterialEntry[]): void {
  for (const name of readdirSync(directory).sort()) {
    const path = join(directory, name);
    const relativePath = prefix ? join(prefix, name) : name;
    const info = lstatSync(path);
    if (info.isSymbolicLink()) throw new Error(`deployment materials cannot contain symlinks: ${relativePath}`);
    if (info.isDirectory()) collectMaterialEntries(path, relativePath, entries);
    else if (info.isFile()) entries.push({ source: path, path: relativePath });
    else throw new Error(`deployment materials contain a special file: ${relativePath}`);
  }
}

function buildMaterialsFromEntries(entries: MaterialEntry[], output: string, revision: string): void {
  if (!/^[a-f0-9]{40}$/.test(revision)) throw new Error('source revision must be an exact Git SHA');
  if (!isAbsolute(output) || existsSync(output)) throw new Error('output must be an absent absolute directory');
  const paths = new Set<string>();
  const files: MaterialFile[] = [];
  const sources = new Map<string, { content: Uint8Array; mode: number }>();
  for (const entry of [...entries].sort((left, right) => left.path.localeCompare(right.path))) {
    if (paths.has(entry.path)) throw new Error(`deployment materials contain duplicate path: ${entry.path}`);
    paths.add(entry.path);
    const info = lstatSync(entry.source, { throwIfNoEntry: false });
    if (!info || info.isSymbolicLink() || !info.isFile()) throw new Error(`deployment materials source is not a regular file: ${entry.path}`);
    const content = entry.content ?? readFileSync(entry.source);
    const mode = entry.mode ?? (info.mode & 0o777);
    sources.set(entry.path, { content, mode });
    files.push({ path: entry.path, sha256: createHash('sha256').update(content).digest('hex'), mode });
  }
  mkdirSync(output, { mode: 0o750 });
  for (const entry of files) {
    const destination = join(output, entry.path);
    mkdirSync(resolve(destination, '..'), { recursive: true, mode: 0o750 });
    const source = sources.get(entry.path)!;
    writeFileSync(destination, source.content, { mode: source.mode });
    chmodSync(destination, entry.mode);
  }
  writeFileSync(join(output, 'materials-manifest.json'), JSON.stringify({ schema_version: 1, source_revision: revision, files }, null, 2) + '\n', { mode: 0o644 });
}

export function buildMaterials(source: string, output: string, revision: string): void {
  const outputRelative = relative(resolve(source), resolve(output));
  if (outputRelative === '' || (!outputRelative.startsWith('..') && !isAbsolute(outputRelative))) throw new Error('output cannot be inside deployment materials');
  const entries: MaterialEntry[] = [];
  collectMaterialEntries(source, '', entries);
  buildMaterialsFromEntries(entries, output, revision);
}

const PROJECTED_DEPLOYMENT_SCRIPTS = new Set([
  'scripts/agent_vm_state_migration.ts',
  'scripts/render_observability_dashboard.ts',
  'scripts/validate_observability_dashboard.ts',
]);

function projectObservabilityReference(source: string): Uint8Array {
  return Buffer.from(readFileSync(source, 'utf8').replaceAll('../assets/observability/', './'), 'utf8');
}

function deploymentMaterialEntries(repository: string): MaterialEntry[] {
  const deployRoot = join(repository, 'deploy');
  const opsRoot = join(repository, 'skills', 'agent-vm-ops');
  const entries: MaterialEntry[] = [];
  const deployEntries: MaterialEntry[] = [];
  collectMaterialEntries(deployRoot, '', deployEntries);
  for (const entry of deployEntries) {
    if (PROJECTED_DEPLOYMENT_SCRIPTS.has(entry.path) || entry.path.startsWith('observability/')) continue;
    entries.push(entry);
  }

  const projected: Array<[string, string, Uint8Array?]> = [
    ['scripts/agent_vm_state_migration.ts', join(opsRoot, 'scripts', 'agent_vm_state_migration.ts')],
    ['scripts/render_observability_dashboard.ts', join(opsRoot, 'scripts', 'render_observability_dashboard.ts')],
    ['scripts/validate_observability_dashboard.ts', join(opsRoot, 'scripts', 'validate_observability_dashboard.ts')],
    ['observability/README.md', join(opsRoot, 'references', 'observability.md'), projectObservabilityReference(join(opsRoot, 'references', 'observability.md'))],
  ];
  entries.push(...projected.map(([path, source, content]) => ({ path, source, content })));

  const assetsRoot = join(opsRoot, 'assets', 'observability');
  const assetEntries: MaterialEntry[] = [];
  collectMaterialEntries(assetsRoot, '', assetEntries);
  entries.push(...assetEntries.map((entry) => ({ path: join('observability', entry.path), source: entry.source })));
  return entries;
}

export function buildMaterialsArchive(repository: string, output: string, revision: string): void {
  if (!isAbsolute(output) || existsSync(output)) throw new Error('archive output must be an absent absolute file');
  const git = (args: string[]) => {
    const result = Bun.spawnSync(['git', '-C', repository, ...args], { stdout: 'pipe', stderr: 'pipe' });
    if (result.exitCode !== 0) throw new Error('cannot verify deployment source identity');
    return new TextDecoder().decode(result.stdout).trim();
  };
  if (git(['rev-parse', 'HEAD']) !== revision || git(['status', '--porcelain', '--untracked-files=normal'])) throw new Error('materials require a clean matching source revision');
  const temporary = mkdtempSync(join(dirname(output), '.agent-vm-materials-'));
  try {
    const directory = join(temporary, 'materials');
    const hasSkillSources = existsSync(join(repository, 'skills', 'agent-vm-ops', 'assets', 'observability'));
    if (hasSkillSources) buildMaterialsFromEntries(deploymentMaterialEntries(repository), directory, revision);
    else buildMaterials(join(repository, 'deploy'), directory, revision);
    const manifest = JSON.parse(readFileSync(join(directory, 'materials-manifest.json'), 'utf8')) as { files: { path: string }[] };
    const archive = join(temporary, 'materials.tar');
    const ownership = process.platform === 'darwin' ? ['--uid', '0', '--gid', '0', '--uname', 'root', '--gname', 'root'] : ['--owner=0', '--group=0', '--numeric-owner'];
    const result = Bun.spawnSync(['tar', '--format=ustar', '--no-xattrs', ...ownership, '-cf', archive, '-C', directory, '--no-recursion', 'materials-manifest.json', ...manifest.files.map(file => file.path)], { stdout: 'pipe', stderr: 'pipe', env: { ...process.env, COPYFILE_DISABLE: '1' } });
    if (result.exitCode !== 0) throw new Error('deployment materials archive failed');
    writeFileSync(output, readFileSync(archive), { flag: 'wx', mode: 0o644 });
  } finally {
    rmSync(temporary, { recursive: true, force: true });
  }
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const value = (key: string) => { const index = args.indexOf(key); if (index < 0 || !args[index + 1]) throw new Error(`missing ${key}`); return args[index + 1]; };
  if (args.includes('--archive')) buildMaterialsArchive(resolve(import.meta.dir, '../..'), value('--output'), value('--source-revision'));
  else buildMaterials(resolve(import.meta.dir, '..'), value('--output'), value('--source-revision'));
}
