"""Contract tests for release identity, integrity, and compatibility windows."""
from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location(
    "agent_vm_release_manifest", ROOT / "skills/agent-vm-ops/scripts/release_manifest.py"
)
assert SPEC and SPEC.loader
MANIFEST = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MANIFEST)


class ManifestContractTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        artifacts = []
        for name, path, role, mode in sorted(MANIFEST.FIXED_ARTIFACTS):
            content = f"{role}:{path}\n".encode()
            target = self.root / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(content)
            target.chmod(mode)
            artifacts.append({
                "name": name,
                "path": path,
                "role": role,
                "sha256": hashlib.sha256(content).hexdigest(),
                "mode": mode,
            })
        self.manifest = MANIFEST.build_manifest("a" * 40, artifacts)
        (self.root / MANIFEST.MANIFEST_NAME).write_text(json.dumps(self.manifest), encoding="utf-8")

    def test_valid_manifest_and_bundle_are_accepted(self):
        checked = MANIFEST.load_manifest(self.root / MANIFEST.MANIFEST_NAME)
        self.assertEqual(checked["format"], 1)
        self.assertEqual(checked["release_id"], "a" * 40)
        self.assertEqual(checked["protocol"], 4)
        self.assertEqual(set(checked["actions"]), set(MANIFEST.ACTION_NAMES))
        self.assertEqual(MANIFEST.verify_bundle(self.root)["source_revision"], "a" * 40)

    def test_fixed_artifact_set_contains_the_complete_bun_controller_and_client(self):
        support_names = {
            "release_manifest.py",
            "control-types.ts",
            "control-store.ts",
            "control-native.ts",
            "control-core.ts",
            "control-observation.ts",
            "control-resources.ts",
            "control-systemd.ts",
            "control-lifecycle.ts",
            "release-manifest.ts",
            "guest-root-growth.ts",
            *MANIFEST.CLIENT_DEPENDENCY_MODES,
        }
        expected = {
            ("agent_vm.ts", "agent_vm.ts", "client", 0o755),
            ("agent_vm_control.ts", "agent_vm_control.ts", "control", 0o755),
            *{(name, name, "support", MANIFEST.CLIENT_DEPENDENCY_MODES.get(name, 0o644)) for name in support_names},
            *{
                ("unit:" + name, "systemd/" + name, "unit", 0o644)
                for name in (
                    "agent-vm-native-age-gc.service",
                    "agent-vm-native-age-gc.timer",
                    "agent-vm-native-expire@.service",
                    "agent-vm-native-reconcile.service",
                )
            },
        }
        self.assertEqual(MANIFEST.FIXED_ARTIFACTS, expected)
        for name in support_names | {"agent_vm_control.ts", "agent_vm.ts"}:
            with self.subTest(missing=name):
                incomplete = copy.deepcopy(self.manifest)
                incomplete["artifacts"] = [
                    item for item in incomplete["artifacts"] if item["path"] != name
                ]
                with self.assertRaises(MANIFEST.ManifestError) as failure:
                    MANIFEST.validate_manifest(incomplete)
                self.assertEqual(failure.exception.code, "manifest_artifact_set_invalid")

    @unittest.skipUnless(shutil.which("bun"), "Bun executable is not installed")
    def test_python_controller_and_client_verify_the_same_complete_package(self):
        sidecar = {
            "format": 1, "release_id": self.manifest["release_id"],
            "source_revision": self.manifest["source_revision"], "protocol": 4,
            "client_artifact": next(item for item in self.manifest["artifacts"] if item["role"] == "client"),
            "client_dependencies": [item for item in self.manifest["artifacts"] if item["path"] in MANIFEST.CLIENT_DEPENDENCY_MODES],
            "client_view": self.manifest["client_view"],
        }
        path = self.root / MANIFEST.CLIENT_SIDECAR_NAME
        script = """
import * as M from './skills/agent-vm-ops/scripts/release-manifest.ts';
import {loadRelease} from './skills/agent-vm/scripts/client-release.ts';
const {root, manifest, fixed} = await Bun.stdin.json();
if (JSON.stringify([...M.FIXED_ARTIFACTS].sort()) !== JSON.stringify(fixed.sort())) throw Error('artifact policies differ');
const checks = [() => M.verify_client_sidecar(root, manifest), () => loadRelease(root + '/client-release.json')];
console.log(JSON.stringify(checks.map(check => {try {check(); return true;} catch {return false;}})));
"""
        def check(value, accepted):
            path.write_text(json.dumps(value), encoding="utf-8")
            if accepted:
                MANIFEST.verify_client_sidecar(self.root, self.manifest)
            else:
                with self.assertRaises(MANIFEST.ManifestError):
                    MANIFEST.verify_client_sidecar(self.root, self.manifest)
            process = subprocess.run(
                [shutil.which("bun"), "--no-macros", "--eval", script],
                input=json.dumps({"root": str(self.root), "manifest": self.manifest,
                                  "fixed": list(MANIFEST.FIXED_ARTIFACTS)}).encode(),
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30, cwd=ROOT,
            )
            self.assertEqual(process.returncode, 0, process.stderr.decode())
            self.assertEqual(json.loads(process.stdout), [accepted, accepted])

        check(sidecar, True)
        for mutate in (
            lambda v: v.pop("client_dependencies"),
            lambda v: v["client_dependencies"].pop(),
            lambda v: v["client_dependencies"].append(v["client_dependencies"][0]),
            lambda v: v["client_dependencies"][0].update(path="../escape.ts"),
            lambda v: v["client_dependencies"][0].update(mode=0o777),
            lambda v: v["client_dependencies"][0].update(mode=float(v["client_dependencies"][0]["mode"])),
            lambda v: v["client_dependencies"][0].update(sha256="0" * 64),
            lambda v: v.update(format=True),
            lambda v: v["client_view"]["actions"]["status"]["request"].update(schema=True),
            lambda v: v["client_artifact"].update(name="agent_vm.py", path="agent_vm.py"),
        ):
            value = copy.deepcopy(sidecar)
            mutate(value)
            check(value, False)
        # Every imported dependency is verified against actual bytes, fixed mode
        # and file type by all three consumers, not merely compared to metadata.
        for name, mode in MANIFEST.CLIENT_PACKAGE_MODES.items():
            with self.subTest(artifact=name):
                artifact = self.root / name
                content = artifact.read_bytes()
                artifact.write_bytes(content + b"tampered")
                check(sidecar, False)
                artifact.write_bytes(content)
                artifact.chmod(0o600)
                check(sidecar, False)
                artifact.chmod(mode)
                artifact.unlink()
                artifact.symlink_to(path)
                check(sidecar, False)
                artifact.unlink()
                check(sidecar, False)
                artifact.write_bytes(content)
                artifact.chmod(mode)
        check(sidecar, True)


    def test_systemd_assets_match_frozen_bun_policy_and_typescript_constants(self):
        expected_hashes = {
            "agent-vm-native-age-gc.service": "6a6387459c3f4b5f888fc7d4fe6d49cf571ff4eabd3a96edde72a05d37588460",
            "agent-vm-native-age-gc.timer": "4eb1c7b012fc3251ef4427c56b8c1b0236deff28762cc4293278f56d02c4cb4b",
            "agent-vm-native-expire@.service": "e0cfb3c7eaf4aaadcfe5e296a0698f0cb235c4bbecd22275ad5c6826d7cbd3e2",
            "agent-vm-native-reconcile.service": "76ac0a8e730ffdfa4c0fe0b0c0cd6ad75dd11e627b747e843ded9fae5fd8ddc8",
        }
        self.assertEqual(MANIFEST.UNIT_CONTENT_SHA256, expected_hashes)
        typescript = (
            ROOT / "skills/agent-vm-ops/scripts/release-manifest.ts"
        ).read_text(encoding="utf-8")
        for name, expected in (
            ("UNIT_CONTENT_SHA256", expected_hashes),
            ("UNIT_EXEC_START", MANIFEST.UNIT_EXEC_START),
        ):
            block = typescript.split(f"export const {name} = Object.freeze({{", 1)[1].split("});", 1)[0]
            self.assertEqual(dict(re.findall(r'"([^"]+)":\s*"([^"]+)"', block)), expected)
        for name, digest in expected_hashes.items():
            with self.subTest(unit=name):
                content = (
                    ROOT / "skills/agent-vm-ops/assets/systemd" / name
                ).read_bytes()
                self.assertEqual(hashlib.sha256(content).hexdigest(), digest)
                if name.endswith(".service"):
                    starts = [
                        line for line in content.decode().splitlines()
                        if line.startswith("ExecStart=")
                    ]
                    self.assertEqual(starts, [MANIFEST.UNIT_EXEC_START[name]])
                    self.assertTrue(starts[0].startswith(
                        "ExecStart=/usr/local/bin/bun /opt/agent-vm-native/agent_vm_control.ts ",
                    ))

    def test_typescript_syntax_uses_fixed_transpiler_with_source_only_on_stdin(self):
        source = (
            "import './missing.ts';\n"
            "export const value: number = 1;\n"
            "throw new Error('must not execute');\n"
        )
        candidate = self.root / "candidate.ts"
        candidate.write_text(source, encoding="utf-8")
        manifest = {"artifacts": [
            {"role": "support", "path": "release_manifest.py"},
            {"role": "control", "path": candidate.name},
        ]}
        with mock.patch.dict(os.environ, {"BUN_OPTIONS": "--preload=/untrusted.ts"}):
            with mock.patch.object(
                MANIFEST.subprocess, "run",
                return_value=subprocess.CompletedProcess([], 0, b"", b""),
            ) as run:
                MANIFEST.verify_typescript_syntax(self.root, manifest)
        run.assert_called_once_with(
            [shutil.which("bun"), "--no-macros", "--eval", MANIFEST.TYPESCRIPT_SYNTAX_CHECK],
            input=source.encode(),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            check=False,
            timeout=30,
            cwd="/",
            env={"PATH": "/usr/local/bin:/usr/bin:/bin"},
        )
        self.assertEqual(
            MANIFEST.TYPESCRIPT_SYNTAX_CHECK,
            "const source = await Bun.stdin.text();\n"
            'new Bun.Transpiler({loader: "ts", target: "bun"}).transformSync(source);\n',
        )

    def test_typescript_syntax_checks_every_control_and_support_module(self):
        paths = ["agent_vm_control.ts", "agent_vm.ts", *MANIFEST.TYPESCRIPT_SUPPORT_ARTIFACT_NAMES]
        artifacts = []
        expected_sources = []
        for index, name in enumerate(paths):
            source = f"export const value: number = {index};\n".encode()
            (self.root / name).write_bytes(source)
            expected_sources.append(source)
            artifacts.append({
                "role": "control" if index == 0 else "support",
                "path": name,
            })
        with mock.patch.object(
            MANIFEST.subprocess, "run",
            return_value=subprocess.CompletedProcess([], 0, b"", b""),
        ) as run:
            MANIFEST.verify_typescript_syntax(self.root, {"artifacts": artifacts})
        self.assertEqual(
            [call.kwargs["input"] for call in run.call_args_list], expected_sources,
        )

    def test_typescript_syntax_failures_are_structured_and_fail_closed(self):
        candidate = self.root / "invalid.ts"
        candidate.write_text("export const broken: = ;\n", encoding="utf-8")
        manifest = {"artifacts": [{"role": "control", "path": candidate.name}]}
        cases = (
            (subprocess.CompletedProcess([], 1, b"", b"parse failed"), None, "typescript_syntax_invalid"),
            (None, FileNotFoundError(), "typescript_syntax_unverified"),
            (None, subprocess.TimeoutExpired("/usr/local/bin/bun", 30), "typescript_syntax_unverified"),
        )
        for result, error, code in cases:
            with self.subTest(code=code, error=type(error).__name__):
                with mock.patch.object(
                    MANIFEST.subprocess, "run", return_value=result, side_effect=error,
                ):
                    with self.assertRaises(MANIFEST.ManifestError) as failure:
                        MANIFEST.verify_typescript_syntax(self.root, manifest)
                self.assertEqual(failure.exception.code, code)
        candidate.write_bytes(b"\xff")
        with mock.patch.object(MANIFEST.subprocess, "run") as run:
            with self.assertRaises(MANIFEST.ManifestError) as failure:
                MANIFEST.verify_typescript_syntax(self.root, manifest)
        self.assertEqual(failure.exception.code, "typescript_syntax_invalid")
        run.assert_not_called()

    @unittest.skipUnless(shutil.which("bun"), "Bun executable is not installed")
    def test_real_transpiler_parses_without_executing_or_resolving_candidate_imports(self):
        candidate = self.root / "syntax-only.ts"
        manifest = {"artifacts": [{"role": "control", "path": candidate.name}]}
        candidate.write_text(
            "import './does-not-exist.ts';\n"
            "export const value: number = 1;\n"
            "throw new Error('candidate source must never execute');\n",
            encoding="utf-8",
        )
        MANIFEST.verify_typescript_syntax(self.root, manifest)
        candidate.write_text("export const broken: = ;\n", encoding="utf-8")
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.verify_typescript_syntax(self.root, manifest)
        self.assertEqual(failure.exception.code, "typescript_syntax_invalid")

    def test_paths_modes_checksums_and_self_reference_fail_closed(self):
        for mutation, code in (
            (lambda value: value["artifacts"][0].update(path="../escape"), "manifest_path_invalid"),
            (lambda value: value["artifacts"][0].update(mode=0o644), "manifest_artifact_set_invalid"),
            (lambda value: value["artifacts"][0].update(sha256="0" * 64), "artifact_checksum_mismatch"),
            (lambda value: value["artifacts"][0].update(path=MANIFEST.MANIFEST_NAME), "manifest_self_reference"),
        ):
            with self.subTest(code=code):
                value = copy.deepcopy(self.manifest)
                mutation(value)
                if code in {"manifest_path_invalid", "manifest_self_reference"}:
                    with self.assertRaises(MANIFEST.ManifestError) as failure:
                        MANIFEST.validate_manifest(value)
                else:
                    with self.assertRaises(MANIFEST.ManifestError) as failure:
                        MANIFEST.verify_bundle(self.root, value)
                self.assertEqual(failure.exception.code, code)

        outside = self.root.parent / (self.root.name + "-outside")
        outside.mkdir()
        self.addCleanup(lambda: outside.rmdir())
        external_client = outside / "agent_vm.ts"
        external_client.write_bytes(b"client\n")
        external_client.chmod(0o755)
        self.addCleanup(external_client.unlink)
        value = copy.deepcopy(self.manifest)
        value["artifacts"][0]["path"] = "nested/agent_vm.ts"
        (self.root / "nested").symlink_to(outside, target_is_directory=True)
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.verify_bundle(self.root, value)
        self.assertEqual(failure.exception.code, "manifest_artifact_set_invalid")

    def test_release_shape_requires_fixed_artifacts_and_client_view(self):
        incomplete = copy.deepcopy(self.manifest)
        incomplete["artifacts"] = incomplete["artifacts"][:-1]
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.validate_manifest(incomplete)
        self.assertEqual(failure.exception.code, "manifest_artifact_set_invalid")

        missing_view = copy.deepcopy(self.manifest)
        del missing_view["client_view"]
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.validate_manifest(missing_view)
        self.assertEqual(failure.exception.code, "manifest_invalid")

        missing_action = copy.deepcopy(self.manifest)
        del missing_action["actions"]["status"]
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.validate_manifest(missing_action)
        self.assertEqual(failure.exception.code, "manifest_action_set_invalid")

    def test_revision_protocol_and_window_are_explicit(self):
        for field, value, code in (
            ("source_revision", "short", "manifest_invalid"),
            ("protocol", 3, "protocol_unsupported"),
        ):
            with self.subTest(field=field):
                value_copy = copy.deepcopy(self.manifest)
                value_copy[field] = value
                if field == "source_revision":
                    value_copy["release_id"] = value
                    value_copy["client_view"]["source_revision"] = value
                    value_copy["client_view"]["release_id"] = value
                with self.assertRaises(MANIFEST.ManifestError) as failure:
                    MANIFEST.validate_manifest(value_copy)
                self.assertEqual(failure.exception.code, code)

        value_copy = copy.deepcopy(self.manifest)
        value_copy["actions"]["status"]["client_to_control"] = {
            "schema_min": 2, "schema_max": 2, "semantic_min": 1, "semantic_max": 1
        }
        value_copy["client_view"]["actions"] = value_copy["actions"]
        with self.assertRaises(MANIFEST.ManifestError):
            MANIFEST.validate_manifest(value_copy)

        unsupported = copy.deepcopy(self.manifest)
        unsupported["actions"]["future-action"] = copy.deepcopy(unsupported["actions"]["status"])
        unsupported["client_view"]["actions"] = unsupported["actions"]
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.validate_manifest(unsupported)
        self.assertEqual(failure.exception.code, "manifest_action_unsupported")

    def test_legacy_capability_warns_but_unknown_action_does_not_parse(self):
        capability = MANIFEST.legacy_capability("status")
        warnings = MANIFEST.check_compatibility(MANIFEST.legacy_manifest(), "status", capability)
        self.assertTrue(warnings)
        self.assertEqual(warnings[0]["code"], "compatible_release")
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.legacy_capability("future-action")
        self.assertEqual(failure.exception.code, "unknown_action")

        invalid_peer = dict(capability, release_id="not-a-git-revision")
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.check_compatibility(MANIFEST.legacy_manifest(), "status", invalid_peer)
        self.assertEqual(failure.exception.code, "compatibility_invalid")

        missing_release = dict(capability)
        missing_release.pop("release_id")
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.check_compatibility(MANIFEST.legacy_manifest(), "status", missing_release)
        self.assertEqual(failure.exception.code, "compatibility_invalid")

    def test_missing_runtime_manifest_is_an_explicit_legacy_warning(self):
        manifest, present, verified, warnings = MANIFEST.load_runtime_manifest(self.root / "missing.json")
        self.assertIsNone(manifest)
        self.assertFalse(present)
        self.assertTrue(verified)
        self.assertEqual(warnings[0]["code"], "legacy_compatibility")


if __name__ == "__main__":
    unittest.main()
