#Requires -Version 5.1
<#
scripts/sync_assets.ps1 - Windows twin of scripts/sync_assets.sh.

Same semantics, same exit codes, same one-line "[sync-assets] ..." output.
Kept as a separate file because a Windows host may not have bash at all. The canonical
sha256 is delegated to python in both versions on purpose: re-implementing canonical JSON
(sorted keys, compact separators, ensure_ascii, UTF-8 bytes) in PowerShell drifts from the
contract the asset repo verifies.

Usage:
  powershell -ExecutionPolicy Bypass -File scripts\sync_assets.ps1 [-Check] [-Offline] [-Url URL] [-Dest PATH]

  (no switch)  download, validate, back up, inject           -> 0 / 2 / 3
  -Check       download + validate, write nothing
                 0 = remote and local definitions are identical
                 1 = they differ (local snapshot stale or ahead)
                 2 = the downloaded asset failed validation
  -Offline     no network at all: only confirm the local file exists and is valid
                 0 = ok, 2 = missing/invalid
  -Url URL     override the asset URL
  -Dest PATH   override the target file (relative paths resolve from the repo root)

Exit codes: 0 ok, 1 -Check mismatch, 2 validation or environment failure (including a
missing python interpreter), 3 network/download failure.

Shape conversion: the asset repo stores
  {cli_version, captured_at, source:{workflow,run_url,sha256}, order:[...], tools:{...}}
with {description, parameters, strict} per tool, while toolshape.go:17 embeds the bare
map  {tool name: {description, parameters}}  that json.Unmarshal reads into
map[string]opencodeToolDefinition. The injection writes that flat map, in the asset's
`order` sequence, using the per-tool key set already present in the destination file.
`strict` is dropped: the Go struct has no such field and the outbound builder hardcodes
"strict": false (toolshape.go:224).

The python helper embedded below is a copy of the one embedded in scripts/sync_assets.sh
(mode for mode, line for line). Each script has to be self-contained; keep them in step.
#>
[CmdletBinding()]
param(
    [switch]$Check,
    [switch]$Offline,
    [string]$Url = '',
    [string]$Dest = ''
)

$ErrorActionPreference = 'Stop'

$DefaultUrl = 'https://raw.githubusercontent.com/2061969365/zen-assets-runner/main/assets/official_tools.json'

function Say {
    param([string]$Line)
    Write-Output ('[sync-assets] ' + $Line)
}

$RepoRoot = Split-Path -Parent $PSScriptRoot
if (-not $Url) { $Url = $DefaultUrl }
if (-not $Dest) {
    $Dest = Join-Path $RepoRoot 'tools\opencode_tools.json'
} elseif (-not [System.IO.Path]::IsPathRooted($Dest)) {
    $Dest = Join-Path $RepoRoot $Dest
}

function Resolve-PythonCommand {
    # Candidates are probed by running them: on Windows `python3` is often the Microsoft
    # Store alias stub, which sits on PATH but exits without doing anything.
    $candidates = @()
    if ($env:PYTHON) { $candidates += , @($env:PYTHON) }
    $candidates += , @('python')
    $candidates += , @('python3')
    $candidates += , @('py', '-3')
    foreach ($candidate in $candidates) {
        $exe = $candidate[0]
        if (-not (Get-Command $exe -ErrorAction SilentlyContinue)) { continue }
        $lead = @()
        if ($candidate.Count -gt 1) { $lead = $candidate[1..($candidate.Count - 1)] }
        & $exe @lead -c 'import sys; sys.exit(0 if sys.version_info[0] == 3 else 1)' 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) { return , $candidate }
    }
    return $null
}

$PyCmd = Resolve-PythonCommand
if (-not $PyCmd) {
    Say 'error: no working python3/python found; the canonical sha256 contract needs python'
    Say 'error: set PYTHON=C:\path\to\python.exe to point at one explicitly'
    exit 2
}

# Runs the helper and leaves python's stdout/stderr in the normal pipeline while handing the
# exit code back through script scope, so callers can both see output and branch on status.
$script:LastExit = 0
function Invoke-Py {
    param([string[]]$PyArgs)
    $exe = $script:PyCmd[0]
    if ($script:PyCmd.Count -gt 1) {
        $lead = $script:PyCmd[1..($script:PyCmd.Count - 1)]
        & $exe @lead @PyArgs 2>&1 | ForEach-Object { Write-Output $_ }
    } else {
        & $exe @PyArgs 2>&1 | ForEach-Object { Write-Output $_ }
    }
    $script:LastExit = $LASTEXITCODE
}

function Test-SameBytes {
    param([string]$Left, [string]$Right)
    if (-not (Test-Path -LiteralPath $Left)) { return $false }
    if (-not (Test-Path -LiteralPath $Right)) { return $false }
    $leftHash = (Get-FileHash -LiteralPath $Left -Algorithm SHA256).Hash
    $rightHash = (Get-FileHash -LiteralPath $Right -Algorithm SHA256).Hash
    return ($leftHash -eq $rightHash)
}

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ('sync-assets-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null
$Helper = Join-Path $TempDir 'helper.py'

$helperSource = @'
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
'@

Set-Content -LiteralPath $Helper -Value $helperSource -Encoding ascii

Say ('python: ' + (@($script:PyCmd) -join ' '))

$ExitCode = 0
try {
    if ($Offline) {
        Say 'offline: no network, checking the local file only'
        Say ('dest: ' + $Dest)
        Invoke-Py @($Helper, 'local', $Dest)
        $rc = $script:LastExit
        if ($rc -eq 0) { Say 'done: local snapshot is usable' }
        $ExitCode = $rc
    } else {
        Say ('source url: ' + $Url)
        Say ('dest file:  ' + $Dest)
        $remote = Join-Path $TempDir 'remote.json'
        Invoke-Py @($Helper, 'fetch', $Url, $remote)
        $rc = $script:LastExit
        if ($rc -ne 0) {
            Say 'error: could not download the asset file'
            Say 'error: not falling back to the stale local file; use -Offline to use it on purpose'
            $ExitCode = 3
        } elseif ($Check) {
            Say 'check: validating the downloaded asset, nothing will be written'
            Invoke-Py @($Helper, 'asset', $remote, $Dest, '-')
            $ExitCode = $script:LastExit
        } else {
            Say 'write: validating and projecting the downloaded asset'
            $injected = Join-Path $TempDir 'injected.json'
            Invoke-Py @($Helper, 'asset', $remote, $Dest, $injected)
            $rc = $script:LastExit
            if ($rc -ne 0) {
                $ExitCode = $rc
            } elseif ((Test-Path -LiteralPath $Dest) -and (Test-SameBytes -Left $injected -Right $Dest)) {
                Say ('write: skipped, ' + $Dest + ' is already byte-identical to the asset projection')
                Say 'done'
                $ExitCode = 0
            } else {
                $destDir = Split-Path -Parent $Dest
                if ($destDir -and -not (Test-Path -LiteralPath $destDir)) {
                    New-Item -ItemType Directory -Path $destDir -Force | Out-Null
                }
                if (Test-Path -LiteralPath $Dest) {
                    if (Test-Path -LiteralPath ($Dest + '.orig')) {
                        Say ('backup: ' + $Dest + '.orig already exists, left untouched')
                    } else {
                        Copy-Item -LiteralPath $Dest -Destination ($Dest + '.orig') -Force
                        Say ('backup: ' + $Dest + ' -> ' + $Dest + '.orig')
                    }
                } else {
                    Say ('backup: skipped, no existing ' + $Dest)
                }
                $stage = $Dest + '.sync-stage.' + $PID
                Copy-Item -LiteralPath $injected -Destination $stage -Force
                Move-Item -LiteralPath $stage -Destination $Dest -Force
                Say ('write: ' + $Dest + ' updated (' + (Get-Item -LiteralPath $Dest).Length + ' bytes)')
                Say 'done'
                $ExitCode = 0
            }
        }
    }
} finally {
    Remove-Item -LiteralPath $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}

exit $ExitCode
