from __future__ import annotations

import copy
import hashlib
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "skills/agent-vm-ops/scripts"))

from release_manifest import (  # noqa: E402
    PRODUCT_LINUX_ARTIFACTS,
    PRODUCT_MANIFEST_FORMAT,
    default_operations,
    validate_product_manifest,
    validate_release_manifest,
    verify_product_bundle,
)


class ProductManifestTests(unittest.TestCase):
    def test_format_4_manifest_is_python_consumable(self) -> None:
        with tempfile.TemporaryDirectory(prefix="agent-vm-product-python-") as raw_root:
            root = Path(raw_root)
            specs = [("agent_vm.ts", "client", 0o755), ("byte-fs.ts", "support", 0o644), ("client-io.ts", "support", 0o644), ("client-release.ts", "support", 0o644), ("client.ts", "support", 0o644), ("guest-source.ts", "support", 0o644), ("sync-native.ts", "support", 0o644), ("sync_worktree.ts", "support", 0o755)]
            artifacts = []
            for name, role, mode in specs:
                payload = root / name
                payload.write_text("#!/usr/bin/env bun\n" if name == "agent_vm.ts" else f"// {name}\n", encoding="utf-8")
                payload.chmod(mode)
                artifacts.append({"name": name, "path": name, "role": role, "sha256": hashlib.sha256(payload.read_bytes()).hexdigest(), "mode": mode})
            actions = default_operations()
            manifest = {
                "format": PRODUCT_MANIFEST_FORMAT,
                "version": "1.0.0",
                "release_id": "a" * 40,
                "source_revision": "a" * 40,
                "protocol": 4,
                "target": {"os": "source", "arch": "any"},
                "toolchain": {"bun": "1.4.4", "go": "go1.27.3", "cgo_enabled": False},
                "artifacts": artifacts,
                "actions": actions,
                "client_view": {"version": "1.0.0", "source_revision": "a" * 40, "protocol": 4, "transport": "legacy-bun", "actions": actions},
                "state_compatibility": {"reads": ["legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "session-image-binding@1"], "writes": ["legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "session-image-binding@1"]},
            }
            self.assertEqual(validate_product_manifest(manifest), manifest)
            self.assertEqual(validate_release_manifest(manifest), manifest)
            self.assertEqual(verify_product_bundle(root, manifest), manifest)
            malformed = dict(manifest)
            malformed["version"] = "0.1"
            with self.assertRaises(Exception):
                validate_product_manifest(malformed)

    def test_format_4_linux_bundle_requires_web_package_contract(self) -> None:
        with tempfile.TemporaryDirectory(prefix="agent-vm-product-linux-python-") as raw_root:
            root = Path(raw_root)
            artifacts = []
            for name, path, role, mode in sorted(PRODUCT_LINUX_ARTIFACTS, key=lambda item: item[1]):
                payload = root / path
                payload.parent.mkdir(parents=True, exist_ok=True)
                data = b"artifact:" + path.encode()
                if path == "bin/agent-vm-control":
                    data = bytearray(20)
                    data[:7] = b"\x7fELF\x02\x01\x01"
                    data[18:20] = (62).to_bytes(2, "little")
                    data = bytes(data)
                payload.write_bytes(data)
                payload.chmod(mode)
                artifacts.append({"name": name, "path": path, "role": role, "sha256": hashlib.sha256(data).hexdigest(), "mode": mode})
            actions = default_operations()
            manifest = {
                "format": PRODUCT_MANIFEST_FORMAT,
                "version": "1.0.0",
                "release_id": "b" * 40,
                "source_revision": "b" * 40,
                "protocol": 4,
                "target": {"os": "linux", "arch": "amd64"},
                "toolchain": {"bun": "1.4.4", "go": "go1.27.3", "cgo_enabled": False},
                "artifacts": artifacts,
                "actions": actions,
                "client_view": {"version": "1.0.0", "source_revision": "b" * 40, "protocol": 4, "transport": "go-request", "actions": actions},
                "state_compatibility": {"reads": ["legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "session-image-binding@1"], "writes": ["legacy-agent-vm-core@1", "managed-image-catalog@1", "management-operation-audit@1", "session-image-binding@1"]},
            }
            self.assertEqual(validate_product_manifest(manifest), manifest)
            self.assertEqual(verify_product_bundle(root, manifest), manifest)
            self.assertIn("systemd/agent-vm-web.service", {item["path"] for item in manifest["artifacts"]})
            self.assertIn("web-assets.manifest.json", {item["path"] for item in manifest["artifacts"]})

            historical = copy.deepcopy(manifest)
            historical_web = next(item for item in historical["artifacts"] if item["path"] == "config/agent-vm-web.json.example")
            historical_web["mode"] = 0o644
            (root / historical_web["path"]).chmod(0o644)
            with self.assertRaises(Exception):
                validate_product_manifest(historical)
            self.assertEqual(
                validate_product_manifest(historical, allow_historical_web_config_mode=True),
                historical,
            )
            self.assertEqual(
                verify_product_bundle(root, historical, allow_historical_web_config_mode=True),
                historical,
            )


if __name__ == "__main__":
    unittest.main()
