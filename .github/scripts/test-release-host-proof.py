#!/usr/bin/env python3
"""Offline SSHSIG contracts with disposable keys, Git objects and an isolated agent."""
import base64
import copy
import importlib.util
import io
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch
import zipfile

HERE = Path(__file__).resolve().parent
REPO, SHA, TREE, RUN, ATTEMPT = "owner/project", "a" * 40, "b" * 40, 10, 2


def load():
    path = HERE / "release-host-proof.py"
    assert path.is_file(), "missing remote machine witness verifier"
    spec = importlib.util.spec_from_file_location("release_host_proof", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class HostProofTests(unittest.TestCase):
    def setUp(self):
        self.proof = load()
        self.core = self.proof.core
        self.temp = tempfile.TemporaryDirectory(prefix="release-host-proof-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        git_bin = str(Path(shutil.which("git")).parent)
        self.env = {"PATH": git_bin + ":/usr/bin:/bin", "HOME": str(self.root), "LC_ALL": "C", "SSH_ASKPASS_REQUIRE": "never"}
        self.enterContext(patch.dict(os.environ, self.env, clear=True))
        self.repo = self.root / "controller"
        self.repo.mkdir()
        self.command("git", "init", "-q", str(self.repo))
        self.keys = []
        for index in range(2):
            key = self.root / ("key" + str(index))
            self.command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key))
            self.keys.append(key)
        self.sha = self.commit(self.keys[0].with_suffix(".pub").read_text())
        self.materials = self.make_materials(self.sha)
        # Schema validation belongs to the concurrently integrated producer. This
        # seam keeps real OpenSSH and all local scope/binding checks unmocked.
        self.validator = self.enterContext(patch.object(self.proof.evidence, "validate_machine", return_value=None))

    def command(self, *args, data=None):
        if args[0] in ("ssh-keygen", "ssh-add", "ssh-agent"):
            args = ("/usr/bin/" + args[0], *args[1:])
        return subprocess.run(args, input=data, capture_output=True, check=True, env=self.env, timeout=15).stdout

    def commit(self, public_key=None, mode="100644"):
        entries = []
        if public_key is not None:
            blob = self.command("git", "-C", str(self.repo), "hash-object", "-w", "--stdin", data=public_key.encode()).strip()
            entries.append(mode.encode() + b" blob " + blob + b"\trelease-machine.pub\0")
        subtree = self.command("git", "-C", str(self.repo), "mktree", "-z", data=b"".join(entries)).strip()
        tree = self.command("git", "-C", str(self.repo), "mktree", "-z", data=b"040000 tree " + subtree + b"\t.github\0").strip()
        raw = b"tree " + tree + b"\nauthor CI <ci@example.invalid> 1 +0000\ncommitter CI <ci@example.invalid> 1 +0000\n\nfixture\n"
        sha = self.command("git", "-C", str(self.repo), "hash-object", "-t", "commit", "-w", "--stdin", data=raw).strip().decode()
        self.command("git", "-C", str(self.repo), "checkout", "-q", "--detach", sha)
        return sha

    def make_materials(self, source):
        scope = dict(schema=1, repo=REPO, sha=SHA, tree=TREE, run_id=RUN, run_attempt=ATTEMPT)
        fact = dict(id=30, digest="sha256:" + "c" * 64, receipt_sha256="d" * 64)
        binding = scope | dict(kind="release-full-binding", workflow_sha=source, artifact_sha256="e" * 64,
                               job_id=31, producer="Release Full evidence", **fact)
        lanes = [scope | dict(name=name, status="success", artifact_sha256="e" * 64,
                              full_binding_sha256=self.core.hashed(self.core.encoded(binding))) for name in ("6a", "6b", "6c")]
        machine = scope | dict(kind="release-machine", workflow_sha=source, status="success", verified=True,
                               artifact_sha256="e" * 64, target_artifact=fact, full_binding=binding, lanes=lanes)
        return scope | dict(kind="release-materials", status="collected", verified=False, authoritative=False, publishable=False,
                            workflow_sha=source, artifact_sha256="e" * 64, target=fact, full=binding, machine=fact,
                            intake=fact, full_lanes=[], machine_lanes=lanes, machine_receipt=machine, machine_signature="")

    def agent(self, *, loaded=True):
        socket = self.root / "agent.sock"
        process = subprocess.Popen(["/usr/bin/ssh-agent", "-D", "-a", str(socket)], env=self.env,
                                   stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        def stop():
            process.terminate()
            process.wait(timeout=5)
        self.addCleanup(stop)
        for _ in range(100):
            if socket.exists():
                break
            time.sleep(0.02)
        self.assertTrue(socket.exists(), "isolated test agent did not start")
        self.env["SSH_AUTH_SOCK"] = str(socket)
        os.environ["SSH_AUTH_SOCK"] = str(socket)
        if loaded:
            self.command("ssh-add", str(self.keys[0]))

    def signed(self):
        self.agent()
        self.materials["machine_signature"] = self.proof.sign_payload(self.materials["machine_receipt"], self.repo, self.sha)
        return self.materials

    def test_real_loaded_signer_verifies_without_reading_private_key_or_using_agent_for_verify(self):
        materials = self.signed()
        signature = base64.b64decode(materials["machine_signature"], validate=True)
        self.assertTrue(signature.startswith(b"-----BEGIN SSH SIGNATURE-----\n"))
        os.environ["SSH_AUTH_SOCK"] = str(self.root / "not-an-agent")
        # Dirty working-tree bytes cannot substitute the reviewed immutable key.
        (self.repo / self.proof.KEY_PATH).write_bytes(self.keys[1].with_suffix(".pub").read_bytes())
        decision = self.proof.assess(materials, self.repo, self.sha)
        self.assertEqual(decision["status"], "verified")
        self.assertEqual(decision["materials_sha256"], self.core.hashed(self.core.encoded(materials)))
        self.core.digest(decision["signer_sha256"])
        self.validator.assert_called_with(materials["machine_receipt"], require_full=True)

    def test_receipt_tamper_or_replay_with_consistent_sha_tree_attempt_is_rejected(self):
        original = self.signed()
        for key, value in (("sha", "f" * 40), ("tree", "f" * 40), ("run_id", RUN + 1),
                           ("run_attempt", ATTEMPT + 1), ("artifact_sha256", "f" * 64)):
            materials = copy.deepcopy(original)
            for item in (materials, materials["machine_receipt"], materials["full"],
                         materials["machine_receipt"]["full_binding"], *materials["machine_lanes"]):
                item[key] = value
            binding_hash = self.core.hashed(self.core.encoded(materials["full"]))
            for lane in materials["machine_lanes"]:
                lane["full_binding_sha256"] = binding_hash
            with self.subTest(key=key):
                self.assertEqual(self.proof.assess(materials, self.repo, self.sha)["status"], "rejected")
        materials = copy.deepcopy(original)
        materials["machine_receipt"]["machine_id"] = "0" * 64
        self.assertEqual(self.proof.assess(materials, self.repo, self.sha)["status"], "rejected")

    def test_absent_blob_is_only_no_anchor_case_and_nonempty_signature_cannot_fallback(self):
        absent = self.commit()
        materials = self.make_materials(absent)
        self.assertEqual(self.proof.assess(materials, self.repo, absent)["status"], "no_host_anchor")
        self.assertEqual(self.proof.sign_payload(materials["machine_receipt"], self.repo, absent), "")
        materials["machine_signature"] = "not-a-valid-signature"
        self.assertEqual(self.proof.assess(materials, self.repo, absent)["status"], "rejected")
        materials["machine_signature"] = ""
        self.validator.side_effect = ValueError("invalid machine proof")
        self.assertEqual(self.proof.assess(materials, self.repo, absent)["status"], "rejected")

    def test_configured_key_without_signer_or_signature_never_falls_back(self):
        self.assertEqual(self.proof.assess(self.materials, self.repo, self.sha)["status"], "rejected")
        with self.assertRaises((ValueError, OSError)):
            self.proof.sign_payload(self.materials["machine_receipt"], self.repo, self.sha)
        self.agent(loaded=False)
        with self.assertRaises((ValueError, OSError)):
            self.proof.sign_payload(self.materials["machine_receipt"], self.repo, self.sha)

    def test_wrong_pinned_key_wrong_namespace_and_bad_or_oversized_signature_reject(self):
        materials = self.signed()
        wrong_sha = self.commit(self.keys[1].with_suffix(".pub").read_text())
        self.assertEqual(self.proof.assess(materials, self.repo, wrong_sha)["status"], "rejected")
        same_key_sha = self.commit(self.keys[0].with_suffix(".pub").read_text().rstrip() + " another-policy\n")
        replay = self.make_materials(same_key_sha)
        replay["machine_signature"] = materials["machine_signature"]
        self.assertEqual(self.proof.assess(replay, self.repo, same_key_sha)["status"], "rejected")
        with tempfile.NamedTemporaryFile(dir=self.root) as public:
            public.write(self.keys[0].with_suffix(".pub").read_bytes())
            public.flush()
            raw = self.command("ssh-keygen", "-Y", "sign", "-U", "-f", public.name, "-n", "wrong-namespace",
                               data=self.core.encoded(materials["machine_receipt"]))
        materials["machine_signature"] = base64.b64encode(raw).decode()
        self.assertEqual(self.proof.assess(materials, self.repo, self.sha)["status"], "rejected")
        wrong = self.command("ssh-keygen", "-Y", "sign", "-f", str(self.keys[1]), "-n", self.proof.NAMESPACE,
                             data=self.core.encoded(materials["machine_receipt"]))
        materials["machine_signature"] = base64.b64encode(wrong).decode()
        self.assertEqual(self.proof.assess(materials, self.repo, self.sha)["status"], "rejected")
        for bad in ("x" * (self.proof.MAX_SIGNATURE + 1), "!", "é", "AA==\n"):
            materials["machine_signature"] = bad
            self.assertEqual(self.proof.assess(materials, self.repo, self.sha)["status"], "rejected")

    def test_blob_errors_bad_key_symlink_and_untrusted_source_are_not_absence(self):
        for public, mode in (("not a public key\n", "100644"), (self.keys[0].with_suffix(".pub").read_text(), "120000")):
            source = self.commit(public, mode)
            self.assertEqual(self.proof.assess(self.make_materials(source), self.repo, source)["status"], "rejected")
        self.assertEqual(self.proof.assess(self.materials, self.root, self.sha)["status"], "rejected")
        self.assertEqual(self.proof.assess(self.materials, self.repo, "0" * 40)["status"], "rejected")
        with patch.object(self.proof.core, "git", side_effect=OSError("real Git error")):
            self.assertEqual(self.proof.assess(self.materials, self.repo, self.sha)["status"], "rejected")
        with self.assertRaises(ValueError):
            self.proof.sign_payload(self.materials["machine_receipt"], self.repo, "0" * 40)

    def native_materials(self, materials, decision, extra=None):
        stream = io.BytesIO()
        with zipfile.ZipFile(stream, "w") as archive:
            for name, value in {"receipt.json": materials, "decision.json": decision, **(extra or {})}.items():
                archive.writestr(name, self.core.encoded(value))
        raw = stream.getvalue()
        artifact = dict(id=70, name=f"release-materials-{RUN}-{ATTEMPT}", expired=False,
                        digest="sha256:" + self.core.hashed(raw), workflow_run={"id": RUN},
                        created_at="2026-11-26T01:00:30Z", updated_at="2026-11-26T01:00:30Z")
        job = dict(id=71, name="Release candidate evidence", run_id=RUN, run_attempt=ATTEMPT,
                   status="completed", conclusion="success", started_at="2026-11-26T01:00:01Z", completed_at="2026-11-26T01:01:00Z")
        run = dict(id=RUN, run_attempt=ATTEMPT, repository={"full_name": REPO}, head_repository={"full_name": REPO},
                   run_started_at="2026-11-26T01:00:00Z")
        api = self.core.GitHub(REPO, "offline-test-token")
        def request(method, path, data=None, binary=False):
            self.assertEqual(method, "GET")
            if binary:
                self.assertEqual(path, api.prefix + "/actions/artifacts/70/zip")
                return raw
            suffix = path[len(api.prefix):].split("?", 1)[0]
            if suffix == f"/actions/runs/{RUN}/attempts/{ATTEMPT}":
                return run
            if suffix.endswith("/jobs"):
                return {"jobs": [job]}
            if suffix.endswith("/artifacts"):
                return {"artifacts": [artifact]}
            raise AssertionError("unexpected API read: " + path)
        api.request = request
        return api, artifact, job

    def test_native_two_file_staging_and_verified_enforcement_generate_only_authoritative_output(self):
        materials = self.signed()
        decision = self.proof.assess(materials, self.repo, self.sha)
        api, artifact, _ = self.native_materials(materials, decision)
        intent = {key: materials[key] for key in ("schema", *self.proof.SCOPE)}
        self.assertEqual(self.proof.read_materials(api, intent), (materials, decision, artifact))
        with patch.object(self.proof.control, "guard", return_value=intent), \
             patch.object(self.proof.evidence, "target", return_value=({"artifact_sha256": materials["artifact_sha256"]}, b"binary", materials["target"])) as target:
            value = self.proof.enforce(api, intent, self.repo)
            target.assert_called_once_with(api, intent)
        self.proof.validate_evidence(value, self.repo, self.sha)
        output = self.root / "verified-output"
        with patch.dict(os.environ, {"GITHUB_REPOSITORY": REPO, "GITHUB_RUN_ID": str(RUN), "GITHUB_RUN_ATTEMPT": str(ATTEMPT)}), \
             patch.object(self.proof.control, "AdmissionAPI", return_value=api), \
             patch.object(self.proof.control, "guard", return_value=intent), \
             patch.object(self.proof.evidence, "target", return_value=({"artifact_sha256": materials["artifact_sha256"]}, b"binary", materials["target"])), \
             patch.object(os.sys, "argv", ["release-host-proof.py", "enforce", "--control", str(self.repo), "--output", str(output)]):
            self.proof.main()
        self.assertEqual(self.core.decode((output / "evidence.json").read_bytes(), self.proof.MAX_MATERIALS + 4096), value)
        self.assertTrue(value["verified"] and value["authoritative"])
        self.assertFalse(value["publishable"])
        changed = copy.deepcopy(value)
        changed["machine_receipt"]["run_attempt"] += 1
        with self.assertRaises(ValueError):
            self.proof.validate_evidence(changed, self.repo, self.sha)
        forged = decision | {"materials_sha256": "0" * 64}
        api, _, _ = self.native_materials(materials, forged)
        with self.assertRaises(ValueError):
            self.proof.read_materials(api, intent)
        api, _, _ = self.native_materials(materials, decision, {"unapproved.json": {}})
        with self.assertRaises(ValueError):
            self.proof.read_materials(api, intent)

    def test_native_materials_reject_foreign_expired_inherited_or_failed_producer(self):
        absent = self.commit()
        materials = self.make_materials(absent)
        decision = self.proof.assess(materials, self.repo, absent)
        intent = {key: materials[key] for key in ("schema", *self.proof.SCOPE)}
        for change in ({"expired": True}, {"workflow_run": {"id": RUN + 1}}, {"created_at": "2026-11-25T01:00:00Z"}):
            api, artifact, _ = self.native_materials(materials, decision)
            artifact.update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.proof.read_materials(api, intent)
        for change in ({"run_attempt": 1}, {"conclusion": "failure"}, {"started_at": "2026-11-25T01:00:00Z"}):
            api, _, job = self.native_materials(materials, decision)
            job.update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.proof.read_materials(api, intent)

    def test_no_signing_cli_and_named_enforcement_writes_nothing_without_anchor(self):
        result = subprocess.run([os.sys.executable, str(HERE / "release-host-proof.py"), "sign"], env=self.env,
                                capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        absent = self.commit()
        materials = self.make_materials(absent)
        intent = {key: materials[key] for key in ("schema", "repo", "sha", "tree", "run_id", "run_attempt", "workflow_sha")}
        decision = self.proof.assess(materials, self.repo, absent)
        api, _, _ = self.native_materials(materials, decision)
        with patch.object(self.proof.control, "guard", return_value=intent):
            with self.assertRaisesRegex(ValueError, "远端机器"):
                self.proof.enforce(api, intent, self.repo)
        output = self.root / "denied-output"
        with patch.dict(os.environ, {"GITHUB_REPOSITORY": REPO, "GITHUB_RUN_ID": str(RUN), "GITHUB_RUN_ATTEMPT": str(ATTEMPT)}), \
             patch.object(self.proof.control, "AdmissionAPI", return_value=api), \
             patch.object(self.proof.control, "guard", return_value=intent), \
             patch.object(os.sys, "argv", ["release-host-proof.py", "enforce", "--control", str(self.repo), "--output", str(output)]):
            with self.assertRaisesRegex(ValueError, "远端机器"):
                self.proof.main()
        self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
