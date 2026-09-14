"""Deterministic creation of one private default-slot AnyGen model API."""

import base64
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile
from types import SimpleNamespace
from urllib.parse import urlsplit

from provision_state import Journal, StateError

TEMPLATE = Path(__file__).resolve().parents[2] / "examples/anygen/app"
TEMPLATE_FILES = ("manifest.json", "package.json", "src/App.tsx", "api/lambda/llm.ts")


def app_bundle():
    stream = io.BytesIO()
    # Explicit allowlist: never upload .env, local state, symlinks or node_modules.
    with tarfile.open(fileobj=stream, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name in sorted(TEMPLATE_FILES):
            path = TEMPLATE / name
            if path.is_symlink() or not path.is_file():
                raise StateError("template is missing a regular source file")
            body = path.read_bytes()
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode, entry.mtime = len(body), 0o644, 0
            archive.addfile(entry, io.BytesIO(body))
    blob = gzip.compress(stream.getvalue(), mtime=0)
    return blob, hashlib.sha256(blob).hexdigest()


def validate_id(value, label, api):
    if not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", value):
        raise api.Failure("invalid " + label + " in platform response")
    return value


def publication_ref(url, api, hook=False):
    parsed = urlsplit(url) if isinstance(url, str) else None
    tail = r"/-/hook/mak_[A-Za-z0-9_]+" if hook else ""
    match = re.fullmatch(r"/v1/openapi/anyclaw/app/([A-Za-z0-9_-]+)" + tail,
                         parsed.path if parsed else "")
    if (not parsed or parsed.scheme != "https" or parsed.netloc != "www.anygen.io"
            or parsed.query or parsed.fragment or not match):
        raise api.Failure("unexpected publication URL; secret URL omitted")
    return match[1]


def options(args, **values):
    return SimpleNamespace(**(vars(args) | {"slot": "", "replace_existing": False} | values))


def run(client, args, api):
    if not args.name.strip() or len(args.name) > 64:
        raise api.Failure("name must contain 1–64 characters")
    if args.allow_charge and not args.model:
        raise api.Failure("--allow-charge requires an explicit --model")
    if args.max_tokens < 1:
        raise api.Failure("max-tokens must be positive")
    try:
        blob, fingerprint = app_bundle()
    except (OSError, StateError):
        raise api.Failure("cannot read the complete regular-file App template") from None
    plan = {
        "status": "plan", "origin": api.ORIGIN, "name": args.name,
        "state_dir": str(Path(args.state_dir).absolute()), "bundle_sha256": fingerprint,
        "key_mode": "create" if args.create_key else "reuse",
        "steps": ["preflight", "createClaw", "deployClawApp", "listClawAppActions",
                  "createClawAppHook", "deleteClawAppHook",
                  "createKey" if args.create_key else "reuseKey", "setClawAppKeyGrant",
                  "verifyKey", "models"] + (["one_chat_request"] if args.allow_charge else []),
        "smoke": {"enabled": args.allow_charge, "model": args.model,
                  "max_tokens": args.max_tokens, "automatic_retries": 0},
        "llm_proxy_changes": False,
    }
    if not args.apply:
        return plan
    if args.create_key and args.gateway_auth != "cookie":
        raise api.Failure("--create-key requires --gateway-auth cookie")
    if not args.create_key and not args.key_id:
        raise api.Failure("reuse mode requires --key-id (or choose --create-key)")
    if args.key_id:
        validate_id(args.key_id, "requested key ID", api)
    # Validate auth locally before creating the state directory or any resources.
    auth = api.env_secret("ANYGEN_SESSION") if args.gateway_auth == "cookie" else api.env_secret(
        args.gateway_key_env or args.key_env)
    if args.gateway_auth == "cookie":
        api.env_secret("ANYGEN_CSRF_TOKEN")
    runtime_key = None if args.create_key else api.env_secret(args.key_env)
    identity = {"name": args.name, "origin": api.ORIGIN, "bundle_sha256": fingerprint,
                "key_id": args.key_id, "create_key": args.create_key,
                "management_auth_sha256": hashlib.sha256(auth.encode()).hexdigest()}
    if runtime_key:
        identity["runtime_key_sha256"] = hashlib.sha256(runtime_key.encode()).hexdigest()
    try:
        with Journal(args.state_dir) as journal:
            state = journal.data
            if state and state.get("identity") != identity:
                raise api.Failure("state belongs to different inputs, auth or template; not resumed")
            if state.get("pending"):
                raise api.Failure("previous write outcome unknown: " + state["pending"] +
                                  "; inspect recorded resource IDs; not retried")
            if state.get("smoke") and args.allow_charge and (
                    state["smoke"].get("requested_model") != args.model
                    or state["smoke"].get("max_tokens") != args.max_tokens):
                raise api.Failure("this run already tested a different model/budget; no new charge attempted")
            if not state:
                state.update({"schema_version": 1, "identity": identity})
                journal.save()
            return provision(client, args, api, journal, blob, runtime_key)
    except StateError as error:
        raise api.Failure(str(error)) from None
    except OSError:
        raise api.Failure("private state write failed; no retry; inspect last recorded pending operation") from None


def provision(client, args, api, journal, blob, runtime_key):
    state = journal.data
    if args.create_key:
        # Key management is an owner-session endpoint; never send sk-ag here.
        client.key_management("GET")
    else:
        verified = client.request("GET", "/v1/openapi/key/verify")
        if verified.get("verified") is not True:
            raise api.Failure("runtime key not verified")
        if args.gateway_auth == "bearer" and args.gateway_key_env:
            owner = client.request("GET", "/v1/openapi/key/verify",
                                   key_override=api.env_secret(args.gateway_key_env))
            if owner.get("verified") is not True or owner.get("user_id") != verified.get("user_id"):
                raise api.Failure("management and runtime keys must belong to the same owner")
        if args.gateway_auth == "cookie":
            keys = client.key_management("GET").get("keys", [])
            if not isinstance(keys, list) or any(not isinstance(item, dict) for item in keys):
                raise api.Failure("invalid owner key list")
            matches = [item for item in keys if item.get("api_key_id") == args.key_id]
            if len(matches) != 1:
                raise api.Failure("runtime key ID is not in the current owner session")
            if matches[0].get("api_key_masked") != runtime_key[:3] + "****" + runtime_key[-5:]:
                raise api.Failure("runtime key mask mismatch before creating resources")

    if "claw" not in state:
        journal.begin("create_claw")
        created = client.gateway("createClaw", {"init_config": {
            "title": args.name, "description": "Private LLM proxy",
        }})
        claw = validate_id(created.get("claw_token"), "claw token", api)
        journal.finish("claw", claw)  # discard access_token/default_chat entirely
    claw = state["claw"]
    target = {"claw_token": claw}
    if "deployed" not in state:
        files = client.gateway("listClawAppFiles", target).get("files")
        if not isinstance(files, list) or files:
            raise api.Failure("new App slot is not empty; refusing to overwrite")
        journal.begin("deploy")
        deployed = client.gateway("deployClawApp", dict(
            target, bundle=base64.b64encode(blob).decode()))
        if deployed.get("ok") is not True or deployed.get("diagnostics"):
            # Failure may already have written live source; pending intentionally stays.
            raise api.Failure("deployment not clean; inspect build diagnostics in the platform; not retried")
        journal.finish("deployed", {"version_id": deployed.get("version_id")})
    actions = api.action_names(client.gateway("listClawAppActions", target))
    if actions != sorted(api.ACTIONS):
        raise api.Failure("deployed action table does not match the two-action template")

    if "publication" not in state:
        status = client.gateway("getClawAppPublishStatus", target)
        if status.get("published") is True:
            journal.finish("publication", publication_ref(status.get("url"), api))
        else:
            hooks = client.gateway("listClawAppHooks", target).get("hooks")
            if not isinstance(hooks, list) or hooks:
                raise api.Failure("unexpected existing hooks; inspect publication before creating")
            journal.begin("create_hook")
            result = client.gateway("createClawAppHook", dict(
                target, name="llm-proxy-bootstrap", action=api.ACTIONS[0]))
            hook_id = validate_id(result.get("hook", {}).get("id"), "hook ID", api)
            # Persist ID before parsing the secret URL so cleanup remains possible.
            state["hook_id"] = hook_id
            journal.save()
            ref = publication_ref(result.get("url"), api, hook=True)
            journal.finish("publication", ref)
    if state.get("hook_id") and not state.get("hook_deleted"):
        journal.begin("delete_hook")
        client.gateway("deleteClawAppHook", dict(target, id=state["hook_id"]))
        remaining = client.gateway("listClawAppHooks", target).get("hooks")
        if not isinstance(remaining, list) or any(h.get("id") == state["hook_id"] for h in remaining):
            raise api.Failure("temporary hook deletion not verified")
        journal.finish("hook_deleted", True)
    status = client.gateway("getClawAppPublishStatus", target)
    if status.get("published") is not True or publication_ref(status.get("url"), api) != state["publication"]:
        raise api.Failure("publication is not enabled for this App; no implicit publish/re-enable")

    credentials = journal.read("credentials.json")
    if credentials is not None and (not isinstance(credentials, dict)
            or not isinstance(credentials.get("api_key"), str)
            or not re.fullmatch(r"sk-ag[-_A-Za-z0-9]+", credentials["api_key"])
            or not isinstance(credentials.get("api_key_id"), str)):
        raise api.Failure("invalid private credentials file")
    if args.create_key:
        if state.get("key_id") and (not credentials or credentials["api_key_id"] != state["key_id"]):
            raise api.Failure("created key secret is missing/mismatched; do not create another key")
        if not credentials:
            journal.begin("create_key")
            result = client.key_management("POST", {"name": args.name + " API",
                "features": ["app:" + claw + ":" + action for action in api.ACTIONS]})
            key_id = validate_id(result.get("api_key_id"), "API key ID", api)
            state["created_key_id"] = key_id
            journal.save()
            runtime_key = result.get("api_key")
            if not isinstance(runtime_key, str) or not re.fullmatch(r"sk-ag[-_A-Za-z0-9]+", runtime_key):
                raise api.Failure("key creation did not return a valid secret; inspect Key ID before recovery")
            credentials = {"api_key": runtime_key, "api_key_id": key_id}
            journal.write("credentials.json", credentials)
            journal.finish("key_id", key_id)
        runtime_key, key_id = credentials["api_key"], credentials["api_key_id"]
    else:
        key_id = args.key_id
        if credentials and credentials.get("api_key") != runtime_key:
            raise api.Failure("stored runtime key differs from supplied key")
        journal.write("credentials.json", {"api_key": runtime_key, "api_key_id": key_id})

    before = api.find_grant(client.gateway("listClawAppKeyGrants", target), key_id)
    expected_mask = runtime_key[:3] + "****" + runtime_key[-5:]
    if before.get("api_key_masked") != expected_mask:
        raise api.Failure("runtime secret does not match the selected platform Key mask")
    if not api.exact_grant(before):
        if before["whole_app"] or before["actions"]:
            raise api.Failure("unexpected existing grant on new App; not overwritten")
        journal.begin("grant")
        result = client.gateway("setClawAppKeyGrant", dict(
            target, api_key_id=key_id, whole_app=False, actions=api.ACTIONS)).get("grant")
        if not api.exact_grant(result) or result.get("api_key_id") != key_id:
            raise api.Failure("grant write response not verified")
        after = api.find_grant(client.gateway("listClawAppKeyGrants", target), key_id)
        if not api.exact_grant(after):
            raise api.Failure("grant read-back differs")
        journal.finish("grant", True)
    direct = api.Client(args)
    direct.runtime_key = runtime_key
    checked = api.check(direct, options(args, ref=state["publication"]))
    credentials = {"base_url": checked["base_url"], "api_key": runtime_key, "api_key_id": key_id}
    journal.write("credentials.json", credentials)
    state["ready"] = {"base_url": checked["base_url"], "models": checked["models"]}
    journal.save()
    if args.allow_charge and "smoke" not in state:
        if args.model not in checked["models"]:
            raise api.Failure("requested smoke model is not in the current catalog; no charge attempted")
        journal.begin("smoke")
        result = api.smoke(direct, options(args, ref=state["publication"]))
        if not result.get("has_text"):
            raise api.Failure("direct smoke must return non-empty text")
        result.update(requested_model=args.model, max_tokens=args.max_tokens)
        journal.finish("smoke", result)
    report = {"status": "verified" if "smoke" in state else "ready_unverified",
              "base_url": checked["base_url"], "api_key_id": key_id, "claw_token": claw,
              "credentials_file": str(journal.directory / "credentials.json"),
              "models": checked["models"], "smoke": state.get("smoke"),
              "llm_proxy_changes": False}
    journal.write("result.json", report)
    return report
