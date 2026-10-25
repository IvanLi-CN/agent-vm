/** POSIX byte paths bypass runtime UTF-8 conversion at the syscall boundary. */
import { dlopen, ptr, toArrayBuffer } from "bun:ffi";
import { constants as C, closeSync, fstatSync, type Stats } from "node:fs";

function load() {
  const linux = process.platform === "linux";
  if (!linux && process.platform !== "darwin") throw new Error("POSIX byte paths are unavailable");
  const common = {
    open: { args: ["ptr", "i32"], returns: "i32" },
    readlink: { args: ["ptr", "ptr", "u64"], returns: "i64" },
    fdopendir: { args: ["i32"], returns: "ptr" },
    readdir: { args: ["ptr"], returns: "ptr" },
    closedir: { args: ["ptr"], returns: "i32" },
    unlink: { args: ["ptr"], returns: "i32" },
    rmdir: { args: ["ptr"], returns: "i32" },
  } as const;
  const lib = linux ? dlopen("libc.so.6", { ...common, __errno_location: { args: [], returns: "ptr" } }) : dlopen("/usr/lib/libSystem.B.dylib", { ...common, __error: { args: [], returns: "ptr" } });
  const errno = () => new Int32Array(toArrayBuffer("__errno_location" in lib.symbols ? lib.symbols.__errno_location()! : lib.symbols.__error()!, 0, 4));
  const error = (op: string): never => { const n = errno()[0]; throw Object.assign(new Error(`${op} failed for a POSIX byte path (errno ${n})`), { code: n === 2 ? "ENOENT" : n === 20 ? "ENOTDIR" : "EIO", errno: n }); };
  const terminated = (path: Buffer): Buffer => { if (path.includes(0)) throw new Error("NUL in byte path"); return Buffer.concat([path, Buffer.from([0])]); };
  return {
    open(path: Buffer, flags: number): number {
      // Production byte-path opens only inspect existing files. In particular, do
      // not call variadic open(O_CREAT) with a fixed FFI signature on Darwin ARM64.
      if (flags & C.O_CREAT) throw new Error("byte-path creation is outside the read-only open boundary");
      const p = terminated(path), fd = lib.symbols.open(ptr(p), flags); if (fd < 0) return error("open"); return fd;
    },
    readlink(path: Buffer): Buffer {
      const p = terminated(path), buffer = Buffer.alloc(65536), n = Number(lib.symbols.readlink(ptr(p), ptr(buffer), buffer.length));
      if (n < 0) return error("readlink"); if (n === buffer.length) throw new Error("symlink target exceeds byte bound"); return buffer.subarray(0, n);
    },
    readdir(path: Buffer): Buffer[] {
      const fd = this.open(path, C.O_RDONLY | C.O_DIRECTORY | C.O_NOFOLLOW), directory = lib.symbols.fdopendir(fd);
      if (!directory) { closeSync(fd); return error("fdopendir"); }
      try {
        const names: Buffer[] = [];
        for (;;) {
          errno()[0] = 0; const entry = lib.symbols.readdir(directory);
          if (!entry) { if (errno()[0]) return error("readdir"); break; }
          // Both supported 64-bit ABIs store d_reclen at 16. Darwin adds d_namlen.
          const header = new DataView(toArrayBuffer(entry, 0, 20)), length = header.getUint16(16, true), offset = linux ? 19 : 21;
          if (length <= offset || length > 65536) throw new Error("invalid native directory entry");
          const bytes = Buffer.from(toArrayBuffer(entry, 0, length)), end = bytes.indexOf(0, offset);
          if (end < offset) throw new Error("unterminated native directory entry");
          const name = Buffer.from(bytes.subarray(offset, end));
          if (!name.equals(Buffer.from(".")) && !name.equals(Buffer.from(".."))) names.push(name);
        }
        return names;
      } finally { lib.symbols.closedir(directory); }
    },
    remove(path: Buffer, directory: boolean): void { const p = terminated(path); if ((directory ? lib.symbols.rmdir(ptr(p)) : lib.symbols.unlink(ptr(p))) !== 0) error(directory ? "rmdir" : "unlink"); },
  };
}
let native: ReturnType<typeof load> | undefined;
const api = () => native ??= load();
export const openBytes = (path: Buffer, flags: number): number => api().open(path, flags);
export function lstatBytes(path: Buffer): Stats {
  // O_PATH returns metadata for the symlink itself on Linux; O_SYMLINK does so on Darwin.
  const fd = openBytes(path, process.platform === "linux" ? 0x200000 | C.O_NOFOLLOW : C.O_RDONLY | 0x200000);
  try { return fstatSync(fd); } finally { closeSync(fd); }
}
export const readlinkBytes = (path: Buffer): Buffer => api().readlink(path);
export const readdirBytes = (path: Buffer): Buffer[] => api().readdir(path);
export function removeTreeBytes(path: Buffer): void {
  const stat = lstatBytes(path);
  if (stat.isDirectory() && !stat.isSymbolicLink()) {
    for (const name of readdirBytes(path)) removeTreeBytes(Buffer.concat([path, Buffer.from("/"), name]));
    api().remove(path, true);
  } else api().remove(path, false);
}
