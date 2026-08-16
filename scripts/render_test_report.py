#!/usr/bin/env python3
"""Render a JUnit XML result set plus flow screenshots into a static report.

Output is a self-contained directory (index.html + shots/) that CI uploads as a
static site, so a reviewer opens one URL instead of downloading a zip.

The report answers, in this order: did it pass, what did the user see, why did
it fail, how do I reproduce it, and what exactly produced this. Provenance is
last because it is the least urgent and the most often needed later.

Test-framework agnostic. It needs JUnit XML (gotestsum --junitfile, pytest
--junit-xml, vitest --reporter=junit) and, optionally, one directory per flow
under --artifacts holding NN-label.png screenshots plus a meta.json naming the
test they belong to. Flows are matched to results on that name; a flow that
matches nothing is reported rather than silently dropped.

Usage:
    render_test_report.py --junit results.xml --artifacts artifacts/ --out report/
"""

from __future__ import annotations

import argparse
import html
import json
import platform
import shutil
import sys
import xml.etree.ElementTree as ET
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path


@dataclass
class Case:
    nodeid: str
    name: str
    classname: str
    time: float
    status: str  # passed | failed | error | skipped
    message: str = ""
    detail: str = ""
    shots: list[Path] = field(default_factory=list)
    flow: str = ""


def parse_junit(path: Path) -> tuple[list[Case], dict]:
    root = ET.parse(path).getroot()
    suites = [root] if root.tag == "testsuite" else list(root.iter("testsuite"))
    cases: list[Case] = []
    for suite in suites:
        for el in suite.iter("testcase"):
            file_attr = el.get("file") or ""
            name = el.get("name") or "?"
            status, message, detail = "passed", "", ""
            for tag in ("failure", "error", "skipped"):
                found = el.find(tag)
                if found is not None:
                    status = {"failure": "failed", "error": "error", "skipped": "skipped"}[tag]
                    message = found.get("message") or ""
                    detail = found.text or ""
                    break
            cases.append(
                Case(
                    nodeid=f"{file_attr}::{name}" if file_attr else name,
                    name=name,
                    classname=el.get("classname") or "",
                    time=float(el.get("time") or 0.0),
                    status=status,
                    message=message,
                    detail=detail,
                )
            )
    totals = {
        "total": len(cases),
        "passed": sum(c.status == "passed" for c in cases),
        "failed": sum(c.status in ("failed", "error") for c in cases),
        "skipped": sum(c.status == "skipped" for c in cases),
        "duration": sum(c.time for c in cases),
    }
    return cases, totals


def attach_shots(cases: list[Case], artifacts: Path, out: Path) -> list[dict]:
    """Join filmstrips to results on the nodeid FlowRecorder recorded.

    A flow whose nodeid matches nothing is reported rather than dropped: it
    means the recorder ran outside a test, or the join key drifted.
    """
    shots_out = out / "shots"
    orphans: list[dict] = []
    by_node = {c.nodeid: c for c in cases}
    # JUnit writes the file path; PYTEST_CURRENT_TEST writes the same. Fall back
    # to matching on the bare test name when the prefix shape differs.
    by_name = {c.name: c for c in cases}

    for meta_path in sorted(artifacts.glob("*/meta.json")):
        flow_dir = meta_path.parent
        try:
            meta = json.loads(meta_path.read_text())
        except (OSError, ValueError):
            continue
        nodeid = (meta.get("nodeid") or "").strip()
        case = by_node.get(nodeid)
        if case is None and "::" in nodeid:
            case = by_name.get(nodeid.rsplit("::", 1)[-1])

        pngs = sorted(flow_dir.glob("*.png"))
        dest = shots_out / flow_dir.name
        dest.mkdir(parents=True, exist_ok=True)
        copied = []
        for p in pngs:
            shutil.copy2(p, dest / p.name)
            copied.append(Path("shots") / flow_dir.name / p.name)

        if case is None:
            orphans.append(
                {"flow": meta.get("flow") or flow_dir.name, "nodeid": nodeid, "shots": copied}
            )
        else:
            case.shots = copied
            case.flow = meta.get("flow") or flow_dir.name
    return orphans


def step_label(filename: str) -> str:
    stem = Path(filename).stem
    n, _, rest = stem.partition("-")
    return f"{n}. {rest.replace('-', ' ')}" if n.isdigit() else stem.replace("-", " ")


CSS = """
:root{--paper:#f6f1e6;--card:#fffaf0;--ink:#1d211f;--muted:#6d7068;--line:#cfc7b7;
--green:#1e7f68;--green-soft:#dcebe3;--red:#b44a3c;--red-soft:#f7e3de;--amber:#8a6410;
--amber-soft:#f5e7bf;--tight:"Avenir Next Condensed","Arial Narrow",sans-serif;
--text:"Iowan Old Style",Palatino,Georgia,serif;--mono:"SFMono-Regular",Menlo,Consolas,monospace}
*{box-sizing:border-box}
body{margin:0;background:var(--paper);color:var(--ink);font-family:var(--text);
font-size:17px;line-height:1.55;-webkit-text-size-adjust:100%}
.shell{width:min(1100px,calc(100% - 26px));margin:0 auto;padding-bottom:4rem}
.verdict{margin:1.8rem 0 .4rem;padding:1.1rem 1.3rem;border-radius:9px;border:2px solid}
.verdict.pass{background:var(--green-soft);border-color:var(--green)}
.verdict.fail{background:var(--red-soft);border-color:var(--red)}
.verdict h1{font-family:var(--tight);font-size:clamp(1.9rem,7vw,3rem);margin:0;letter-spacing:-.01em}
.verdict.pass h1{color:var(--green)}
.verdict.fail h1{color:var(--red)}
.counts{font-family:var(--mono);font-size:.9rem;margin-top:.35rem;color:var(--ink)}
.ident{display:flex;flex-wrap:wrap;gap:.3rem 1.4rem;font-size:.85rem;color:var(--muted);
margin:0 0 2rem;padding:.7rem 0;border-bottom:1px solid var(--line)}
.ident b{font-weight:400;color:var(--ink);font-family:var(--mono);font-size:.82rem}
h2{font-family:var(--tight);font-size:1.5rem;margin:2.4rem 0 .8rem;padding-top:1.1rem;
border-top:2px solid var(--ink)}
.case{background:var(--card);border:1px solid var(--line);border-radius:8px;
padding:1rem 1.1rem 1.1rem;margin-bottom:1.2rem}
.case.failed{border-color:var(--red);border-left-width:4px}
.chead{display:flex;align-items:baseline;justify-content:space-between;gap:.8rem;flex-wrap:wrap}
.cname{font-family:var(--mono);font-size:.92rem;font-weight:600;word-break:break-all}
.pill{font-family:var(--mono);font-size:.66rem;text-transform:uppercase;letter-spacing:.1em;
padding:.2em .6em;border-radius:99px;border:1px solid;white-space:nowrap}
.pill.passed{background:var(--green-soft);color:var(--green);border-color:var(--green)}
.pill.failed,.pill.error{background:var(--red-soft);color:var(--red);border-color:var(--red)}
.pill.skipped{background:var(--amber-soft);color:var(--amber);border-color:var(--amber)}
.dur{font-family:var(--mono);font-size:.78rem;color:var(--muted)}
.strip{display:flex;gap:.7rem;overflow-x:auto;padding:.9rem .1rem .3rem;
scroll-snap-type:x mandatory;-webkit-overflow-scrolling:touch}
figure{margin:0;flex:0 0 auto;width:min(190px,44vw);scroll-snap-align:start}
figure img{width:100%;height:340px;object-fit:cover;object-position:top;background:#fff;
border:1px solid var(--line);border-radius:6px;display:block}
figcaption{font-family:var(--mono);font-size:.66rem;color:var(--muted);margin-top:.35rem;text-align:center}
pre{background:#2b2b28;color:#f0ece2;padding:.9rem 1rem;border-radius:6px;overflow-x:auto;
font-family:var(--mono);font-size:.76rem;line-height:1.5;margin:.7rem 0 0}
pre.cmd{background:var(--card);color:var(--ink);border:1px solid var(--line)}
.note{font-size:.88rem;color:var(--muted)}
.orphan{border-left:4px solid var(--amber)}
a{color:var(--green)}
footer{color:var(--muted);font-size:.82rem;border-top:1px solid var(--line);
padding-top:1rem;margin-top:2.5rem}
"""


def render(cases, totals, orphans, meta) -> str:
    ok = totals["failed"] == 0
    bits = [f'{totals["passed"]} passed']
    if totals["failed"]:
        bits.append(f'{totals["failed"]} failed')
    if totals["skipped"]:
        bits.append(f'{totals["skipped"]} skipped')
    counts = ", ".join(bits) + f' in {totals["duration"]:.2f}s'

    def ident_value(v: str) -> str:
        safe = html.escape(str(v))
        if str(v).startswith("http"):
            # Shown short so a phone-width strip stays readable, still tappable.
            return f'<a href="{safe}">open</a>'
        return f"<b>{safe}</b>"

    ident = "".join(
        f"<span>{html.escape(k)} {ident_value(v)}</span>" for k, v in meta["ident"].items() if v
    )

    def case_html(c: Case, orphan=False) -> str:
        figs = "".join(
            f'<figure><a href="{p.as_posix()}"><img loading="lazy" src="{p.as_posix()}" '
            f'alt="{html.escape(step_label(p.name))}"></a>'
            f"<figcaption>{html.escape(step_label(p.name))}</figcaption></figure>"
            for p in c.shots
        )
        strip = f'<div class="strip">{figs}</div>' if figs else ""
        fail = ""
        if c.status in ("failed", "error"):
            body = (c.message + "\n\n" + c.detail).strip()
            fail = f"<pre>{html.escape(body[:4000])}</pre>"
        flow = f' <span class="dur">{html.escape(c.flow)}</span>' if c.flow else ""
        return f"""<div class="case {c.status}{' orphan' if orphan else ''}">
  <div class="chead">
    <span class="cname">{html.escape(c.name)}{flow}</span>
    <span><span class="pill {c.status}">{c.status}</span> <span class="dur">{c.time:.2f}s</span></span>
  </div>
  {fail}{strip}
</div>"""

    ordered = sorted(cases, key=lambda c: (c.status == "passed", c.name))
    documented = [c for c in ordered if c.shots]
    plain = [c for c in ordered if not c.shots]

    orphan_html = ""
    if orphans:
        rows = "".join(
            case_html(
                Case(
                    nodeid="",
                    name=o["flow"],
                    classname="",
                    time=0.0,
                    status="skipped",
                    shots=o["shots"],
                    flow=f'unmatched nodeid: {o["nodeid"] or "(none)"}',
                ),
                orphan=True,
            )
            for o in orphans
        )
        orphan_html = (
            "<h2>Unmatched filmstrips</h2>"
            '<p class="note">Screenshots whose recorded test id matched no result. '
            "Usually the recorder ran outside a test, or the join key drifted.</p>" + rows
        )

    return f"""<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<title>{html.escape(meta['title'])}</title>
<style>{CSS}</style></head><body><div class="shell">

<div class="verdict {'pass' if ok else 'fail'}">
  <h1>{'PASSED' if ok else 'FAILED'}</h1>
  <div class="counts">{html.escape(counts)}</div>
</div>
<div class="ident">{ident}</div>

<h2>Flows</h2>
{''.join(case_html(c) for c in documented) or '<p class="note">No flow recorded a filmstrip.</p>'}

<h2>Other tests</h2>
{''.join(case_html(c) for c in plain) or '<p class="note">None.</p>'}

{orphan_html}

<h2>Reproduce</h2>
<pre class="cmd">{html.escape(meta['reproduce'])}</pre>

<h2>Environment</h2>
<pre class="cmd">{html.escape(meta['environment'])}</pre>

<footer>
  Generated {html.escape(meta['generated'])} by <code>scripts/render_test_report.py</code>.
  Screenshots are taken by the tests themselves as they run, on pass as well as failure.
</footer>
</div></body></html>
"""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--junit", required=True, type=Path)
    ap.add_argument("--artifacts", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--title", default="e2e report")
    ap.add_argument("--commit", default="")
    ap.add_argument("--branch", default="")
    ap.add_argument("--pr", default="")
    ap.add_argument("--run-url", default="")
    ap.add_argument("--repo", default="")
    ap.add_argument("--reproduce", default="", help="command a reader runs to rerun this locally")
    a = ap.parse_args()

    if not a.junit.exists():
        print(f"no junit xml at {a.junit}", file=sys.stderr)
        return 1

    cases, totals = parse_junit(a.junit)
    a.out.mkdir(parents=True, exist_ok=True)
    orphans = attach_shots(cases, a.artifacts, a.out) if a.artifacts.exists() else []

    meta = {
        "title": a.title,
        "generated": datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC"),
        "ident": {
            "commit": a.commit[:8],
            "branch": a.branch,
            "pr": f"#{a.pr}" if a.pr else "",
            "repo": a.repo,
            "ci run": a.run_url,
        },
        "reproduce": (
            f"git fetch origin && git checkout {a.commit[:8] or '<sha>'}\n"
            f"{a.reproduce or 'make e2e-ci'}"
        ),
        "environment": (
            f"python   {platform.python_version()}\n"
            f"platform {platform.system()} {platform.machine()}\n"
            f"tests    {totals['total']} collected"
        ),
    }

    (a.out / "index.html").write_text(render(cases, totals, orphans, meta))
    print(
        f"report: {totals['passed']} passed, {totals['failed']} failed, "
        f"{len(orphans)} unmatched filmstrip(s) -> {a.out}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
