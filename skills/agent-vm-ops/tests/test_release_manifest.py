"""Contract tests for release identity, integrity, and compatibility windows."""
from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import stat
import tempfile
import unittest


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
        external_client = outside / "agent_vm.py"
        external_client.write_bytes(b"client\n")
        external_client.chmod(0o755)
        self.addCleanup(external_client.unlink)
        value = copy.deepcopy(self.manifest)
        value["artifacts"][0]["path"] = "nested/agent_vm.py"
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
