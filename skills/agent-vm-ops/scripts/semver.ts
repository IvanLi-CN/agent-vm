/** Small SemVer 2.0.0 implementation used by the manual release boundary. */

export interface SemVer {
  major: number;
  minor: number;
  patch: number;
  prerelease: string[];
  build: string[];
}

const SEMVER_RE = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/;

function numeric(value: string, label: string): number {
  if (value.length > 1 && value.startsWith("0")) throw new Error(`${label} contains a leading zero`);
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) throw new Error(`${label} is outside the supported integer range`);
  return parsed;
}

function identifiers(value: string | undefined): string[] {
  return value === undefined ? [] : value.split(".");
}

export function parseSemVer(value: string): SemVer {
  const match = SEMVER_RE.exec(value);
  if (!match) throw new Error(`invalid SemVer: ${value}`);
  const prerelease = identifiers(match[4]);
  if (prerelease.some((item) => item === "" || /^0[0-9]+$/.test(item))) throw new Error(`invalid prerelease identifier: ${value}`);
  return {
    major: numeric(match[1], "major"),
    minor: numeric(match[2], "minor"),
    patch: numeric(match[3], "patch"),
    prerelease,
    build: identifiers(match[5]),
  };
}

export function formatSemVer(value: SemVer, includeBuild = true): string {
  const core = `${value.major}.${value.minor}.${value.patch}`;
  const pre = value.prerelease.length ? `-${value.prerelease.join(".")}` : "";
  const build = includeBuild && value.build.length ? `+${value.build.join(".")}` : "";
  return `${core}${pre}${build}`;
}

function compareIdentifiers(left: string, right: string): number {
  const leftNumeric = /^0$|^[1-9][0-9]*$/.test(left);
  const rightNumeric = /^0$|^[1-9][0-9]*$/.test(right);
  if (leftNumeric && rightNumeric) return Number(left) - Number(right);
  if (leftNumeric) return -1;
  if (rightNumeric) return 1;
  return left < right ? -1 : left > right ? 1 : 0;
}

/** Compare SemVer precedence; build metadata is intentionally ignored. */
export function compareSemVer(left: SemVer | string, right: SemVer | string): number {
  const a = typeof left === "string" ? parseSemVer(left) : left;
  const b = typeof right === "string" ? parseSemVer(right) : right;
  for (const key of ["major", "minor", "patch"] as const) {
    if (a[key] !== b[key]) return a[key] < b[key] ? -1 : 1;
  }
  if (!a.prerelease.length && !b.prerelease.length) return 0;
  if (!a.prerelease.length) return 1;
  if (!b.prerelease.length) return -1;
  for (let index = 0; index < Math.max(a.prerelease.length, b.prerelease.length); index += 1) {
    if (index >= a.prerelease.length) return -1;
    if (index >= b.prerelease.length) return 1;
    const result = compareIdentifiers(a.prerelease[index], b.prerelease[index]);
    if (result !== 0) return result;
  }
  return 0;
}

export function isFinal(value: SemVer | string): boolean {
  return (typeof value === "string" ? parseSemVer(value) : value).prerelease.length === 0;
}

export type ReleaseTarget = "patch" | "minor" | "major" | string;

export function resolveReleaseTarget(
  target: ReleaseTarget,
  completedFinalVersions: readonly string[],
  occupiedVersions: readonly string[] = completedFinalVersions,
): string {
  const parsedFinal = completedFinalVersions.filter(isFinal).map(parseSemVer);
  const baseline = parsedFinal.sort((a, b) => compareSemVer(a, b)).at(-1);
  if (target === "patch" || target === "minor" || target === "major") {
    if (!baseline) throw new Error("symbolic target requires a trusted completed final baseline");
    const candidate = target === "patch"
      ? `${baseline.major}.${baseline.minor}.${baseline.patch + 1}`
      : target === "minor"
        ? `${baseline.major}.${baseline.minor + 1}.0`
        : `${baseline.major + 1}.0.0`;
    if (occupiedVersions.some((item) => compareSemVer(item, candidate) === 0)) {
      throw new Error(`symbolic target resolves to an occupied SemVer: ${candidate}`);
    }
    return candidate;
  }
  const explicit = parseSemVer(target);
  if (!baseline) {
    if (formatSemVer(explicit) !== "1.0.0") throw new Error("the first public release must be exactly 1.0.0");
    if (occupiedVersions.some((item) => compareSemVer(item, explicit) === 0)) throw new Error("explicit target is already occupied");
    return formatSemVer(explicit);
  }
  if (compareSemVer(explicit, baseline) <= 0) throw new Error("explicit target must be greater than the trusted final baseline");
  if (occupiedVersions.some((item) => compareSemVer(item, explicit) >= 0)) {
    throw new Error("explicit target is not greater than every occupied SemVer");
  }
  return formatSemVer(explicit);
}

export function policyRevision(text: string): string {
  const hash = new Bun.CryptoHasher("sha256");
  hash.update(text);
  return hash.digest("hex");
}
