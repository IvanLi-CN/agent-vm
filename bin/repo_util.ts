import { existsSync, lstatSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");

export type GitResult = {
  exitCode: number;
  stdout: string;
  stderr: string;
};

function decode(output: Uint8Array): string {
  return new TextDecoder().decode(output);
}

export function runGit(args: string[], cwd: string = REPO_ROOT): GitResult {
  const result = Bun.spawnSync(["git", ...args], {
    cwd,
    stdout: "pipe",
    stderr: "pipe",
  });
  return {
    exitCode: result.exitCode,
    stdout: decode(result.stdout),
    stderr: decode(result.stderr),
  };
}

export function trackedFiles(repoRoot: string = REPO_ROOT): string[] {
  const result = runGit(["ls-files", "-co", "--exclude-standard", "-z"], repoRoot);
  if (result.exitCode !== 0) {
    throw new Error(`git ls-files failed: ${result.stderr.trim()}`);
  }
  return result.stdout
    .split("\0")
    .filter(Boolean)
    .map((path) => join(repoRoot, path));
}

export type WalkOptions = {
  ignoredDirectories?: Set<string>;
};

export function walkFiles(root: string, options: WalkOptions = {}): string[] {
  if (!existsSync(root) || !lstatSync(root).isDirectory()) {
    return [];
  }

  const ignored = options.ignoredDirectories ?? new Set<string>();
  const files: string[] = [];
  const visit = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = join(directory, entry.name);
      if (entry.isSymbolicLink()) {
        if (entry.isFile()) {
          files.push(path);
        }
        continue;
      }
      if (entry.isDirectory()) {
        if (!ignored.has(entry.name)) {
          visit(path);
        }
        continue;
      }
      if (entry.isFile()) {
        files.push(path);
      }
    }
  };
  visit(root);
  return files.sort();
}

export function readText(path: string): string {
  return readFileSync(path, "utf8");
}

export function relativePath(path: string, repoRoot: string = REPO_ROOT): string {
  return relative(repoRoot, path).split("\\").join("/");
}

export function resolveExistingFile(candidates: string[]): string | undefined {
  return candidates.find((candidate) => {
    try {
      return lstatSync(candidate).isFile();
    } catch {
      return false;
    }
  });
}
