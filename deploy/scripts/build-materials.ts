#!/usr/bin/env bun
/** Produce revision-bound deployment materials without changing product bundles. */
import { createHash } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync, lstatSync } from 'node:fs';
import { join, resolve, relative, isAbsolute } from 'node:path';

export function buildMaterials(source: string, output: string, revision: string) {
  if (!/^[a-f0-9]{40}$/.test(revision)) throw new Error('source revision must be an exact Git SHA');
  if (!isAbsolute(output) || existsSync(output)) throw new Error('output must be an absent absolute directory');
  const outputRelative = relative(source, output);
  if (!outputRelative.startsWith('..') && !isAbsolute(outputRelative)) throw new Error('output cannot be inside deployment materials');
  const files: {path: string; sha256: string; mode: number}[] = [];
  function visit(directory: string) {
    for (const name of readdirSync(directory).sort()) {
      const path = join(directory, name);
      const info = lstatSync(path);
      if (info.isSymbolicLink()) throw new Error('deployment materials cannot contain symlinks');
      if (info.isDirectory()) visit(path);
      else if (info.isFile()) files.push({path: relative(source, path), sha256: createHash('sha256').update(readFileSync(path)).digest('hex'), mode: info.mode & 0o777});
      else throw new Error('deployment materials contain a special file');
    }
  }
  visit(source);
  mkdirSync(output, {mode: 0o750});
  for (const entry of files) {
    const destination = join(output, entry.path);
    mkdirSync(resolve(destination, '..'), {recursive: true, mode: 0o750});
    writeFileSync(destination, readFileSync(join(source, entry.path)), {mode: entry.mode});
    chmodSync(destination, entry.mode);
  }
  writeFileSync(join(output, 'materials-manifest.json'), JSON.stringify({schema_version: 1, source_revision: revision, files}, null, 2) + '\n', {mode: 0o644});
}
if (import.meta.main) {
  const args = process.argv.slice(2);
  const value = (key: string) => {const i=args.indexOf(key); if(i<0 || !args[i+1]) throw new Error(`missing ${key}`); return args[i+1];};
  buildMaterials(resolve(import.meta.dir, '..'), value('--output'), value('--source-revision'));
}
