"""Contract tests for the format-2 Go control publication."""

from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location(
    "agent_vm_release_manifest_go", ROOT / "skills/agent-vm-ops/scripts/release_manifest.py"
)
assert SPEC and SPEC.loader
MANIFEST = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MANIFEST)


class GoManifestContractTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        artifacts = []
        for name, path, role, mode in sorted(MANIFEST.GO_FIXED_ARTIFACTS):
            content = (
                b"\x7fELF\x02\x01\x01\x00" + b"\x00" * 10 + b"\x3e\x00" + b"\x00" * 42
                if path == "bin/agent-vm-control"
                else f"{role}:{path}\n".encode()
            )
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
        self.manifest = MANIFEST.build_go_manifest("b" * 40, artifacts)

    def test_format_two_bundle_is_accepted_without_running_binary(self) -> None:
        checked = MANIFEST.verify_go_bundle(self.root, self.manifest)
        self.assertEqual(checked["format"], 2)
        self.assertEqual(checked["toolchain"]["go"], "go1.27.3")
        self.assertEqual(checked["toolchain"]["cgo_enabled"], False)
        self.assertEqual(checked["client_view"]["transport"], "go-request")
        self.assertEqual(checked["retired_units"], list(MANIFEST.GO_RETIRED_UNITS))

    def test_format_two_https_transport_round_trips_through_python(self) -> None:
        manifest = MANIFEST.build_go_manifest("b" * 40, self.manifest["artifacts"], transport="https")
        checked = MANIFEST.verify_go_bundle(self.root, manifest)
        self.assertEqual(checked["client_view"]["transport"], "https")
        self.assertEqual(MANIFEST.validate_release_manifest(manifest), manifest)

    def test_format_one_loader_remains_strict_but_generic_loader_accepts_format_two(self) -> None:
        path = self.root / MANIFEST.MANIFEST_NAME
        path.write_text(json.dumps(self.manifest), encoding="utf-8")
        with self.assertRaises(MANIFEST.ManifestError) as failure:
            MANIFEST.load_manifest(path)
        self.assertEqual(failure.exception.code, "manifest_invalid")
        self.assertEqual(MANIFEST.load_release_manifest(path)["format"], 2)

    def test_transport_target_and_artifact_path_are_fixed(self) -> None:
        for mutate in (
            lambda value: value["client_view"].update(transport="unknown"),
            lambda value: value["target"].update(arch="darwin"),
            lambda value: value["retired_units"].pop(),
            lambda value: value["artifacts"][0].update(path="../candidate"),
        ):
            value = copy.deepcopy(self.manifest)
            mutate(value)
            with self.subTest(value=value):
                with self.assertRaises(MANIFEST.ManifestError):
                    MANIFEST.validate_go_manifest(value)


if __name__ == "__main__":
    unittest.main()
