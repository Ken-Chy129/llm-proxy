import contextlib
import io
import json
import os
import unittest
import urllib.error
from email.message import Message
from unittest.mock import patch

import anygen


class AnyGenTests(unittest.TestCase):
    def run_cli(self, *args, responses=()):
        out = io.StringIO()
        with patch.dict(os.environ, {"ANYGEN_LLM_KEY": "test-key"}), \
                patch.object(anygen.Client, "request", side_effect=responses) as request, \
                contextlib.redirect_stdout(out):
            anygen.main(list(args))
        return json.loads(out.getvalue()), request

    def test_grant_defaults_to_offline_preview(self):
        result, request = self.run_cli("grant", "--claw", "new-claw", "--key-id", "key-new")
        request.assert_not_called()
        self.assertFalse(result["body"]["whole_app"])
        self.assertEqual(result["body"]["actions"], anygen.ACTIONS)

    def test_grant_requires_both_actions_before_write(self):
        with self.assertRaisesRegex(anygen.Failure, "actions"):
            self.run_cli("grant", "--claw", "new-claw", "--key-id", "key-new", "--apply",
                         responses=[{"actions": [{"action": anygen.ACTIONS[0]}]}])

    def test_grant_does_not_overwrite_existing_different_permissions(self):
        with self.assertRaisesRegex(anygen.Failure, "existing grant"):
            self.run_cli("grant", "--claw", "new-claw", "--key-id", "key-new", "--apply",
                         responses=[{"actions": [{"action": a} for a in anygen.ACTIONS]},
                                    {"grants": [{"api_key_id": "key-new", "whole_app": True,
                                                 "actions": []}]}])

    def test_identical_grant_is_noop(self):
        grant = {"api_key_id": "key-new", "whole_app": False, "actions": anygen.ACTIONS}
        result, request = self.run_cli("grant", "--claw", "new-claw", "--key-id", "key-new",
                                       "--apply", responses=[
                                           {"actions": [{"action": a} for a in anygen.ACTIONS]},
                                           {"grants": [grant]}])
        self.assertEqual(result["status"], "unchanged")
        self.assertEqual(request.call_count, 2)

    def test_smoke_requires_cost_opt_in_before_network(self):
        with self.assertRaisesRegex(anygen.Failure, "allow-charge"):
            self.run_cli("smoke", "--ref", "app-new", "--model", "model-new")

    def test_smoke_rejects_empty_success(self):
        with self.assertRaisesRegex(anygen.Failure, "usable"):
            self.run_cli("smoke", "--ref", "app-new", "--model", "model-new", "--allow-charge",
                         responses=[{"object": "chat.completion", "choices": [
                             {"message": {"content": ""}, "finish_reason": "length"}]}])

    def test_check_rejects_unverified_key(self):
        with self.assertRaisesRegex(anygen.Failure, "verified"):
            self.run_cli("check", "--ref", "app-new", responses=[{"verified": False}])

    def test_check_models_and_credits_use_different_paths(self):
        result, request = self.run_cli("check", "--ref", "app-new", responses=[
            {"verified": True, "credits": "10"},
            {"object": "list", "data": [{"id": "model-new"}]}])
        self.assertEqual(result["models"], ["model-new"])
        self.assertEqual(request.call_args_list[0].args[1], "/v1/openapi/key/verify")
        self.assertEqual(request.call_args_list[1].args[1],
                         "/v1/openapi/anyclaw/app/app-new/api/v1/models")

    def test_invalid_ref_cannot_change_destination(self):
        with self.assertRaises(anygen.Failure):
            anygen.app_path("../wrong?key=x", "")

    def test_gateway_business_error_on_http_200_is_failure(self):
        with self.assertRaisesRegex(anygen.Failure, "4101"):
            anygen.decode_response(b'{"code":4101,"msg":"sensitive"}', gateway=True)

    def test_gateway_success_is_unwrapped(self):
        self.assertEqual(anygen.decode_response(b'{"code":0,"data":{"actions":[]}}', True),
                         {"actions": []})

    def test_non_json_does_not_leak_body(self):
        with self.assertRaisesRegex(anygen.Failure, "JSON") as error:
            anygen.decode_response(b'<html>private token</html>', False)
        self.assertNotIn("private token", str(error.exception))

    def test_named_slot_path_and_reserved_names(self):
        self.assertEqual(anygen.app_path("app-new", "llm"),
                         "/v1/openapi/anyclaw/app/app-new/llm/api/v1")
        for slot in ("app", "apps", "default", "a" * 33, "../llm"):
            with self.subTest(slot=slot), self.assertRaises(anygen.Failure):
                anygen.app_path("app-new", slot)

    def test_transport_uses_fixed_origin_and_bearer(self):
        args = anygen.parser().parse_args(["check", "--ref", "app-new"])
        client = anygen.Client(args)
        with patch.dict(os.environ, {"ANYGEN_LLM_KEY": "test-key"}), \
                patch.object(client.opener, "open") as opened:
            opened.return_value.__enter__.return_value.read.return_value = b'{"verified":true}'
            self.assertTrue(client.request("GET", "/v1/openapi/key/verify")["verified"])
            req = opened.call_args.args[0]
            self.assertEqual(req.full_url, "https://www.anygen.io/v1/openapi/key/verify")
            self.assertEqual(req.get_header("Authorization"), "Bearer test-key")
            self.assertIsNone(req.get_header("Cookie"))
            self.assertIsNone(req.get_header("Origin"))

    def test_cookie_management_transport(self):
        args = anygen.parser().parse_args(["--gateway-auth", "cookie", "inspect", "--claw", "new"])
        client = anygen.Client(args)
        with patch.dict(os.environ, {"ANYGEN_SESSION": "test-session", "ANYGEN_CSRF_TOKEN": "test-csrf"}), \
                patch.object(client.opener, "open") as opened:
            opened.return_value.__enter__.return_value.read.return_value = b'{"code":0,"data":{}}'
            client.gateway("listClawAppActions", {"claw_token": "new"})
            req = opened.call_args.args[0]
            self.assertEqual(req.full_url, "https://www.anygen.io/api/v1/gateway_anygen")
            self.assertEqual(req.get_header("Cookie"), "session=test-session; _csrf_token=test-csrf")
            self.assertIsNone(req.get_header("Authorization"))

    def test_redirect_does_not_forward_credentials(self):
        self.assertIsNone(anygen.NoRedirect().redirect_request(
            None, None, 302, "Found", {}, "https://other.example"))

    def test_key_creation_sends_same_origin_context(self):
        args = anygen.parser().parse_args(["--gateway-auth", "cookie", "inspect", "--claw", "new"])
        client = anygen.Client(args)
        with patch.dict(os.environ, {"ANYGEN_SESSION": "test-session", "ANYGEN_CSRF_TOKEN": "test-csrf"}), \
                patch.object(client.opener, "open") as opened:
            opened.return_value.__enter__.return_value.read.return_value = b'{"success":true,"data":{}}'
            client.key_management("POST", {"name": "test"})
            req = opened.call_args.args[0]
            self.assertEqual(req.get_header("Origin"), anygen.ORIGIN)
            self.assertEqual(req.get_header("Referer"), anygen.ORIGIN + "/")
            self.assertIsNone(req.get_header("Authorization"))

    def test_http_error_hides_response_and_does_not_retry(self):
        args = anygen.parser().parse_args(["check", "--ref", "app-new"])
        client = anygen.Client(args)
        headers = Message()
        headers["x-tt-logid"] = "log-123"
        error = urllib.error.HTTPError("https://www.anygen.io", 403, "private", headers,
                                       io.BytesIO(b'private response'))
        with patch.dict(os.environ, {"ANYGEN_LLM_KEY": "test-key"}), \
                patch.object(client.opener, "open", side_effect=error) as opened, \
                self.assertRaisesRegex(anygen.Failure, "HTTP 403; x-tt-logid=log-123") as result:
            client.request("GET", "/v1/openapi/key/verify")
        self.assertNotIn("private", str(result.exception))
        self.assertEqual(opened.call_count, 1)

    def test_grant_write_is_verified_by_read_back(self):
        old = {"api_key_id": "key-new", "whole_app": False, "actions": []}
        new = dict(old, actions=anygen.ACTIONS)
        result, request = self.run_cli("grant", "--claw", "new-claw", "--key-id", "key-new",
                                       "--apply", responses=[
                                           {"actions": [{"action": a} for a in anygen.ACTIONS]},
                                           {"grants": [old]}, {"grant": new}, {"grants": [new]}])
        self.assertEqual(result["status"], "applied")
        self.assertEqual(request.call_count, 4)


if __name__ == "__main__":
    unittest.main()
