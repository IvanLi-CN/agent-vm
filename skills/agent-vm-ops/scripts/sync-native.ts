import { dlopen, ptr } from "bun:ffi";

// Open native capabilities before creating any transaction paths. No copy/delete fallback.
export function nativeCapabilities(): { lock(fd: number): boolean; unlock(fd: number): void; rename(old: Buffer, next: Buffer, exchange: boolean): void; close(): void } {
  const linux = process.platform === "linux";
  if (!linux && process.platform !== "darwin") throw new Error("native atomic directory transaction is unavailable");
  const lib = linux ? dlopen("libc.so.6", {
    flock: { args: ["i32", "i32"], returns: "i32" },
    renameat2: { args: ["i32", "ptr", "i32", "ptr", "u32"], returns: "i32" },
  }) : dlopen("/usr/lib/libSystem.B.dylib", {
    flock: { args: ["i32", "i32"], returns: "i32" },
    renamex_np: { args: ["ptr", "ptr", "u32"], returns: "i32" },
  });
  return {
    lock: fd => lib.symbols.flock(fd, 2 | 4) === 0,
    unlock: fd => { lib.symbols.flock(fd, 8); },
    rename: (old, next, exchange) => {
      const a = Buffer.concat([old, Buffer.from([0])]), b = Buffer.concat([next, Buffer.from([0])]);
      const result = "renameat2" in lib.symbols ? lib.symbols.renameat2(-100, ptr(a), -100, ptr(b), exchange ? 2 : 1) : lib.symbols.renamex_np(ptr(a), ptr(b), exchange ? 2 : 4);
      if (result !== 0) throw new Error("native atomic directory rename failed; source and recovery material retained");
    },
    close: () => lib.close(),
  };
}
