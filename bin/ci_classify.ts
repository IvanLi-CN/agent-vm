#!/usr/bin/env bun
import { appendFileSync } from "node:fs";

const output = process.env.GITHUB_OUTPUT;
const base = process.env.GITHUB_BASE_SHA || "";
const head = process.env.GITHUB_SHA || "HEAD";
const diff = Bun.spawnSync(["git", "diff", "--name-only", `${base}...${head}`], { stdout: "pipe", stderr: "pipe" });
if (diff.exitCode !== 0 || !base) {
  if (output) appendFileSync(output, "full=true\nmacos=true\nreason=classification-failed\n");
  process.exit(0);
}
const files = new TextDecoder().decode(diff.stdout).split(/\r?\n/).filter(Boolean);
const docsOnly = files.length > 0 && files.every((file) => /^(docs\/|README|GLOSSARY)/.test(file));
const webOnly = files.length > 0 && files.every((file) => /^(web\/|package\.json$|bun\.lock$|tsconfig\.json$)/.test(file));
const macos = files.some((file) => /(^|\/)(macos|darwin|native|desktop|\.mm$|\.m$)/i.test(file)) || files.some((file) => /^\.github\/workflows\//.test(file));
const full = !(docsOnly || webOnly) || macos;
if (output) appendFileSync(output, `full=${full}\nmacos=${macos || full}\nreason=${full ? "full-or-platform-change" : "known-non-platform-change"}\n`);
console.log(JSON.stringify({ files, full, macos: macos || full }));
