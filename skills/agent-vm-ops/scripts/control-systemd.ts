import {
  closeSync,
  constants,
  fsyncSync,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { randomUUID } from "node:crypto";
import { dirname, join } from "node:path";
import { CommandRunner } from "./control-native.ts";
import {
  ControlError,
  DEFAULT_TIMER_DIR,
  TASK_ID_RE,
  iso_time,
  parse_time,
  type Data,
  type Integer,
} from "./control-types.ts";

function error_code(error: unknown): string | undefined {
  if (!(error instanceof Error)) return undefined;
  return (error as NodeJS.ErrnoException).code;
}

function verification_error(error: unknown): boolean {
  return (
    error instanceof ControlError ||
    error instanceof TypeError ||
    error instanceof RangeError ||
    error_code(error) !== undefined
  );
}

function split_lines(text: string): string[] {
  if (text === "") return [];
  const lines = text.split(
    /\r\n|[\n\r\v\f\u001c-\u001e\u0085\u2028\u2029]/u,
  );
  if (lines.at(-1) === "") lines.pop();
  return lines;
}

function read_lines(path: string): string[] {
  const text = new TextDecoder("utf-8", { fatal: true }).decode(
    readFileSync(path),
  );
  return split_lines(text);
}

function properties(stdout: Buffer): Map<string, string> {
  const result = new Map<string, string>();
  for (const line of split_lines(stdout.toString("utf8"))) {
    const separator = line.indexOf("=");
    if (separator !== -1) {
      result.set(line.slice(0, separator), line.slice(separator + 1));
    }
  }
  return result;
}

function regular_file(path: string): boolean {
  try {
    const info = lstatSync(path);
    return !info.isSymbolicLink() && info.isFile();
  } catch (error) {
    if (["ENOENT", "ENOTDIR"].includes(error_code(error) ?? "")) {
      return false;
    }
    throw error;
  }
}

function path_present(path: string): boolean {
  try {
    // lstat also detects dangling symlinks.
    lstatSync(path);
    return true;
  } catch (error) {
    if (["ENOENT", "ENOTDIR"].includes(error_code(error) ?? "")) {
      return false;
    }
    throw error;
  }
}

function unlink_if_present(path: string): boolean {
  try {
    unlinkSync(path);
    return true;
  } catch (error) {
    if (error_code(error) === "ENOENT") return false;
    throw error;
  }
}

function fsync_directory(path: string): void {
  const fd = openSync(path, constants.O_RDONLY | constants.O_DIRECTORY);
  try {
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
}

function atomic_write(path: string, content: string): void {
  const directory = dirname(path);
  const temporary = join(
    directory,
    `.${path.slice(path.lastIndexOf("/") + 1)}.${randomUUID()}`,
  );
  let fd: number | undefined;
  let created = false;
  let renamed = false;

  try {
    // Match tempfile.mkstemp: exclusive creation with private permissions.
    fd = openSync(
      temporary,
      constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL,
      0o600,
    );
    created = true;
    writeFileSync(fd, content, "utf8");
    fsyncSync(fd);

    const written_fd = fd;
    fd = undefined;
    closeSync(written_fd);

    // The temporary file shares the destination filesystem.
    renameSync(temporary, path);
    renamed = true;
    fsync_directory(directory);
  } finally {
    try {
      if (fd !== undefined) closeSync(fd);
    } finally {
      if (created && !renamed) unlink_if_present(temporary);
    }
  }
}

function calendar_time(expires_at: string): string {
  const parsed = parse_time(expires_at);
  if (parsed === null) {
    throw new ControlError("invalid_time", "expiry timestamp is required");
  }
  return iso_time(parsed).replace("T", " ").replace(/\+00:00$/, " UTC");
}

export class SystemdManager {
  runner: CommandRunner;
  config: Data;
  enabled: boolean;
  timer_dir: string;
  static_unit_dir: string;

  constructor(
    runner: CommandRunner,
    config: Data,
    { enabled = true }: { enabled?: boolean } = {},
  ) {
    this.runner = runner;
    this.config = config;
    this.enabled = enabled;
    this.timer_dir = String(config.timer_dir);
    this.static_unit_dir = DEFAULT_TIMER_DIR;
  }

  static timer_unit(task_id: string, generation: Integer): string {
    const valid_task =
      typeof task_id === "string" &&
      TASK_ID_RE.exec(task_id)?.[0] === task_id;
    const valid_generation =
      typeof generation === "bigint"
        ? generation >= 1n
        : typeof generation === "number" &&
          Number.isSafeInteger(generation) &&
          generation >= 1;

    if (!valid_task || !valid_generation) {
      throw new ControlError("invalid_timer", "invalid task timer identity");
    }
    return `agent-vm-native-expire-${task_id}-g${generation}.timer`;
  }

  static instance_name(task_id: string, generation: Integer): string {
    return `${task_id}-g${generation}`;
  }

  // Python static methods are also callable through an instance.
  timer_unit(task_id: string, generation: Integer): string {
    return SystemdManager.timer_unit(task_id, generation);
  }

  instance_name(task_id: string, generation: Integer): string {
    return SystemdManager.instance_name(task_id, generation);
  }

  async _systemctl(...args: string[]): Promise<void> {
    if (this.enabled) {
      await this.runner.run(["systemctl", ...args], { timeout: 30 });
    }
  }

  async schedule_expiry(
    task_id: string,
    generation: Integer,
    expires_at: string,
  ): Promise<string> {
    const unit = this.timer_unit(task_id, generation);
    const timer_path = join(this.timer_dir, unit);
    const content =
      "[Unit]\n" +
      "Description=Expire native agent VM lease\n" +
      "After=agent-vm-native-reconcile.service\n\n" +
      "[Timer]\n" +
      `OnCalendar=${calendar_time(expires_at)}\n` +
      "Persistent=yes\n" +
      "AccuracySec=1s\n" +
      "RandomizedDelaySec=0\n" +
      `Unit=agent-vm-native-expire@${this.instance_name(task_id, generation)}.service\n\n` +
      "[Install]\n" +
      "WantedBy=timers.target\n";

    if (this.enabled) {
      mkdirSync(this.timer_dir, { recursive: true });
      atomic_write(timer_path, content);
      await this._systemctl("daemon-reload");
      await this._systemctl("enable", "--now", unit);
      await this._systemctl("restart", unit);
    }
    return unit;
  }

  async expiry_active(
    task_id: string,
    generation: Integer,
    expires_at: string,
  ): Promise<boolean> {
    const unit = this.timer_unit(task_id, generation);
    const service_unit =
      `agent-vm-native-expire@${this.instance_name(task_id, generation)}.service`;
    if (!this.enabled) return true;

    const path = join(this.timer_dir, unit);
    try {
      if (!regular_file(path)) return false;
      const expected_calendar = `OnCalendar=${calendar_time(expires_at)}`;
      const lines = read_lines(path);
      const result = await this.runner.run(
        [
          "systemctl", "show", unit,
          "-p", "ActiveState",
          "-p", "FragmentPath",
          "-p", "UnitFileState",
        ],
        { timeout: 10, check: false },
      );
      const values = properties(result.stdout);
      const count = (line: string): number =>
        lines.filter((item) => item === line).length;

      return (
        result.returncode === 0 &&
        values.get("ActiveState") === "active" &&
        values.get("FragmentPath") === path &&
        values.get("UnitFileState") === "enabled" &&
        count(`Unit=${service_unit}`) === 1 &&
        count(expected_calendar) === 1 &&
        count("Persistent=yes") === 1
      );
    } catch (error) {
      if (verification_error(error)) return false;
      throw error;
    }
  }

  async service_asset_active(
    unit: string,
    required_lines: readonly string[],
  ): Promise<boolean> {
    if (!this.enabled) return true;

    const path = join(this.static_unit_dir, unit);
    try {
      if (!regular_file(path)) return false;
      const lines = read_lines(path);

      if (unit.endsWith("@.service")) {
        const result = await this.runner.run(
          ["systemctl", "cat", unit],
          { timeout: 10, check: false },
        );
        return (
          result.returncode === 0 &&
          result.stdout.toString("utf8").includes(`# ${path}\n`) &&
          required_lines.every((line) => lines.includes(line))
        );
      }

      const result = await this.runner.run(
        [
          "systemctl", "show", unit,
          "-p", "LoadState",
          "-p", "FragmentPath",
        ],
        { timeout: 10, check: false },
      );
      const values = properties(result.stdout);
      return (
        result.returncode === 0 &&
        values.get("LoadState") === "loaded" &&
        values.get("FragmentPath") === path &&
        required_lines.every((line) => lines.includes(line))
      );
    } catch (error) {
      if (verification_error(error)) return false;
      throw error;
    }
  }

  async enabled_unit_active(unit: string): Promise<boolean> {
    if (!this.enabled) return true;

    try {
      const result = await this.runner.run(
        [
          "systemctl", "show", unit,
          "-p", "ActiveState",
          "-p", "UnitFileState",
        ],
        { timeout: 10, check: false },
      );
      const values = properties(result.stdout);
      return (
        result.returncode === 0 &&
        values.get("ActiveState") === "active" &&
        values.get("UnitFileState") === "enabled"
      );
    } catch (error) {
      if (verification_error(error)) return false;
      throw error;
    }
  }

  async enabled_service_ready(unit: string): Promise<boolean> {
    // A completed oneshot service need not remain active.
    if (!this.enabled) return true;

    try {
      const result = await this.runner.run(
        [
          "systemctl", "show", unit,
          "-p", "LoadState",
          "-p", "UnitFileState",
        ],
        { timeout: 10, check: false },
      );
      const values = properties(result.stdout);
      return (
        result.returncode === 0 &&
        values.get("LoadState") === "loaded" &&
        values.get("UnitFileState") === "enabled"
      );
    } catch (error) {
      if (verification_error(error)) return false;
      throw error;
    }
  }

  async remove_expiry(
    task_id: string,
    generation: Integer,
  ): Promise<string> {
    const unit = this.timer_unit(task_id, generation);
    const path = join(this.timer_dir, unit);

    try {
      await this._systemctl("disable", "--now", unit);
    } catch (error) {
      if (
        !(error instanceof ControlError) ||
        error.code !== "command_failed" ||
        !this.enabled
      ) {
        throw error;
      }

      // A crash may precede timer creation after durable reservation.
      let probe: Awaited<ReturnType<CommandRunner["run"]>>;
      try {
        probe = await this.runner.run(
          [
            "systemctl", "show", unit,
            "-p", "LoadState",
            "-p", "ActiveState",
            "-p", "FragmentPath",
            "-p", "UnitFileState",
          ],
          { timeout: 10, check: false },
        );
      } catch (probe_error) {
        if (probe_error instanceof ControlError) throw error;
        throw probe_error;
      }

      const values = properties(probe.stdout);
      if (
        ![0, 1].includes(probe.returncode) ||
        values.size !== 4 ||
        values.get("LoadState") !== "not-found" ||
        values.get("ActiveState") !== "inactive" ||
        values.get("FragmentPath") !== "" ||
        values.get("UnitFileState") !== "" ||
        path_present(path)
      ) {
        throw error;
      }
    }

    if (this.enabled) {
      if (unlink_if_present(path)) {
        fsync_directory(this.timer_dir);
      }
      await this._systemctl("daemon-reload");
    }
    return unit;
  }

  async enable_periodic_units(): Promise<void> {
    await this._systemctl("daemon-reload");
    await this._systemctl(
      "enable", "--now", "agent-vm-native-age-gc.timer",
    );
    await this._systemctl(
      "enable", "agent-vm-native-reconcile.service",
    );
  }

  async retry_cleanup(
    task_id: string,
    generation: Integer,
  ): Promise<void> {
    await this._systemctl(
      "start",
      "--no-block",
      `agent-vm-native-expire@${this.instance_name(task_id, generation)}.service`,
    );
  }
}
