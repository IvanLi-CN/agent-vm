from __future__ import annotations

import hashlib
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "skills/agent-vm-ops/scripts"))

from release_manifest import (  # noqa: E402
    PRODUCT_MANIFEST_FORMAT,
    default_operations,
    validate_product_manifest,
    validate_release_manifest,
    verify_product_bundle,
)


class ProductManifestTests(unittest.TestCase):
    def test_format_3_manifest_is_python_consumable(self) -> None:
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
                "source_revision": "a" * 40,
                "protocol": 4,
                "target": {"os": "source", "arch": "any"},
                "toolchain": {"bun": "1.4.4", "go": "go1.27.3", "cgo_enabled": False},
                "artifacts": artifacts,
                "actions": actions,
                "client_view": {"version": "1.0.0", "source_revision": "a" * 40, "protocol": 4, "transport": "legacy-bun", "actions": actions},
            }
            self.assertEqual(validate_product_manifest(manifest), manifest)
            self.assertEqual(validate_release_manifest(manifest), manifest)
            self.assertEqual(verify_product_bundle(root, manifest), manifest)
            malformed = dict(manifest)
            malformed["version"] = "0.1"
            with self.assertRaises(Exception):
                validate_product_manifest(malformed)


if __name__ == "__main__":
    unittest.main()
