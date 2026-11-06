import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("ci_cache", Path(__file__).with_name("ci_cache.py"))
cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache)


class CacheContractTests(unittest.TestCase):
    def plan(self, **changes):
        inputs = dict(role="portable", platform="Linux", arch="X64", image="ubuntu26", go_version="go1.27.3", bun_version="1.4.4", dependency_hash="deps-a", source_hash="source-a", lock_hash="lock-a", paths={"GOMODCACHE": "/tmp/modules", "GOCACHE": "/tmp/build"})
        inputs.update(changes)
        return cache.cache_plan(**inputs)

    def test_source_changes_refresh_build_but_preserve_compatible_restore(self):
        before, after = self.plan(), self.plan(source_hash="source-b")
        self.assertNotEqual(before["build"]["key"], after["build"]["key"])
        self.assertEqual(before["build"]["prefix"], after["build"]["prefix"])
        self.assertEqual(before["module"], after["module"])
        self.assertEqual(before["bun"], after["bun"])

    def test_role_and_platform_boundaries(self):
        before = self.plan()
        heavy = self.plan(role="heavy")
        self.assertNotEqual(before["build"]["prefix"], heavy["build"]["prefix"])
        self.assertEqual(before["module"], heavy["module"])
        for changes in [dict(platform="macOS"), dict(arch="ARM64"), dict(image="ubuntu28"), dict(go_version="go1.28.0")]:
            after = self.plan(**changes)
            for name in ["module", "build"]:
                self.assertNotEqual(before[name]["prefix"], after[name]["prefix"])
        with self.assertRaises(ValueError):
            self.plan(role="unknown")

    def test_lock_changes_allow_partial_download_reuse(self):
        before = self.plan()
        after = self.plan(dependency_hash="deps-b", lock_hash="lock-b")
        for name in ["module", "bun"]:
            self.assertNotEqual(before[name]["key"], after[name]["key"])
            self.assertEqual(before[name]["prefix"], after[name]["prefix"])
        self.assertNotEqual(before["build"]["prefix"], after["build"]["prefix"])

    def test_bun_retains_legacy_path_version_and_reports_expanded_size(self):
        bun = self.plan()["bun"]
        self.assertEqual(bun["path"], "~/.bun/install/cache")
        self.assertEqual(bun["key"], "bun-1.4.4-Linux-X64-lock-a")
        with tempfile.TemporaryDirectory() as directory:
            with patch.dict("os.environ", {"HOME": directory}):
                target = Path(directory) / ".bun/install/cache"
                target.mkdir(parents=True)
                (target / "package").write_bytes(b"package" * 1024)
                self.assertGreater(cache.size_kib(bun["path"]), 0)

    def test_cold_exact_partial_and_skipped_save_reports(self):
        with tempfile.TemporaryDirectory() as directory:
            caches = self.plan()
            for entry in caches.values():
                entry["path"] = directory
            state = {"started": 10, "caches": caches}
            results = {
                "module": dict(start=10, end=12, matched=caches["module"]["key"]),
                "build": dict(start=12, end=15, matched=caches["build"]["prefix"] + "older"),
                "bun": dict(start=15, end=16, matched=""),
            }
            summary = cache.report(state, "restore", results, 16)
            for status in ["exact-hit", "prefix-hit", "miss", "6.00s"]:
                self.assertIn(status, summary)
            self.assertTrue(caches["module"]["hit"])
            self.assertFalse(caches["build"]["hit"])
            self.assertFalse(caches["bun"]["hit"])
            for result in results.values():
                result["outcome"] = "skipped"
            self.assertIn("skipped", cache.report(state, "save", results, 16))


if __name__ == "__main__":
    unittest.main()
