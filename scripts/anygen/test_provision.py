import contextlib
import base64
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import anygen


class FakePlatform:
    """Stateful platform model. No network, real tokens or production mutation."""
    key = "sk-ag-test-platform-credential-ABCDE"
    key_id = "key-test"
    base = anygen.ORIGIN + "/v1/openapi/anyclaw/app/app-test"

    def __init__(self):
        self.events = []
        self.files = []
        self.hooks = []
        self.published = False
        self.grant = []
        self.fail_after = None
        self.empty_output = False
        self.diagnostics = []
        self.mismatched_mask = False
        self.direct_keys = []

    def transport(self, client, method, path, body=None, gateway=False, **kwargs):
        if gateway:
            action, data = body["method"], body["body"]
            self.events.append(action)
            if action == "createClaw":
                result = {"claw_token": "claw-test", "access_token": "private-claw-secret"}
            elif action == "listClawAppFiles":
                result = {"files": self.files}
            elif action == "deployClawApp":
                with tarfile.open(fileobj=io.BytesIO(base64.b64decode(data["bundle"])), mode="r:gz") as tar:
                    self.files = [{"path": p} for p in tar.getnames()]
                result = {"ok": True, "version_id": "v1", "diagnostics": self.diagnostics}
            elif action == "listClawAppActions":
                result = {"actions": [{"action": a} for a in anygen.ACTIONS]}
            elif action == "getClawAppPublishStatus":
                result = {"published": self.published, "url": self.base if self.published else ""}
            elif action == "listClawAppHooks":
                result = {"hooks": list(self.hooks)}
            elif action == "createClawAppHook":
                self.published = True
                self.hooks = [{"id": "hook-test", "action": anygen.ACTIONS[0]}]
                result = {"hook": self.hooks[0], "url": self.base + "/-/hook/mak_test_private"}
            elif action == "deleteClawAppHook":
                self.hooks = [h for h in self.hooks if h["id"] != data["id"]]
                result = {"hooks": []}
            elif action in ("listClawAppKeyGrants", "setClawAppKeyGrant"):
                if action == "setClawAppKeyGrant":
                    self.grant = data["actions"]
                grant = {"api_key_id": self.key_id, "actions": self.grant, "whole_app": False,
                         "api_key_masked": "sk-****WRONG" if self.mismatched_mask else "sk-****ABCDE"}
                result = {"grants": [grant]} if action.startswith("list") else {"grant": grant}
            else:
                raise AssertionError("unexpected gateway action: " + action)
            if self.fail_after == action:
                raise anygen.Failure("response lost")
            return result
        if path == "/api/page/openapi/keys":
            self.events.append("create_key" if method == "POST" else "list_keys")
            if method == "GET":
                return {"success": True, "data": {"keys": [{"api_key_id": self.key_id,
                    "api_key_masked": "sk-****WRONG" if self.mismatched_mask else "sk-****ABCDE"}]}}
            self.grant = list(anygen.ACTIONS)  # exact features granted at creation
            return {"success": True, "data": {"api_key_id": self.key_id, "api_key": self.key}}
        if path == "/v1/openapi/key/verify":
            return {"verified": True, "user_id": "123", "credits": "10"}
        self.direct_keys.append(client.runtime_key)
        if path == "/v1/openapi/anyclaw/app/app-test/api/v1/models":
            self.events.append("models")
            return {"object": "list", "data": [{"id": "model-test"}]}
        if path == "/v1/openapi/anyclaw/app/app-test/api/v1/chat/completions":
            self.events.append("chat")
            return {"object": "chat.completion", "model": "model-test", "choices": [{
                "message": {"role": "assistant", "content": "" if self.empty_output else "ANYGEN_PROXY_OK"}
            }], "usage": {"total_tokens": 12}}
        raise AssertionError("unexpected path: " + path)


class ProvisionTests(unittest.TestCase):
    def provision(self, directory, fake, *extra):
        out = io.StringIO()
        argv = ["--gateway-auth", "cookie", "provision", "--name", "proxy",
                "--state-dir", str(directory), "--apply", *extra]
        if "--create-key" not in extra:
            argv.extend(["--key-id", fake.key_id])
        with patch.dict(os.environ, {"ANYGEN_SESSION": "test-session", "ANYGEN_CSRF_TOKEN": "test-csrf",
                                     "ANYGEN_LLM_KEY": fake.key}), \
                patch.object(anygen.Client, "request",
                             new=lambda client, *a, **kw: fake.transport(client, *a, **kw)), \
                contextlib.redirect_stdout(out):
            anygen.main(argv)
        return json.loads(out.getvalue()), out.getvalue()

    def test_plan_needs_no_credentials_and_changes_no_files(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "new"
            out = io.StringIO()
            with patch.dict(os.environ, {}, clear=True), \
                    patch.object(anygen.Client, "request") as request, \
                    contextlib.redirect_stdout(out):
                anygen.main(["provision", "--name", "test-proxy", "--state-dir", str(state)])
            self.assertEqual(json.loads(out.getvalue())["status"], "plan")
            self.assertFalse(state.exists())
            request.assert_not_called()

    def test_bundle_is_reproducible_with_manifest_at_root(self):
        from provision import app_bundle
        first, fingerprint = app_bundle()
        second, again = app_bundle()
        self.assertEqual((first, fingerprint), (second, again))
        with tarfile.open(fileobj=io.BytesIO(first), mode="r:gz") as archive:
            self.assertIn("manifest.json", archive.getnames())
            self.assertIn("package.json", archive.getnames())
            self.assertIn("api/lambda/llm.ts", archive.getnames())
            self.assertIn("src/App.tsx", archive.getnames())
            self.assertFalse(any(name.startswith("app/") for name in archive.getnames()))
            manifest = json.load(archive.extractfile("manifest.json"))
            self.assertEqual(manifest["capabilities"], ["llm"])
            self.assertFalse(manifest["teamVisible"])

    def test_create_key_requires_cookie_before_network_or_files(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "new"
            with patch.object(anygen.Client, "request") as request, \
                    self.assertRaisesRegex(anygen.Failure, "cookie"):
                anygen.main(["provision", "--name", "proxy", "--state-dir", str(state),
                             "--create-key", "--apply"])
            self.assertFalse(state.exists())
            request.assert_not_called()

    def test_charge_requires_explicit_model(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(anygen.Failure, "model"):
                anygen.main(["provision", "--name", "proxy", "--state-dir", directory,
                             "--key-id", "key-test", "--allow-charge", "--apply"])

    def test_complete_provision_creates_direct_endpoint_and_private_credentials(self):
        fake = FakePlatform()
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            result, output = self.provision(state, fake, "--allow-charge", "--model", "model-test")
            self.assertEqual(result["status"], "verified")
            self.assertFalse(result["llm_proxy_changes"])
            self.assertEqual(result["base_url"], fake.base + "/api/v1")
            creds = json.loads((state / "credentials.json").read_text())
            self.assertEqual(creds["api_key"], fake.key)
            self.assertEqual((state / "credentials.json").stat().st_mode & 0o777, 0o600)
            self.assertEqual(state.stat().st_mode & 0o777, 0o700)
            self.assertNotIn(fake.key, output + (state / "state.json").read_text())
            self.assertNotIn("private-claw-secret", (state / "state.json").read_text())
            self.assertNotIn("mak_test", (state / "state.json").read_text())
            self.assertFalse(fake.hooks)
            self.assertEqual(fake.events.count("chat"), 1)
            self.assertTrue(all(key == fake.key for key in fake.direct_keys))

    def test_unverified_resume_only_charges_once_without_recreating(self):
        fake = FakePlatform()
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            result, _ = self.provision(state, fake)
            self.assertEqual(result["status"], "ready_unverified")
            self.assertNotIn("chat", fake.events)
            self.provision(state, fake, "--allow-charge", "--model", "model-test")
            self.provision(state, fake, "--allow-charge", "--model", "model-test")
            for event in ("createClaw", "deployClawApp", "createClawAppHook", "chat"):
                self.assertEqual(fake.events.count(event), 1, event)

    def test_unknown_create_outcome_is_never_retried(self):
        fake = FakePlatform()
        fake.fail_after = "createClaw"
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            with self.assertRaises(anygen.Failure):
                self.provision(state, fake)
            fake.fail_after = None
            with self.assertRaisesRegex(anygen.Failure, "unknown"):
                self.provision(state, fake)
            self.assertEqual(fake.events.count("createClaw"), 1)

    def test_deployment_diagnostics_stop_before_publication(self):
        fake = FakePlatform()
        fake.diagnostics = ["Type check failed"]
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(anygen.Failure, "deployment"):
                self.provision(Path(directory) / "run", fake)
            self.assertNotIn("createClawAppHook", fake.events)

    def test_empty_text_is_not_verified_and_not_auto_retried(self):
        fake = FakePlatform()
        fake.empty_output = True
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            with self.assertRaisesRegex(anygen.Failure, "usable"):
                self.provision(state, fake, "--allow-charge", "--model", "model-test")
            with self.assertRaisesRegex(anygen.Failure, "unknown"):
                self.provision(state, fake, "--allow-charge", "--model", "model-test")
            self.assertEqual(fake.events.count("chat"), 1)

    def test_new_key_mode_uses_generated_key_and_does_not_recreate_it(self):
        fake = FakePlatform()
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            result, _ = self.provision(state, fake, "--create-key", "--allow-charge", "--model", "model-test")
            self.assertEqual(result["status"], "verified")
            self.provision(state, fake, "--create-key")
            self.assertEqual(fake.events.count("create_key"), 1)
            self.assertTrue(all(key == fake.key for key in fake.direct_keys))

    def test_mismatched_secret_never_grants_permissions(self):
        fake = FakePlatform()
        fake.mismatched_mask = True
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(anygen.Failure, "mask"):
                self.provision(Path(directory) / "run", fake)
            self.assertNotIn("setClawAppKeyGrant", fake.events)
            self.assertNotIn("createClaw", fake.events)

    def test_lost_journal_does_not_recreate_resources(self):
        fake = FakePlatform()
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            self.provision(state, fake)
            (state / "state.json").unlink()
            with self.assertRaisesRegex(anygen.Failure, "journal missing"):
                self.provision(state, fake)
            self.assertEqual(fake.events.count("createClaw"), 1)

    def test_other_smoke_model_is_not_reported_as_already_verified(self):
        fake = FakePlatform()
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            self.provision(state, fake, "--allow-charge", "--model", "model-test")
            with self.assertRaisesRegex(anygen.Failure, "different model"):
                self.provision(state, fake, "--allow-charge", "--model", "model-other")
            self.assertEqual(fake.events.count("chat"), 1)

    def test_unknown_hook_result_does_not_create_another(self):
        fake = FakePlatform()
        fake.fail_after = "createClawAppHook"
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            with self.assertRaises(anygen.Failure):
                self.provision(state, fake)
            with self.assertRaisesRegex(anygen.Failure, "unknown"):
                self.provision(state, fake)
            self.assertEqual(fake.events.count("createClawAppHook"), 1)

    def test_journal_refuses_concurrent_run_and_symlink(self):
        from provision_state import Journal, StateError
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory) / "run"
            with Journal(state):
                with self.assertRaises(StateError):
                    with Journal(state):
                        pass
            link = Path(directory) / "link"
            link.symlink_to(state, target_is_directory=True)
            with self.assertRaises(StateError):
                with Journal(link):
                    pass


if __name__ == "__main__":
    unittest.main()
