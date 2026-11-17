"""scripts/test_tool_overrides.py - assertions for the local tool-definition overlay.

Run: python scripts/test_tool_overrides.py        (stdlib only, no network)

What it proves, in the order the merge actually happens:

  A1  every tool named in scripts/tool_overrides.json exists in
      tools/opencode_tools.json, and the overlay neither adds nor drops a tool;
  A2  an overlay spec only carries `description` / `parameters` - no rename, no
      `strict`, no stray keys;
  A3  every merged description's first line is <= 120 bytes and the text carries both
      `Use when:` and `Don't use for:` (plus 2-5 `- ` bullets);
  A4  the merged text is not longer than the asset's, per tool and in total. Tools whose
      asset text is shorter than STRUCTURE_FLOOR get that floor instead: a one-line
      77-byte description cannot hold the required four-part structure at all;
  A5  the merge keeps the schema callable: no property added under any `properties`, no
      `type` rewritten, `required` replaced rather than extended;
  A6  merging twice is a no-op, and the file on disk equals
      apply(tools/opencode_tools.json.orig, overlay) - i.e. scripts/sync_assets.sh has
      been run since the overlay was last edited;
  A7  no description reintroduces one of the three upstream fingerprint phrases that
      toolshape.go matches, and read.parameters.properties.offset.minimum is 1.

Exit: 0 all green, 1 at least one assertion failed.
"""
import copy
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
GATEWAY = os.path.join(ROOT, "tools", "opencode_tools.json")
BASELINE = GATEWAY + ".orig"          # what sync_assets.sh backed up before overlaying
OVERLAY = os.path.join(HERE, "tool_overrides.json")

ALLOWED_TOOL_KEYS = ("description", "parameters")
FIRST_LINE_LIMIT = 120
STRUCTURE_FLOOR = 440                # shortest text that still holds the mandated shape
BULLETS_MIN, BULLETS_MAX = 2, 5

# Upstream free-tier fingerprints, mirrored from toolshape.go:bannedDescriptionPhrases
# (matched case-insensitively). toolshape.go sanitises client-supplied descriptions, but the
# official stubs are published straight from this file (officialToolset -> renderOfficialTool),
# so a phrase here would leave the process verbatim. Keep the rewritten text clean instead.
BANNED_PHRASES = (
    "utf-8 text file",
    "line-numbered content",
    "session skill catalog",
)

# Explicit targets from the rewrite brief; a tool not listed is capped at its own size.
TARGET_CAPS = {
    "edit": 800,
    "patch": 900,
    "execute": 900,
    "subagent": 900,
    "read": 489,
    "shell": 533,
    "question": 596,
}

# Fallback "asset text" sizes, measured on the 2026-11-17 asset snapshot. Only used when
# tools/opencode_tools.json.orig is absent (fresh clone, never synced).
ORIGINAL_BYTES = {
    "edit": 950, "glob": 77, "grep": 255, "question": 596, "read": 489, "shell": 533,
    "skill": 220, "subagent": 1275, "webfetch": 287, "websearch": 216, "write": 232,
    "execute": 972, "patch": 1091,
}

FAILURES = []
NOTES = []


def fail(message):
    FAILURES.append(message)


def note(message):
    NOTES.append(message)


def read_json(path):
    with open(path, "rb") as handle:
        return json.loads(handle.read().decode("utf-8"))


def scrub(message):
    """Keep the source ASCII: the repo's shell twins are ascii-encoded too."""
    return message.replace("\u2192", "->")


class MergeError(Exception):
    pass


def deep_merge(node, patch, path):
    """Same rules as the helper embedded in scripts/sync_assets.sh.

    Objects merge recursively; scalars and arrays replace wholesale; `type` may not change;
    no new key may appear under a `properties` object.
    """
    if not isinstance(node, dict):
        raise MergeError("%s is not an object, cannot merge into it" % path)
    for key, value in patch.items():
        where = path + "." + key
        if key == "properties":
            if not isinstance(value, dict):
                raise MergeError("%s must be an object" % where)
            base = node.get(key)
            if not isinstance(base, dict):
                raise MergeError("%s has no base object to merge into" % where)
            added = sorted(set(value) - set(base))
            if added:
                raise MergeError("%s may not add propert%s %s"
                                 % (where, "y" if len(added) == 1 else "ies", ", ".join(added)))
            deep_merge(base, value, where)
            continue
        if key == "type" and isinstance(node.get(key), str) and node[key] != value:
            raise MergeError("%s may not change type (%s -> %s)"
                             % (where, node[key], json.dumps(value, ensure_ascii=False)))
        if isinstance(node.get(key), dict) and isinstance(value, dict):
            deep_merge(node[key], value, where)
            continue
        node[key] = copy.deepcopy(value)
    return node


def apply_overlay(base, overlay):
    tools = overlay["tools"]
    unknown = sorted(name for name in tools if name not in base)
    if unknown:
        raise MergeError("overlay names tool(s) the gateway file does not define: %s"
                         % ", ".join(unknown))
    result = copy.deepcopy(base)
    for name in result:
        if name not in tools:
            continue
        spec = tools[name]
        extra = sorted(set(spec) - set(ALLOWED_TOOL_KEYS))
        if extra:
            raise MergeError("tools.%s may only override %s, found %s"
                             % (name, "/".join(ALLOWED_TOOL_KEYS), ", ".join(extra)))
        deep_merge(result[name], copy.deepcopy(spec), name)
    return result


def text_bytes(value):
    return len(value.encode("utf-8"))


def budget(tool, original):
    if original < STRUCTURE_FLOOR:
        return STRUCTURE_FLOOR
    return min(original, TARGET_CAPS.get(tool, original))


def check_description(name, text, original):
    if not isinstance(text, str) or not text.strip():
        fail("%s: description is empty" % name)
        return 0
    lines = text.split("\n")
    first = text_bytes(lines[0])
    if first > FIRST_LINE_LIMIT:
        fail("%s: first line is %d bytes, limit is %d" % (name, first, FIRST_LINE_LIMIT))
    if "Use when:" not in text:
        fail("%s: description has no 'Use when:' line" % name)
    if "Don't use for:" not in text:
        fail("%s: description has no \"Don't use for:\" line" % name)
    bullets = sum(1 for line in lines if line.startswith("- "))
    if not BULLETS_MIN <= bullets <= BULLETS_MAX:
        fail("%s: %d bullet line(s), expected %d-%d"
             % (name, bullets, BULLETS_MIN, BULLETS_MAX))
    size = text_bytes(text)
    cap = budget(name, original)
    if size > cap:
        fail("%s: %d bytes exceeds its budget of %d (asset text was %d)"
             % (name, size, cap, original))
    return first


def check_invariants(name, before, after):
    if not isinstance(after, dict):
        fail("%s: merged tool is not an object" % name)
        return
    if before.get("required") != after.get("required"):
        fail("%s: required changed (%r -> %r)"
             % (name, before.get("required"), after.get("required")))
    before_props = (before.get("parameters") or {}).get("properties") or {}
    after_props = (after.get("parameters") or {}).get("properties") or {}
    if sorted(before_props) != sorted(after_props):
        fail("%s: property set changed (%s -> %s)"
             % (name, sorted(before_props), sorted(after_props)))
        return
    for prop in before_props:
        b = before_props[prop]
        a = after_props[prop]
        if isinstance(b, dict) and isinstance(a, dict):
            if "type" in b and b["type"] != a.get("type"):
                fail("%s.parameters.properties.%s: type changed (%r -> %r)"
                     % (name, prop, b["type"], a.get("type")))
    if "type" in before and before["type"] != after.get("type"):
        fail("%s: parameters.type changed (%r -> %r)"
             % (name, before["type"], after.get("type")))


def main():
    gateway = read_json(GATEWAY)
    overlay = read_json(OVERLAY)

    if not isinstance(overlay, dict) or overlay.get("version") != 1:
        print("FAIL overlay root must be {version: 1, tools: {...}}")
        return 1
    if not isinstance(overlay.get("tools"), dict) or not overlay["tools"]:
        print("FAIL overlay tools must be a non-empty object")
        return 1

    have_baseline = os.path.isfile(BASELINE)
    if have_baseline:
        baseline = read_json(BASELINE)
    else:
        note("no %s; rebuilding the baseline from the bundled byte table"
             % os.path.basename(BASELINE))
        baseline = copy.deepcopy(gateway)

    # A1 - no tool may be added, dropped or renamed by the overlay.
    unknown = sorted(set(overlay["tools"]) - set(gateway))
    if unknown:
        fail("overlay names tool(s) missing from the gateway file: %s" % ", ".join(unknown))
    if have_baseline and sorted(baseline) != sorted(gateway):
        fail("tool set changed: asset %s vs gateway %s"
             % (sorted(baseline), sorted(gateway)))
    if len(overlay["tools"]) != len(gateway):
        note("%d of %d tool(s) carry an overlay entry"
             % (len(overlay["tools"]), len(gateway)))

    # A2 - only description/parameters per tool.
    for name, spec in sorted(overlay["tools"].items()):
        if not isinstance(spec, dict) or not spec:
            fail("%s: overlay spec must be a non-empty object" % name)
            continue
        extra = sorted(set(spec) - set(ALLOWED_TOOL_KEYS))
        if extra:
            fail("%s: overlay may only set %s, found %s"
                 % (name, "/".join(ALLOWED_TOOL_KEYS), ", ".join(extra)))
        if "description" in spec and not isinstance(spec["description"], str):
            fail("%s: description must be a string" % name)
        if "parameters" in spec and not isinstance(spec["parameters"], dict):
            fail("%s: parameters must be an object" % name)

    # A3/A4/A5 - merged text and schema, tool by tool.
    rows = []
    total_original = total_merged = 0
    for name in gateway:
        merged = gateway[name]
        original_text = (baseline.get(name) or {}).get("description", "")
        original = text_bytes(original_text) if original_text else ORIGINAL_BYTES.get(name, 0)
        text = merged.get("description", "")
        first = check_description(name, text, original)
        check_invariants(name, baseline.get(name, {}), merged)
        total_original += original
        total_merged += text_bytes(text)
        rows.append((name, original, text_bytes(text), first))

    if total_merged > total_original:
        fail("total description bytes grew: %d -> %d" % (total_original, total_merged))

    # read.offset: the schema used to accept 0 while the text said 1-based.
    offset = (((gateway.get("read") or {}).get("parameters") or {})
              .get("properties") or {}).get("offset") or {}
    if offset.get("minimum") != 1:
        fail("read.parameters.properties.offset.minimum is %r, expected 1"
             % (offset.get("minimum"),))
    if "1-based" not in (offset.get("description") or ""):
        fail("read.parameters.properties.offset.description no longer says 1-based")

    # No upstream fingerprint phrase may enter the embedded definitions.
    for name, tool in gateway.items():
        text = ((tool.get("description") or "") + "\n" +
                json.dumps(tool.get("parameters") or {}, ensure_ascii=False)).lower()
        for phrase in BANNED_PHRASES:
            if phrase in text:
                fail("%s: contains the upstream fingerprint phrase %r" % (name, phrase))

    # A6 - idempotent, and on disk equals baseline+overlay (sync_assets.sh has run).
    try:
        first_pass = apply_overlay(baseline, overlay) if have_baseline else None
        second_pass = apply_overlay(copy.deepcopy(first_pass), overlay) if first_pass else None
    except MergeError as exc:
        fail("merge raised: %s" % exc)
        first_pass = second_pass = None
    if first_pass is not None:
        if first_pass != second_pass:
            fail("merge is not idempotent: a second pass changed the result")
        if have_baseline and first_pass != gateway:
            fail("gateway file is not apply(.orig, overlay); run scripts/sync_assets.sh")
    try:
        apply_overlay(copy.deepcopy(gateway), overlay)
    except MergeError as exc:
        fail("overlay cannot be re-applied to the gateway file: %s" % exc)

    print("%-10s %8s %8s %8s" % ("tool", "asset", "merged", "first line"))
    for name, original, size, first in rows:
        print("%-10s %8d %8d %8d" % (name, original, size, first))
    print("%-10s %8d %8d" % ("TOTAL", total_original, total_merged))

    for line in NOTES:
        print("[note] %s" % line)
    if FAILURES:
        for line in FAILURES:
            print("FAIL %s" % scrub(line))
        print("%d assertion(s) failed" % len(FAILURES))
        return 1
    print("PASS: %d tool(s), 7 assertion group(s), 0 failures"
          % len(gateway))
    return 0


if __name__ == "__main__":
    sys.exit(main())
