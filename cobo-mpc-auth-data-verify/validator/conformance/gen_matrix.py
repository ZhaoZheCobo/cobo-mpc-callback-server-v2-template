"""Cross-language conformance matrix generator: Python (cobo-libs, ground truth) vs Go.

Cobo Auth statement messages are rendered once, in Python (cobo-libs + Jinja2), and
independently rebuilt in Go (this repo's gonja-based validator) to verify a signature.
The two engines have already diverged once in production (null values in "~" string
concatenation - see statement.go's prepareDataForPythonJinjaConcat). Since cobo-libs'
Python/Jinja2 side is fixed and cannot be replaced, the only way to catch the *next*
divergence before it hits a real (and, for the mobile TSS signer, slow-to-patch)
transaction is to fuzz every template with edge-case values and diff Python's real
output against Go's, as a repeatable regression matrix instead of a one-off incident
investigation.

Usage:
    COBO_LIBS_ROOT=/path/to/cobo-libs \
    EXAMPLE_DATAS_DIR=/path/to/validator/template_datas/example_datas \
    OUT_FILE=/path/to/validator/conformance/cases.jsonl \
        /path/to/cobo-libs/.venv/bin/python3 gen_matrix.py

Must run with cobo-libs' own virtualenv so the exact pinned jinja2 version is used.
Each output line is one JSON record: which template, which field path was mutated,
the mutated biz_data, and Python's rendered message (or error). conformance_test.go
consumes this file and renders the same biz_data through the Go engine, diffing.
"""

import copy
import glob
import json
import os
import re

from jinja2 import StrictUndefined, select_autoescape
from jinja2.sandbox import SandboxedEnvironment
from jinja2 import TemplateError

COBO_LIBS_ROOT = os.environ.get("COBO_LIBS_ROOT", "")
COBO_LIBS_TEMPLATES = os.path.join(
    COBO_LIBS_ROOT, "cobo_libs/cobo_auth/templates/json_templates"
)
EXAMPLE_DATAS = os.environ.get("EXAMPLE_DATAS_DIR", "")
OUT_FILE = os.environ.get(
    "OUT_FILE", os.path.join(os.path.dirname(__file__), "cases.jsonl")
)

MAX_MUTATIONS_PER_TEMPLATE = 60

EDGE_CASES = [
    ("null", None),
    ("empty_str", ""),
    ("zero", 0),
    ("negative", -1),
    ("huge_int_str", "99999999999999999999999999999999999999"),
    ("unicode", "测试🎉Ñ"),
]


def make_env():
    """Mirrors AuthStatementTemplateManager._reset_env_filters exactly."""
    env = SandboxedEnvironment(
        undefined=StrictUndefined,
        autoescape=select_autoescape(
            enabled_extensions=("html", "xml"),
            default_for_string=False,
            default=False,
        ),
        trim_blocks=True,
        lstrip_blocks=True,
    )
    env.globals.clear()
    env.filters.clear()
    env.filters["toString"] = lambda v: (
        json.dumps(str(v), ensure_ascii=False)
        if isinstance(v, int)
        else json.dumps(v, ensure_ascii=False)
    )
    env.filters["toInt"] = lambda v: int(v)
    env.filters["len"] = lambda v: len(v)
    env.filters["toList1"] = lambda v: json.dumps(
        [str(x) for x in v if x], ensure_ascii=False
    )
    env.filters["toList2"] = lambda v: json.dumps(
        [[str(x) for x in row if x is not None] for row in v if row],
        ensure_ascii=False,
    )
    env.filters["toRules"] = lambda v: json.dumps(
        [{str(k): str(v) for k, v in x.items()} for x in v if x], ensure_ascii=False
    )
    return env


def render(env, template_text, data):
    try:
        template = env.from_string(template_text)
        rendered = template.render(data)
        parsed = json.loads(rendered)
        message = json.dumps(parsed, ensure_ascii=False, separators=(",", ":"))
        return message, None
    except TemplateError as e:
        return None, f"TemplateError: {e}"
    except json.JSONDecodeError as e:
        return None, f"JSONDecodeError: {e}"
    except Exception as e:
        return None, f"{type(e).__name__}: {e}"


def walk_leaves(obj, path):
    if isinstance(obj, dict):
        for k, v in obj.items():
            yield from walk_leaves(v, path + [("k", k)])
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            yield from walk_leaves(v, path + [("i", i)])
    else:
        yield path, obj


def set_path(data, path, value):
    cur = data
    for _, seg in path[:-1]:
        cur = cur[seg]
    _, seg = path[-1]
    cur[seg] = value


def path_to_str(path):
    parts = []
    for seg_type, seg in path:
        parts.append(seg if seg_type == "k" else f"[{seg}]")
    return ".".join(parts) if parts else ""


VERSION_RE = re.compile(r"^(.*)_(\d+\.\d+\.\d+)\.json\.j2$")


def main():
    if not COBO_LIBS_ROOT or not EXAMPLE_DATAS:
        raise SystemExit("set COBO_LIBS_ROOT and EXAMPLE_DATAS_DIR env vars")

    env = make_env()
    templates = sorted(glob.glob(os.path.join(COBO_LIBS_TEMPLATES, "*.json.j2")))
    total_cases = 0
    templates_with_examples = 0
    templates_no_examples = 0

    with open(OUT_FILE, "w") as out:
        for tpl_path in templates:
            fname = os.path.basename(tpl_path)
            m = VERSION_RE.match(fname)
            if not m:
                continue
            tpl_name, version = m.group(1), m.group(2)

            example_files = sorted(
                glob.glob(os.path.join(EXAMPLE_DATAS, f"{tpl_name}_{version}*.json"))
            )
            if not example_files:
                templates_no_examples += 1
                continue
            templates_with_examples += 1

            with open(tpl_path) as f:
                template_text = f.read()

            for example_file in example_files:
                with open(example_file) as f:
                    base_data = json.load(f)
                if not isinstance(base_data, dict):
                    continue

                leaves = [
                    (p, v)
                    for p, v in walk_leaves(base_data, [])
                    if isinstance(v, (str, int, float, bool)) or v is None
                ]
                leaves = leaves[:MAX_MUTATIONS_PER_TEMPLATE]

                for path, orig_value in leaves:
                    path_str = path_to_str(path)
                    for edge_label, edge_value in EDGE_CASES:
                        if edge_value == orig_value:
                            continue
                        mutated = copy.deepcopy(base_data)
                        set_path(mutated, path, edge_value)

                        message, err = render(env, template_text, mutated)

                        record = {
                            "template_file": fname,
                            "path": path_str,
                            "edge_label": edge_label,
                            "biz_data": mutated,
                            "python_message": message,
                            "python_error": err,
                        }
                        out.write(json.dumps(record, ensure_ascii=False) + "\n")
                        total_cases += 1

    print(f"templates_with_examples={templates_with_examples}")
    print(f"templates_no_examples={templates_no_examples}")
    print(f"total_cases={total_cases}")
    print(f"wrote {OUT_FILE}")


if __name__ == "__main__":
    main()
