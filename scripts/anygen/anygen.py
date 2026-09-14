#!/usr/bin/env python3
"""Inspect, authorize and verify an AnyGen LLM App. Python 3.9+, stdlib only."""

import argparse
import http.client
import json
import math
import os
import re
import sys
import time
import urllib.error
import urllib.request

ORIGIN = "https://www.anygen.io"
ACTIONS = ["POST /api/v1/chat/completions", "GET /api/v1/models"]


class Failure(Exception):
    """A safe-to-display error; never includes upstream bodies or credentials."""


def app_path(ref, slot):
    if not re.fullmatch(r"[A-Za-z0-9_-]+", ref):
        raise Failure("invalid publication ref (use the ref, not a URL or claw token)")
    if slot and (not re.fullmatch(r"[a-z][a-z0-9-]{0,31}", slot)
                 or slot in {"api", "static", "assets", "app", "apps", "default"}):
        raise Failure("invalid or reserved App slot")
    return "/v1/openapi/anyclaw/app/" + ref + ("/" + slot if slot else "") + "/api/v1"


def decode_response(body, gateway=False):
    try:
        data = json.loads(body)
    except (ValueError, UnicodeError):
        raise Failure("expected JSON, received a non-JSON response") from None
    if not isinstance(data, dict):
        raise Failure("expected a JSON object")
    if gateway:
        code = data.get("code")
        if type(code) is not int or code != 0:
            # Do not echo msg: framework errors can contain request credentials.
            label = str(code) if type(code) is int else "missing/invalid"
            raise Failure("gateway business error code=" + label +
                          "; check owner login, permissions and deployed gateway support")
        data = data.get("data")
        if not isinstance(data, dict):
            raise Failure("gateway response data must be an object")
    elif "error" in data or ("code" in data and data["code"] != 0):
        raise Failure("upstream returned an error envelope, not a model API result")
    return data


def env_secret(name):
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name):
        raise Failure("invalid credential environment-variable name")
    value = os.environ.get(name, "").strip()
    if not value or any(ord(c) < 33 or ord(c) > 126 for c in value):
        raise Failure("missing or invalid credential in environment variable " + name)
    return value


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None  # Never forward Authorization/Cookie to a redirect target.


class Client:
    def __init__(self, args):
        self.args = args
        self.runtime_key = None
        self.opener = urllib.request.build_opener(NoRedirect())

    def request(self, method, path, body=None, gateway=False, cookie=False, key_override=None):
        headers = {"Accept": "application/json"}
        if cookie or (gateway and self.args.gateway_auth == "cookie"):
            session = env_secret("ANYGEN_SESSION")
            csrf = env_secret("ANYGEN_CSRF_TOKEN")
            if ";" in session or ";" in csrf:
                raise Failure("provide individual cookie values, not a copied Cookie header")
            headers["Cookie"] = "session=" + session + "; _csrf_token=" + csrf
            headers["x-csrftoken"] = csrf
            # Owner-session writes use the same origin context as the web client.
            # Keep it off Bearer model requests and never follow redirects.
            headers["Origin"] = ORIGIN
            headers["Referer"] = ORIGIN + "/"
        else:
            key_env = (self.args.gateway_key_env or self.args.key_env) if gateway else self.args.key_env
            key = key_override or (self.runtime_key if not gateway else None) or env_secret(key_env)
            headers["Authorization"] = "Bearer " + key
        payload = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            payload = json.dumps(body).encode()
        req = urllib.request.Request(ORIGIN + path, data=payload, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=self.args.timeout) as response:
                raw = response.read(4 * 1024 * 1024 + 1)
                if len(raw) > 4 * 1024 * 1024:
                    raise Failure("response exceeds this diagnostic tool's 4 MiB budget")
        except urllib.error.HTTPError as error:
            log_id = error.headers.get("x-tt-logid", "")
            log_id = log_id if re.fullmatch(r"[A-Za-z0-9_-]{1,128}", log_id) else "unavailable"
            status = error.code
            error.close()
            raise Failure("HTTP %s; x-tt-logid=%s; no automatic retry" % (status, log_id)) from None
        except (OSError, urllib.error.URLError, http.client.HTTPException):
            raise Failure("network/TLS/timeout failure; outcome may be unknown; no automatic retry") from None
        return decode_response(raw, gateway)

    def gateway(self, method, body):
        path = "/v1/openapi/gateway" if self.args.gateway_auth == "bearer" else "/api/v1/gateway_anygen"
        return self.request("POST", path, {"method": method, "body": body}, gateway=True)

    def key_management(self, method, body=None):
        if self.args.gateway_auth != "cookie":
            raise Failure("key management requires explicit owner cookie authentication")
        result = self.request(method, "/api/page/openapi/keys", body, cookie=True)
        if "code" in result:
            result = decode_response(json.dumps(result), gateway=True)
        if result.get("success") is not True or not isinstance(result.get("data"), dict):
            raise Failure("key management request was not successful; response body omitted")
        return result["data"]


def target(args):
    if not re.fullmatch(r"[A-Za-z0-9_-]+", args.claw):
        raise Failure("invalid claw token")
    app_path("validation-only", args.slot)
    body = {"claw_token": args.claw}
    if args.slot:
        body["app"] = args.slot
    return body


def action_names(response):
    items = response.get("actions")
    if not isinstance(items, list) or any(
            not isinstance(item, dict) or not isinstance(item.get("action"), str) for item in items):
        raise Failure("invalid actions response")
    return sorted({item["action"] for item in items})


def find_grant(response, key_id):
    items = response.get("grants")
    if not isinstance(items, list):
        raise Failure("invalid grants response")
    matches = [item for item in items if isinstance(item, dict) and item.get("api_key_id") == key_id]
    if len(matches) != 1:
        raise Failure("key ID not uniquely found in owner's user-created keys")
    grant = matches[0]
    if not isinstance(grant.get("actions"), list) or type(grant.get("whole_app")) is not bool:
        raise Failure("invalid grant shape")
    if any(not isinstance(action, str) for action in grant["actions"]):
        raise Failure("invalid grant actions")
    return grant


def exact_grant(grant):
    return (isinstance(grant, dict) and grant.get("whole_app") is False
            and isinstance(grant.get("actions"), list)
            and all(isinstance(action, str) for action in grant["actions"])
            and sorted(grant["actions"]) == sorted(ACTIONS))


def grant_key(client, args):
    body = dict(target(args), api_key_id=args.key_id, whole_app=False, actions=ACTIONS)
    if not re.fullmatch(r"[A-Za-z0-9_-]+", args.key_id):
        raise Failure("invalid API key ID (not the secret sk-ag key)")
    if not args.apply:
        return {"status": "preview", "method": "setClawAppKeyGrant", "body": body}
    actions = action_names(client.gateway("listClawAppActions", target(args)))
    if not set(ACTIONS).issubset(actions):
        raise Failure("required actions not deployed; publish both routes before granting")
    before = find_grant(client.gateway("listClawAppKeyGrants", target(args)), args.key_id)
    if exact_grant(before):
        return {"status": "unchanged", "api_key_id": args.key_id, "actions": ACTIONS}
    if (before["whole_app"] or before["actions"]) and not args.replace_existing:
        raise Failure("existing grant differs; inspect it first, then explicitly use --replace-existing")
    result = client.gateway("setClawAppKeyGrant", body).get("grant", {})
    if not exact_grant(result) or result.get("api_key_id") != args.key_id:
        raise Failure("write response differs from requested grant; inspect before retrying")
    after = find_grant(client.gateway("listClawAppKeyGrants", target(args)), args.key_id)
    if not exact_grant(after):
        raise Failure("grant read-back differs after write; inspect before retrying")
    return {"status": "applied", "api_key_id": args.key_id, "whole_app": False, "actions": ACTIONS}


def check(client, args):
    path = app_path(args.ref, args.slot)
    credits = client.request("GET", "/v1/openapi/key/verify")
    if credits.get("verified") is not True:
        raise Failure("key was not verified")
    catalog = client.request("GET", path + "/models")
    items = catalog.get("data")
    if catalog.get("object") != "list" or not isinstance(items, list) or not items:
        raise Failure("models endpoint returned no valid OpenAI model list")
    if any(not isinstance(item, dict) or not isinstance(item.get("id"), str) or not item["id"]
           for item in items):
        raise Failure("invalid model IDs")
    return {"status": "checked", "base_url": ORIGIN + path, "verified": True,
            "credits": credits.get("credits"), "models": sorted({item["id"] for item in items}),
            "chat_tested": False}


def smoke(client, args):
    if not args.allow_charge:
        raise Failure("smoke invokes a model and spends credits; explicitly pass --allow-charge")
    if not args.model.strip() or args.max_tokens < 1:
        raise Failure("model and positive max-tokens are required")
    start = time.monotonic()
    result = client.request("POST", app_path(args.ref, args.slot) + "/chat/completions", {
        "model": args.model, "messages": [{"role": "user", "content": "Reply only: ANYGEN_PROXY_OK"}],
        "max_tokens": args.max_tokens, "stream": False,
    })
    choices = result.get("choices")
    if result.get("object") != "chat.completion" or not isinstance(choices, list):
        raise Failure("not a standard chat.completion response")
    usable = False
    has_text = False
    for choice in choices:
        if not isinstance(choice, dict) or not isinstance(choice.get("message"), dict):
            continue
        msg = choice["message"]
        text = msg.get("content")
        has_text = has_text or (isinstance(text, str) and bool(text.strip()))
        calls = msg.get("tool_calls")
        valid_call = isinstance(calls, list) and any(
            isinstance(call, dict) and isinstance(call.get("function"), dict)
            and isinstance(call["function"].get("name"), str) and call["function"]["name"].strip()
            and isinstance(call["function"].get("arguments"), str)
            for call in calls)
        usable = usable or (isinstance(text, str) and bool(text.strip())) or bool(valid_call)
    if not usable:
        raise Failure("HTTP success without usable assistant output; inspect reasoning/output token budget")
    return {"status": "passed", "model": result.get("model"), "usable_output": True, "has_text": has_text,
            "elapsed_seconds": round(time.monotonic() - start, 3), "usage": result.get("usage"),
            "streaming": False}


def parser():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("--key-env", default="ANYGEN_LLM_KEY", help="runtime key environment-variable name")
    cli.add_argument("--gateway-key-env", help="optional separate owner management key env name")
    cli.add_argument("--gateway-auth", choices=["bearer", "cookie"], default="bearer",
                     help="cookie mode reads ANYGEN_SESSION and ANYGEN_CSRF_TOKEN; never automatic")
    cli.add_argument("--timeout", type=float, default=180, help="socket timeout seconds (not platform deadline)")
    commands = cli.add_subparsers(dest="command", required=True)
    provision = commands.add_parser("provision", help="create a private direct-call AnyGen API")
    provision.add_argument("--name", required=True, help="new private Agent name")
    provision.add_argument("--state-dir", required=True, help="private 0700 directory, parent must exist")
    key_mode = provision.add_mutually_exclusive_group()
    key_mode.add_argument("--create-key", action="store_true", help="create a dedicated Key using owner cookie")
    key_mode.add_argument("--key-id", help="reuse this Key ID with --key-env (no replacement)")
    provision.add_argument("--apply", action="store_true", help="otherwise print an offline plan")
    provision.add_argument("--allow-charge", action="store_true", help="allow one model call after setup")
    provision.add_argument("--model", help="explicit model for paid smoke; must be in the live catalog")
    provision.add_argument("--max-tokens", type=int, default=1024)
    for name in ("inspect", "grant", "check", "smoke", "config"):
        command = commands.add_parser(name)
        command.add_argument("--slot", default="", help="omit for the default App; named slots require platform support")
        if name in ("inspect", "grant"):
            command.add_argument("--claw", required=True)
        else:
            command.add_argument("--ref", required=True, help="publication ref from machine URL, not build/page ref")
        if name == "grant":
            command.add_argument("--key-id", required=True)
            command.add_argument("--apply", action="store_true", help="perform the authorization write")
            command.add_argument("--replace-existing", action="store_true",
                                 help="explicitly replace this App's existing permissions, not other App grants")
        if name in ("smoke", "config"):
            command.add_argument("--model", required=True, help="exact model ID discovered by check")
        if name == "smoke":
            command.add_argument("--allow-charge", action="store_true")
            command.add_argument("--max-tokens", type=int, default=1024)
    return cli


def main(argv=None):
    args = parser().parse_args(argv)
    if not math.isfinite(args.timeout) or args.timeout <= 0:
        raise Failure("timeout must be finite and positive")
    client = Client(args)
    if args.command == "provision":
        from provision import run
        # Pass this module explicitly so direct CLI execution and unittest share
        # the same Failure class, without a duplicate __main__/anygen import.
        result = run(client, args, sys.modules[__name__])
    elif args.command == "grant":
        result = grant_key(client, args)
    elif args.command == "inspect":
        body = target(args)
        status = client.gateway("getClawAppPublishStatus", body)
        actions = action_names(client.gateway("listClawAppActions", body))
        grants = client.gateway("listClawAppKeyGrants", body)
        result = {"publication": status, "actions": actions, "grants": grants.get("grants"),
                  "required_actions_present": set(ACTIONS).issubset(actions)}
    elif args.command == "check":
        result = check(client, args)
    elif args.command == "smoke":
        result = smoke(client, args)
    else:
        path = app_path(args.ref, args.slot)
        # Emit a fragment only: never overwrite a running proxy's configuration.
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", args.key_env):
            raise Failure("invalid key environment-variable name")
        print("anygen:\n  enabled: true\n  base_url: " + json.dumps(ORIGIN + path) +
              "\n  api_key_env: " + json.dumps(args.key_env) + "\nmodels:\n  - name: " +
              json.dumps(args.model) + "\n    providers: [anygen]")
        return
    # Defense in depth against accidental secrets in upstream diagnostics.
    text = json.dumps(result, ensure_ascii=False, indent=2)
    for name in {args.key_env, args.gateway_key_env, "ANYGEN_SESSION", "ANYGEN_CSRF_TOKEN"}:
        secret = os.environ.get(name, "") if name else ""
        if secret:
            text = text.replace(json.dumps(secret, ensure_ascii=False)[1:-1], "<redacted>")
    text = re.sub(r"(?:sk-ag[-_A-Za-z0-9]+|mak_[A-Za-z0-9_]+)", "<redacted>", text)
    print(text)


if __name__ == "__main__":
    try:
        main()
    except Failure as error:
        print(json.dumps({"error": str(error)}), file=sys.stderr)
        sys.exit(1)
