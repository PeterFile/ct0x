#!/usr/bin/env python3
"""Offline host binding tests; every write stays in a synthetic account home."""
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import runpy
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("release-host.py")
REQUIRED = set("driver_6a driver_6b observer_6b audit_6b facts_6b prepare_6b seed_6b driver_6c audit_6c seed_6c legacy_6a runner_6b verifier_6b export_fixture native_exact native_preliminary native_project_stage normal_entry compare_first_turn".split())
SUPPORT = set("six_b six_c six_b_http six_a_original native_transcript".split())
host = None
if SCRIPT.is_file():
    spec = importlib.util.spec_from_file_location("release_host", SCRIPT)
    host = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(host)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class HostTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(host, "缺少稳定用户锚点和私有绑定模块")
        self.temp = tempfile.TemporaryDirectory(dir=Path("/tmp").resolve())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.home = self.root / "account"
        self.home.mkdir(mode=0o700)
        account = patch.object(host.pwd, "getpwuid", return_value=SimpleNamespace(pw_dir=str(self.home)))
        account.start()
        self.addCleanup(account.stop)
        self.volume = self.root / "volume"
        self.volume.mkdir(mode=0o700)
        self.harness = self.volume / "harness"
        self.harness.mkdir(mode=0o700)
        info = self.volume.stat()
        self.config = {"schema": 1, "hdd": {"path": str(self.volume), "device": info.st_dev, "inode": info.st_ino},
                       "harness": str(self.harness), "sources": {}, "support": {}}
        for name in REQUIRED:
            path = self.harness / (name + (".go" if name.startswith("seed_") else ".py"))
            path.write_text("# audited " + name + "\n")
            self.config["sources"][name] = {"path": str(path), "sha256": digest(path)}
        for name in SUPPORT:
            directory = self.volume / name
            directory.mkdir(mode=0o700)
            helper = directory / "helper.py"
            helper.write_text("# support " + name + "\n")
            self.config["support"][name] = {"path": str(directory), "files": {helper.name: digest(helper)}}
        self.manifest = self.root / "operator.json"
        self.write_manifest(self.config)

    def write_manifest(self, value):
        self.manifest.write_text(json.dumps(value))
        self.manifest.chmod(0o600)

    def bind(self):
        host.bind(self.manifest)
        return host.registry_root()

    def test_anchor_uses_account_home_not_scratch_hostname_or_home_env(self):
        expected = self.home / "Library/Application Support/agent-workspace/release-machine"
        self.assertEqual(host.registry_root(), expected)
        with patch.dict(os.environ, HOME=str(self.root / "other")), patch.object(os, "uname", return_value=SimpleNamespace(nodename="other-host")):
            (self.root / "different-scratch").mkdir()
            self.assertEqual(host.registry_root(), expected)
        self.assertFalse(expected.exists(), "computing the anchor must not install state")

    def test_bind_is_private_atomic_and_idempotent(self):
        root = self.bind()
        expected = (root / "binding.json").read_bytes()
        host.bind(self.manifest)
        self.assertEqual((root / "binding.json").read_bytes(), expected)
        self.assertEqual(root.stat().st_mode & 0o777, 0o700)
        self.assertEqual((root / "binding.json").stat().st_mode & 0o777, 0o600)
        self.assertEqual(sorted(path.name for path in root.iterdir()), ["binding.json"])
        self.assertEqual(host.load_binding(), self.config)

    def test_unbound_load_never_creates_registry(self):
        with self.assertRaises((ValueError, OSError)):
            host.load_binding()
        self.assertFalse(host.registry_root().exists())

    def test_existing_identity_cannot_change_with_new_scratch(self):
        root = self.bind()
        changed = copy.deepcopy(self.config)
        alternate = self.volume / "other-harness"
        alternate.mkdir(mode=0o700)
        changed["harness"] = str(alternate)
        self.write_manifest(changed)
        with self.assertRaises(ValueError):
            host.bind(self.manifest)
        self.assertEqual(host.load_binding(), self.config)
        self.assertEqual(host.registry_root(), root)

    def test_changed_driver_and_support_bytes_are_not_reapproved(self):
        self.bind()
        paths = [Path(self.config["sources"]["driver_6a"]["path"]), self.volume / "six_b/helper.py"]
        for path in paths:
            with self.subTest(path=path.name):
                original = path.read_bytes()
                path.write_bytes(original + b"# changed\n")
                with self.assertRaises(ValueError):
                    host.load_binding()
                with self.assertRaises(ValueError):
                    host.bind(self.manifest)
                path.write_bytes(original)

    def test_support_set_rejects_new_or_missing_importable_sources(self):
        self.bind()
        helper = self.volume / "six_b/helper.py"
        extra = helper.with_name("new_dependency.py")
        extra.write_text("# unreviewed\n")
        with self.assertRaises(ValueError):
            host.load_binding()
        extra.unlink()
        helper.unlink()
        with self.assertRaises(ValueError):
            host.load_binding()

    def test_registry_and_config_permissions_are_enforced(self):
        root = self.bind()
        for path, bad_mode, good_mode in ((root, 0o755, 0o700), (root / "binding.json", 0o644, 0o600)):
            path.chmod(bad_mode)
            with self.assertRaises(ValueError):
                host.load_binding()
            path.chmod(good_mode)
        with patch.object(host.os, "getuid", return_value=os.getuid() + 1):
            with self.assertRaises(ValueError):
                host.load_binding()

    def test_symlinked_registry_or_parent_is_rejected(self):
        library = self.home / "Library"
        outside = self.root / "redirect"
        outside.mkdir(mode=0o700)
        library.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(ValueError):
            host.bind(self.manifest)
        self.assertEqual(list(outside.iterdir()), [])

    def test_symlinked_binding_or_operator_manifest_is_rejected(self):
        root = self.bind()
        target = root / "binding.json"
        target.unlink()
        target.symlink_to(self.manifest)
        with self.assertRaises(ValueError):
            host.load_binding()
        alias = self.root / "alias.json"
        alias.symlink_to(self.manifest)
        with self.assertRaises(ValueError):
            host.bind(alias)

    def test_source_symlink_and_parent_escape_are_rejected(self):
        path = Path(self.config["sources"]["driver_6a"]["path"])
        other = self.root / "outside.py"
        other.write_bytes(path.read_bytes())
        path.unlink()
        path.symlink_to(other)
        with self.assertRaises(ValueError):
            host.bind(self.manifest)
        path.unlink()
        directory = self.harness / "alias"
        directory.symlink_to(self.root, target_is_directory=True)
        self.config["sources"]["driver_6a"]["path"] = str(directory / other.name)
        self.write_manifest(self.config)
        with self.assertRaises(ValueError):
            host.bind(self.manifest)

    def test_manifest_cannot_freeze_controller_or_omit_required_logical_sources(self):
        for change in ("release_machine", "omit"):
            value = copy.deepcopy(self.config)
            if change == "omit":
                value["sources"].pop("driver_6a")
            else:
                value["sources"][change] = value["sources"]["driver_6a"]
            self.write_manifest(value)
            with self.assertRaises(ValueError):
                host.bind(self.manifest)

    def test_config_tampering_duplicate_fields_and_public_operator_manifest_fail(self):
        root = self.bind()
        target = root / "binding.json"
        original = target.read_bytes()
        for data in (b'{"schema":1,"schema":1}', b'{}', b'x' * 65537):
            target.write_bytes(data)
            with self.assertRaises(ValueError):
                host.load_binding()
        target.write_bytes(original)
        self.manifest.chmod(0o644)
        with self.assertRaises(ValueError):
            host.bind(self.manifest)

    def test_opened_binding_must_still_be_private(self):
        root = self.bind()
        target = root / "binding.json"
        original = os.open
        def raced_open(path, *args, **kwargs):
            if Path(path) == target:
                target.chmod(0o644)
            return original(path, *args, **kwargs)
        with patch.object(host.os, "open", side_effect=raced_open):
            with self.assertRaises(ValueError):
                host.load_binding()

    def test_failed_atomic_publish_leaves_no_binding_or_staging_file(self):
        with patch.object(host.os, "link", side_effect=OSError("synthetic interruption")):
            with self.assertRaises(OSError):
                host.bind(self.manifest)
        self.assertEqual(list(host.registry_root().iterdir()), [])
        self.bind()
        self.assertEqual(host.load_binding(), self.config)

    def test_volume_identity_and_lexical_escape_are_rejected(self):
        for field in ("device", "inode"):
            value = copy.deepcopy(self.config)
            value["hdd"][field] += 1
            self.write_manifest(value)
            with self.assertRaises(ValueError):
                host.bind(self.manifest)
        value = copy.deepcopy(self.config)
        value["harness"] = str(self.volume / "harness/../harness")
        self.write_manifest(value)
        with self.assertRaises(ValueError):
            host.bind(self.manifest)

    def test_scope_checks_new_claims_even_after_scratch_moves(self):
        self.bind()
        for name in ("scratch-one", "scratch-two"):
            work = self.volume / name
            work.mkdir(mode=0o700)
            self.assertEqual(host.validate_scope(work), work)
            claim = work / "claim"
            claim.mkdir(mode=0o700)
            self.assertEqual(host.validate_scope(claim), claim)
        outside = self.root / "outside"
        outside.mkdir(mode=0o700)
        alias = self.volume / "alias"
        alias.symlink_to(outside, target_is_directory=True)
        for path in (outside, alias):
            with self.assertRaises(ValueError):
                host.validate_scope(path)
        work.chmod(0o755)
        with self.assertRaises(ValueError):
            host.validate_scope(work)

    def test_bind_cli_uses_only_synthetic_account_and_sanitizes_failure(self):
        output = io.StringIO()
        with patch("sys.argv", [str(SCRIPT), "bind", "--manifest", str(self.manifest)]), patch("sys.stdout", output):
            runpy.run_path(str(SCRIPT), run_name="__main__")
        self.assertNotIn(str(self.root), output.getvalue())
        self.assertEqual(host.load_binding(), self.config)
        self.manifest.chmod(0o644)
        error = io.StringIO()
        with patch("sys.argv", [str(SCRIPT), "bind", "--manifest", str(self.manifest)]), patch("sys.stderr", error):
            with self.assertRaises(SystemExit) as caught:
                runpy.run_path(str(SCRIPT), run_name="__main__")
        self.assertEqual(caught.exception.code, 1)
        self.assertNotIn(str(self.root), error.getvalue())

    def test_adapter_exposes_named_hashes_and_complete_sanitized_support_bundle(self):
        self.bind()
        paths, public, bundle = host.source_manifest(self.harness)
        self.assertEqual(set(paths), REQUIRED)
        self.assertEqual(set(public), REQUIRED)
        self.assertEqual(public, {key: digest(path) for key, path in paths.items()})
        expected = dict(public)
        for group, row in self.config["support"].items():
            expected.update({f"support:{group}/{name}": value for name, value in row["files"].items()})
        self.assertEqual(bundle, expected)
        self.assertNotIn(str(self.root), json.dumps(bundle))
        self.assertEqual(host.source_manifest(self.harness), (paths, public, bundle))
        with self.assertRaises(ValueError):
            host.source_manifest(self.volume)


if __name__ == "__main__":
    unittest.main()
