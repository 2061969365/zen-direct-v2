#!/bin/sh
# scripts/sync_assets.sh — inject the asset repo's tool definitions before `go build`.
#
# Why this exists: toolshape.go:17 does `//go:embed tools/opencode_tools.json`, so the
# outbound tool set is frozen into the binary at build time. After zen-assets-runner
# merges a new definition, only a rebuild picks it up; until now that copy step was done
# by hand and was silently forgotten. This script makes it mechanical.
#
# Usage:
#   scripts/sync_assets.sh [--check] [--offline] [--url URL] [--dest PATH]
#
#   (no flags)   download, validate, back up, inject          -> 0 / 2 / 3
#   --check      download + validate, write nothing
#                  0 = remote and local definitions are identical
#                  1 = they differ (local snapshot stale or ahead)
#                  2 = the downloaded asset failed validation
#   --offline    no network at all: only confirm the local file exists and is valid
#                  0 = ok, 2 = missing/invalid
#   --url URL    override the asset URL
#   --dest PATH  override the target file (relative paths resolve from the repo root)
#
# Exit codes: 0 ok, 1 --check mismatch, 2 validation or environment failure (including a
# missing python interpreter), 3 network/download failure. A failed download is never
# papered over with the stale local file: without --offline the script fails, and
# build-linux.sh fails with it.
#
# Shape conversion (verified against both real files, do not guess):
#   asset repo, assets/official_tools.json:
#     {cli_version, captured_at, source:{workflow,run_url,sha256}, order:[...], tools:{...}}
#     each tool: {description, parameters, strict}
#   gateway repo, tools/opencode_tools.json:
#     a bare map — exactly what `json.Unmarshal` into
#     map[string]opencodeToolDefinition expects — of
#     {tool name: {description, parameters}}   (13 entries, no wrapper keys at all)
#   So the injection writes the flat map only, in the asset's `order` sequence, using the
#   per-tool key set already present in the destination file. `strict` is dropped: the Go
#   struct has no such field and the outbound builder hardcodes "strict": false
#   (toolshape.go:224), so carrying it would only add bytes that never reach the wire.
#
# Contract sha256 (must stay byte-identical to the asset repo's rule):
#   json.dumps(tools, sort_keys=True, separators=(",", ":"))   # ensure_ascii default True
#   .encode("utf-8") -> hashlib.sha256().hexdigest()
# Reference implementation: zen-assets-runner/tools/validate_assets.py:canonical_tools_sha256
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$HERE/.." && pwd)

DEFAULT_URL='https://raw.githubusercontent.com/2061969365/zen-assets-runner/main/assets/official_tools.json'
DEFAULT_DEST="$REPO_ROOT/tools/opencode_tools.json"

say() {
  echo "[sync-assets] $*"
}

usage() {
  say "usage: scripts/sync_assets.sh [--check] [--offline] [--url URL] [--dest PATH]"
  say "  --check     download and validate, write nothing (0 match / 1 mismatch / 2 invalid)"
  say "  --offline   no network; only check the local file exists and is valid"
  say "  --url URL   asset URL (default: the zen-assets-runner main branch copy)"
  say "  --dest PATH target file (default: <repo>/tools/opencode_tools.json)"
}

MODE=write
URL=$DEFAULT_URL
DEST=$DEFAULT_DEST

while [ $# -gt 0 ]; do
  case "$1" in
    --check)
      MODE=check
      ;;
    --offline)
      MODE=offline
      ;;
    --url)
      [ $# -ge 2 ] || { say "error: --url needs a value"; exit 2; }
      URL=$2
      shift
      ;;
    --url=*)
      URL=${1#--url=}
      ;;
    --dest)
      [ $# -ge 2 ] || { say "error: --dest needs a value"; exit 2; }
      DEST=$2
      shift
      ;;
    --dest=*)
      DEST=${1#--dest=}
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      say "error: unknown argument: $1"
      usage
      exit 2
      ;;
  esac
  shift
done

# --offline wins if both were given: it is the only flag that promises no network.
# Absolute paths pass through untouched; a Windows drive-letter path (F:/..., F:\...)
# counts as absolute too, otherwise `--dest F:/x` silently becomes <repo>/F:/x.
case "$DEST" in
  /*) ;;
  [A-Za-z]:[\\/]*) ;;
  *) DEST="$REPO_ROOT/$DEST" ;;
esac

# python is mandatory: the canonical serialisation must not be re-implemented in shell or
# in PowerShell, where key ordering and escaping drift from the contract.
# Candidates are probed by actually running them: on Windows `python3` is often the
# Microsoft Store alias stub, which is on PATH but exits without doing anything.
pick_python() {
  for candidate in ${PYTHON:-} python3 python; do
    [ -n "$candidate" ] || continue
    if command -v "$candidate" >/dev/null 2>&1 &&
      "$candidate" -c 'import sys; sys.exit(0 if sys.version_info[0] == 3 else 1)' >/dev/null 2>&1; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

if ! PY=$(pick_python); then
  say "error: no working python3/python found; the canonical sha256 contract needs python"
  say "error: set PYTHON=/path/to/python to point at one explicitly"
  exit 2
fi
say "python: $("$PY" -c 'import sys;print(sys.executable)')"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/sync-assets.XXXXXX") || {
  say "error: cannot create a temp directory"
  exit 2
}
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

cat >"$TMP/helper.py" <<'PY'
"""Validate an asset file and project it into the gateway tool map.

Invoked only by scripts/sync_assets.sh. The same program is embedded in
scripts/sync_assets.ps1 (PowerShell calls python for the contract sha256 instead of
re-implementing canonical JSON); keep the two copies in step.

Modes:
  fetch <url> <out-path>                          download (urllib, 2 attempts)
  asset <remote.json> <dest-path> <emit-path|->   validate + project (+ render)
  local <dest-path>                               validate the gateway-shaped file

Exit: 0 ok, 1 --check mismatch, 2 validation/environment failure, 3 download failure.
"""
import hashlib
import json
import os
import re
import sys

PREFIX = "[sync-assets]"
SHA_RE = re.compile(r"\A[0-9a-f]{64}\Z")


def say(line):
    sys.stdout.write(PREFIX + " " + line + "\n")
    sys.stdout.flush()


def die(line, code=2):
    sys.stdout.write(PREFIX + " " + line + "\n")
    sys.stdout.flush()
    raise SystemExit(code)


def canonical_tools_sha256(tools):
    # Contract, copied literally: sort_keys + compact separators, ensure_ascii default.
    payload = json.dumps(tools, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def read_json(path):
    try:
        with open(path, "rb") as handle:
            raw = handle.read()
    except OSError as exc:
        die("validate: FAIL cannot read %s: %s" % (path, exc))
    try:
        return json.loads(raw.decode("utf-8"))
    except UnicodeDecodeError as exc:
        die("validate: FAIL %s is not valid UTF-8: %s" % (path, exc))
    except json.JSONDecodeError as exc:
        die("validate: FAIL %s is not valid JSON: %s" % (path, exc))


def gateway_tool_keys(dest):
    """Per-tool key order used by the destination file's current shape."""
    observed = []
    if dest and os.path.isfile(dest):
        try:
            with open(dest, "rb") as handle:
                data = json.loads(handle.read().decode("utf-8"))
        except Exception:
            data = None
        if isinstance(data, dict):
            for value in data.values():
                if isinstance(value, dict) and value:
                    observed = list(value.keys())
                    break
    ordered = ["description", "parameters"]
    for key in observed:
        if key not in ordered:
            ordered.append(key)
    return ordered


def project(order, tools, keys):
    """Asset {order, tools} -> gateway flat map, in asset order."""
    ordered_names = list(order)
    leftovers = sorted(name for name in tools if name not in set(ordered_names))
    extra_fields = set()
    result = {}
    for name in ordered_names + leftovers:
        source = tools[name]
        entry = {}
        for key in keys:
            if key in source:
                entry[key] = source[key]
            else:
                extra_fields.add("(gateway-only) " + key)
        for key in source:
            if key not in keys:
                extra_fields.add(key)
        result[name] = entry
    return result, leftovers, sorted(extra_fields)


def render(value, indent=0):
    """Gateway JSON style: objects expanded with 2 spaces, arrays kept on one line.

    This reproduces tools/opencode_tools.json byte-for-byte today, so a sync that only
    changes definitions produces a content-only diff instead of a full reformat.
    """
    pad = "  " * indent
    if isinstance(value, dict):
        if not value:
            return "{}"
        body = ",\n".join(
            '%s  %s: %s' % (pad, json.dumps(key, ensure_ascii=False), render(item, indent + 1))
            for key, item in value.items()
        )
        return "{\n" + body + "\n" + pad + "}"
    if isinstance(value, list):
        if not value:
            return "[]"
        return "[" + ", ".join(render(item, indent + 1) for item in value) + "]"
    return json.dumps(value, ensure_ascii=False)


def validate_asset(data):
    if not isinstance(data, dict):
        die("validate: FAIL asset root is not a JSON object")
    order = data.get("order")
    if not isinstance(order, list) or not order:
        die("validate: FAIL asset 'order' must be a non-empty array")
    seen = set()
    for index, name in enumerate(order):
        if not isinstance(name, str) or not name:
            die("validate: FAIL order[%d] is not a non-empty string" % index)
        if name in seen:
            die("validate: FAIL order contains duplicate tool %r" % name)
        seen.add(name)
    tools = data.get("tools")
    if not isinstance(tools, dict) or not tools:
        die("validate: FAIL asset 'tools' must be a non-empty object")
    for name in order:
        if name not in tools:
            die("validate: FAIL order lists %r but tools has no such entry" % name)
        tool = tools[name]
        if not isinstance(tool, dict):
            die("validate: FAIL tools.%s is not an object" % name)
        if not isinstance(tool.get("description"), str):
            die("validate: FAIL tools.%s.description is not a string" % name)
        if not isinstance(tool.get("parameters"), dict):
            die("validate: FAIL tools.%s.parameters is not an object" % name)
    source = data.get("source")
    if not isinstance(source, dict):
        die("validate: FAIL asset 'source' is not an object")
    declared = source.get("sha256")
    if not isinstance(declared, str) or not SHA_RE.match(declared.strip().lower()):
        die("validate: FAIL source.sha256 must be 64 hex characters")
    return order, tools, declared.strip().lower()


def mode_fetch(url, out):
    """Download the asset file.

    urllib rather than curl/wget: python is already required for the canonical sha256, and
    a single TLS stack removes the Windows schannel revocation-check failures that make
    curl die with CRYPT_E_REVOCATION_OFFLINE on otherwise healthy networks.
    """
    import time
    import urllib.request

    attempts = 2
    for attempt in range(1, attempts + 1):
        try:
            with urllib.request.urlopen(url, timeout=20) as response:
                body = response.read()
                status = getattr(response, "status", None)
        except Exception as exc:  # URLError, HTTPError, timeout, DNS, TLS
            say("fetch: attempt %d/%d failed: %s: %s"
                % (attempt, attempts, type(exc).__name__, exc))
            if attempt < attempts:
                time.sleep(1)
            continue
        try:
            with open(out, "wb") as handle:
                handle.write(body)
        except OSError as exc:
            die("fetch: FAIL cannot write %s: %s" % (out, exc))
        if status is None:
            say("fetch: %d bytes" % len(body))
        else:
            say("fetch: http %s, %d bytes" % (status, len(body)))
        return 0
    say("error: download failed: %s" % url)
    return 3


def mode_asset(remote, dest, emit):
    order, tools, declared = validate_asset(read_json(remote))
    say("validate: structure ok order=%d tools=%d" % (len(order), len(tools)))
    recomputed = canonical_tools_sha256(tools)
    say("%-40s = %s" % ("remote sha256 (source.sha256 declared)", declared))
    say("%-40s = %s" % ("remote sha256 (recomputed canonical)", recomputed))
    if declared != recomputed:
        die("validate: FAIL source.sha256 mismatch (declared %s != recomputed %s)"
            % (declared, recomputed))
    say("validate: sha256 matches recomputed canonical tools")

    keys = gateway_tool_keys(dest)
    projected, leftovers, extra_fields = project(order, tools, keys)
    projected_sha = canonical_tools_sha256(projected)
    say("%-40s = %s" % ("remote sha256 (gateway projection)", projected_sha))
    say("project: %d tool(s) in order, tool keys kept: %s"
        % (len(projected), ",".join(keys)))
    if leftovers:
        say("project: %d tool(s) absent from order, appended at the end: %s"
            % (len(leftovers), ",".join(leftovers)))
    if extra_fields:
        say("project: asset fields not carried by the gateway shape: %s"
            % ",".join(extra_fields))

    verdict = "MISMATCH"
    if os.path.isfile(dest):
        local = read_json(dest)
        if not isinstance(local, dict):
            die("validate: FAIL local %s is not a JSON object" % dest)
        local_sha = canonical_tools_sha256(local)
        say("%-40s = %s" % ("local sha256 (canonical, gateway format)", local_sha))
        verdict = "MATCH" if local_sha == projected_sha else "MISMATCH"
    else:
        say("%-40s = %s" % ("local sha256 (canonical, gateway format)", "<missing: %s>" % dest))
    say("check: %s" % verdict)

    if emit != "-":
        payload = (render(projected) + "\n").encode("utf-8")
        try:
            with open(emit, "wb") as handle:
                handle.write(payload)
        except OSError as exc:
            die("render: FAIL cannot write %s: %s" % (emit, exc))
        say("render: %d bytes -> %s" % (len(payload), emit))
        return 0
    return 0 if verdict == "MATCH" else 1


def mode_local(dest):
    if not os.path.isfile(dest):
        die("offline: FAIL %s does not exist" % dest)
    data = read_json(dest)
    if not isinstance(data, dict) or not data:
        die("offline: FAIL %s is not a non-empty JSON object" % dest)
    for name, tool in data.items():
        if not isinstance(tool, dict):
            die("offline: FAIL tool %r is not an object" % name)
        if not isinstance(tool.get("description"), str):
            die("offline: FAIL %s.description is not a string" % name)
        if "parameters" in tool and not isinstance(tool["parameters"], dict):
            die("offline: FAIL %s.parameters is not an object" % name)
    say("offline: structure ok tools=%d" % len(data))
    say("%-40s = %s" % ("local sha256 (canonical, gateway format)", canonical_tools_sha256(data)))
    return 0


def main(argv):
    if len(argv) < 3:
        die("error: helper invoked with wrong arguments")
    mode = argv[1]
    if mode == "fetch":
        if len(argv) != 4:
            die("error: fetch mode needs <url> <out>")
        return mode_fetch(argv[2], argv[3])
    if mode == "asset":
        if len(argv) != 5:
            die("error: asset mode needs <remote> <dest> <emit|->")
        return mode_asset(argv[2], argv[3], argv[4])
    if mode == "local":
        return mode_local(argv[2])
    die("error: unknown helper mode %r" % mode)


raise SystemExit(main(sys.argv))
PY

if [ "$MODE" = offline ]; then
  say "offline: no network, checking the local file only"
  say "dest: $DEST"
  "$PY" "$TMP/helper.py" local "$DEST"
  say "done: local snapshot is usable"
  exit 0
fi

say "source url: $URL"
say "dest file:  $DEST"

set +e
"$PY" "$TMP/helper.py" fetch "$URL" "$TMP/remote.json"
STATUS=$?
set -e
if [ "$STATUS" -ne 0 ]; then
  say "error: could not download the asset file"
  say "error: not falling back to the stale local file; use --offline to use it on purpose"
  exit 3
fi
say "fetch: ok $(wc -c <"$TMP/remote.json" | tr -d ' \t') bytes on disk"

if [ "$MODE" = check ]; then
  say "check: validating the downloaded asset, nothing will be written"
  set +e
  "$PY" "$TMP/helper.py" asset "$TMP/remote.json" "$DEST" -
  STATUS=$?
  set -e
  exit "$STATUS"
fi

say "write: validating and projecting the downloaded asset"
set +e
"$PY" "$TMP/helper.py" asset "$TMP/remote.json" "$DEST" "$TMP/injected.json"
STATUS=$?
set -e
[ "$STATUS" -eq 0 ] || exit "$STATUS"

same_bytes() {
  "$PY" -c 'import filecmp,sys; sys.exit(0 if filecmp.cmp(sys.argv[1], sys.argv[2], shallow=False) else 1)' "$1" "$2"
}

if [ -f "$DEST" ]; then
  set +e
  same_bytes "$TMP/injected.json" "$DEST"
  SAME=$?
  set -e
  if [ "$SAME" -eq 0 ]; then
    say "write: skipped, $DEST is already byte-identical to the asset projection"
    say "done"
    exit 0
  fi
  if [ -f "$DEST.orig" ]; then
    say "backup: $DEST.orig already exists, left untouched"
  else
    cp -p "$DEST" "$DEST.orig"
    say "backup: $DEST -> $DEST.orig"
  fi
else
  say "backup: skipped, no existing $DEST"
fi

mkdir -p "$(dirname -- "$DEST")"
STAGE="$DEST.sync-stage.$$"
cp "$TMP/injected.json" "$STAGE"
mv -f "$STAGE" "$DEST"
say "write: $DEST updated ($(wc -c <"$DEST" | tr -d ' \t') bytes)"
say "done"
