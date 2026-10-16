# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.


"""Charts for ateapi-cold-resume-summary.md: the throughput knee, resume
latency against throughput, and size10 replica scaling.

    python3 ateapi-cold-resume-charts/make_charts.py

Run from the repository root. Writes PNGs and the CSVs they plot into
ateapi-cold-resume-charts/.
"""

import csv
import json
import os

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.ticker import FuncFormatter, LogLocator, NullFormatter

OUT = os.path.dirname(os.path.abspath(__file__))
S0 = "ateapi-cold-size0-5tests-20261015/results"
S10 = "ateapi-cold-4to9x8-size10-20261015/results"

# (label, steps.jsonl, first saturated target)
SIZE0 = [
    ("1 × 2", f"{S0}/2cpu/steps.jsonl", 1000),
    ("1 × 4", f"{S0}/4cpu/steps.jsonl", 1750),
    ("1 × 8", f"{S0}/8cpu/steps.jsonl", 2700),
    ("1 × 10", f"{S0}/10cpu/steps.jsonl", 3000),
    ("2 × 4", f"{S0}/2x4cpu/steps.jsonl", 3000),
    ("2 × 8", "ateapi-cold-6tests-20261014/results/repeat-20261015/2x8cpu/steps.jsonl", 4000),
]
SIZE10 = [
    (2, f"{OUT}/data/size10-2x8-20261014-steps.jsonl", 4000, "ateapi-cold-6tests-20261014/results/wal-io.csv"),
    (3, f"{OUT}/data/size10-3x8-20261014-steps.jsonl", 5500, "ateapi-cold-6tests-20261014/results/wal-io.csv"),
    (4, f"{S10}/4x8cpu_size10/steps.jsonl", 6000, f"{S10}/wal-io.csv"),
    (5, f"{S10}/5x8cpu_size10/steps.jsonl", 7000, f"{S10}/wal-io.csv"),
    (6, f"{S10}/6x8cpu_size10/steps.jsonl", 7500, f"{S10}/wal-io.csv"),
    (7, f"{S10}/rerun/7x8cpu_size10/steps.jsonl", 7500, f"{S10}/rerun/wal-io.csv"),
    (8, f"{S10}/rerun/8x8cpu_size10/steps.jsonl", 7500, f"{S10}/rerun/wal-io.csv"),
    (9, f"{S10}/9x8cpu_size10/steps.jsonl", 7500, f"{S10}/9x8cpu_size10/wal-io.csv"),
]
EARLIER = {2, 3}  # 2026-10-14, at an earlier commit

# Reference palette (dataviz skill), light mode. Validated: categorical slots
# 1-6 pass on adjacent pairs; the four-step blue ramp passes as ordinal.
SURFACE = "#ffffff"
TEXT = "#0b0b0b"
TEXT2 = "#52514e"
GRID = "#e6e5e1"
REF = "#b5b4ae"
MEAN = "#52514e"  # text-secondary gray: a reference line, not a series hue
CATEGORICAL = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300"]
RAMP4 = ["#86b6ef", "#3987e5", "#1c5cab", "#0d366b"]

plt.rcParams.update({
    "font.family": "DejaVu Sans", "font.size": 10,
    "axes.edgecolor": GRID, "axes.linewidth": 1, "axes.labelcolor": TEXT2,
    "axes.titlesize": 11, "axes.titleweight": "bold", "axes.titlecolor": TEXT,
    "xtick.color": TEXT2, "ytick.color": TEXT2, "xtick.labelsize": 9, "ytick.labelsize": 9,
    "axes.grid": True, "grid.color": GRID, "grid.linewidth": 1, "grid.linestyle": "-",
    "axes.spines.top": False, "axes.spines.right": False,
    "figure.facecolor": SURFACE, "axes.facecolor": SURFACE,
    "legend.frameon": False, "legend.fontsize": 9,
    "lines.linewidth": 2, "lines.solid_capstyle": "round", "lines.solid_joinstyle": "round",
})
compact = FuncFormatter(lambda v, _: f"{v / 1000:g}K" if v >= 1000 else f"{v:g}")


def steps(path):
    with open(path) as f:
        return [json.loads(line) for line in f]


def wal_rows(path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            try:
                rows.append({k: float(v) for k, v in r.items() if k != "timing"})
            except ValueError:
                pass
    return rows


def wal_window(rows, a, b):
    """fsyncs/s, mean fsync ms and commits per fsync over [a, b], or None."""
    w = [r for r in rows if a <= r["ts"] <= b]
    if len(w) < 2:
        return None
    x, y = w[0], w[-1]
    n = y["wal_fsyncs"] - x["wal_fsyncs"]
    if n <= 0:
        return None
    dt = y["ts"] - x["ts"]
    return n / dt, (y["wal_fsync_ms"] - x["wal_fsync_ms"]) / n, (y["xact_commit"] - x["xact_commit"]) / n


def end_labels(ax, ends, color_of, label_x, min_gap):
    """Label line ends in a column at label_x, spread at least min_gap apart,
    each joined to its line end by a thin leader line."""
    order = sorted(ends, key=lambda e: e[2])
    placed = []
    for _, _, y in order:
        placed.append(max(y, placed[-1] + min_gap) if placed else y)
    for (label, x, y), ly in zip(order, placed):
        ax.plot([x, label_x], [y, ly], color=REF, linewidth=1, zorder=1, clip_on=False)
        ax.plot([label_x], [ly], marker="o", markersize=5, color=color_of[label],
                markeredgecolor=SURFACE, markeredgewidth=1, clip_on=False, zorder=4)
        ax.annotate(label, (label_x, ly), xytext=(6, 0), textcoords="offset points",
                    va="center", ha="left", color=TEXT, fontsize=9, annotation_clip=False)


def series_size0():
    return [(lab, CATEGORICAL[i], steps(p)) for i, (lab, p, _) in enumerate(SIZE0)]


def series_size10(group):
    sel = [s for s in SIZE10 if s[0] in group]
    return [(f"{n} × 8", RAMP4[i], steps(p)) for i, (n, p, _, _) in enumerate(sel)]


PANELS = [
    ("size0 Postgres", series_size0),
    ("size10, 2–5 replicas × 8 CPUs", lambda: series_size10({2, 3, 4, 5})),
    ("size10, 6–9 replicas × 8 CPUs", lambda: series_size10({6, 7, 8, 9})),
]


def knee():
    fig, axes = plt.subplots(1, 3, figsize=(13.5, 4.6))
    rows = []
    for ax, (title, get) in zip(axes, PANELS):
        series = get()
        lo = min(d["target_rps"] for _, _, st in series for d in st)
        hi = max(d["target_rps"] for _, _, st in series for d in st)
        ax.plot([0, hi * 1.02], [0, hi * 1.02], color=REF, linewidth=1, zorder=1)
        ends, colors = [], {}
        for label, color, st in series:
            xs = [d["target_rps"] for d in st]
            ys = [d["resume_rps"] for d in st]
            ax.plot(xs, ys, color=color, label=label, zorder=3)
            ax.plot(xs, ys, "o", markersize=5, color=color, markeredgecolor=SURFACE, markeredgewidth=1.5, zorder=4)
            ends.append((label, xs[-1], ys[-1]))
            colors[label] = color
            rows += [{"panel": title, "setup": label, "target_cycles_per_s": round(x), "achieved_cycles_per_s": round(y, 1)} for x, y in zip(xs, ys)]
        x0 = 0 if title.startswith("size0") else lo * 0.9
        ax.set_xlim(x0, hi * 1.0)
        ax.set_ylim(x0, hi * 1.02)
        ax.annotate("achieved = target", (hi * 0.97, hi * 0.97), ha="right", va="bottom", color=TEXT2, fontsize=8, rotation=0)
        span = hi - x0
        if not title.startswith("size10, 6"):  # those lines converge; the legend carries them
            end_labels(ax, ends, colors, hi + span * 0.03, span * 0.05)
        ax.set_title(title, loc="left")
        ax.set_xlabel("Target cycles/s")
        ax.xaxis.set_major_formatter(compact)
        ax.yaxis.set_major_formatter(compact)
        ax.legend(loc="upper left", handlelength=1.2)
    axes[0].set_ylabel("Achieved cycles/s")
    fig.suptitle("Cold resume throughput levels off at each setup's ceiling", x=0.01, ha="left",
                 fontsize=13, fontweight="bold", color=TEXT)
    fig.text(0.01, 0.005, "One cycle = one cold resume + one pause.\n"
             "Setups are ate-api-server replicas × CPUs per replica.\n"
             "Each point is a 3-minute ladder step (steady window).",
             color=TEXT2, fontsize=8)
    fig.tight_layout(rect=(0, 0.075, 1, 0.985), w_pad=6)
    fig.savefig(f"{OUT}/1-throughput-knee.png", dpi=200)
    write_csv("1-throughput-knee.csv", rows)


def scaling():
    pts = []
    for n, path, sat_from, walp in SIZE10:
        st = steps(path)
        sat = [d for d in st if d["target_rps"] >= sat_from]
        wal = wal_rows(walp)
        ws = [w for w in (wal_window(wal, *d["window"]) for d in sat) if w]
        pts.append({
            "replicas": n,
            "date": "2026-10-14" if n in EARLIER else "2026-10-15",
            "ceiling_cycles_per_s": round(sum(d["resume_rps"] for d in sat) / len(sat)),
            "commits_per_s": round(sum(d["pg_commits_per_s"] for d in sat) / len(sat)),
            "fsyncs_per_s": round(sum(w[0] for w in ws) / len(ws)) if ws else "",
            "mean_fsync_ms": round(sum(w[1] for w in ws) / len(ws), 2) if ws else "",
            "commits_per_fsync": round(sum(w[2] for w in ws) / len(ws)) if ws else "",
        })
    write_csv("3-size10-scaling.csv", pts)

    fig, axes = plt.subplots(4, 1, figsize=(8, 12.6), sharex=True)
    panels = [
        ("ceiling_cycles_per_s", "Ceiling, cycles/s", "Throughput stops rising after 6 replicas", compact),
        ("commits_per_s", "Postgres commits/s", "Postgres commits level off at ~180,000/s, 24 per cycle", compact),
        ("fsyncs_per_s", "WAL fsyncs/s", "That is fsyncs/s, which fall as each fsync takes 0.53 → 0.74 ms…", compact),
        ("commits_per_fsync", "Commits per fsync", "…times commits per fsync, which rise until the two cancel", FuncFormatter(lambda v, _: f"{v:g}")),
    ]
    color = CATEGORICAL[0]
    for ax, (key, ylabel, title, fmt) in zip(axes, panels):
        ok = [p for p in pts if p[key] != ""]
        xs = [p["replicas"] for p in ok]
        ys = [p[key] for p in ok]
        ax.plot(xs, ys, color=color, zorder=3)
        ax.plot(xs, ys, "o", markersize=8, zorder=4, color=color, markeredgecolor=SURFACE, markeredgewidth=2)
        for p in (ok[0], ok[-1]):
            ax.annotate(f"{p[key]:,}", (p["replicas"], p[key]), textcoords="offset points", xytext=(0, 9),
                        ha="center", color=TEXT, fontsize=9)
        ax.set_ylabel(ylabel)
        ax.set_title(title, loc="left", fontsize=10.5)
        ax.yaxis.set_major_formatter(fmt)
        ax.set_ylim(0, max(ys) * 1.22)
    for ax in axes[2:]:
        ax.annotate("9 × 8 not sampled", (9, ax.get_ylim()[1] * 0.1), ha="center", color=TEXT2, fontsize=8)
    axes[-1].set_xlabel("ate-api-server replicas (8 CPUs each), size10 Postgres")
    axes[-1].set_xticks(range(2, 10))
    axes[-1].set_xlim(1.6, 9.4)
    fig.suptitle("size10: Postgres's WAL flush caps throughput at ~7,450 cycles/s", x=0.02, ha="left",
                 fontsize=13, fontweight="bold", color=TEXT)
    fig.text(0.02, 0.006, "Commits/s = fsyncs/s × commits per fsync; a cycle costs 24 commits.",
             color=TEXT2, fontsize=8)
    fig.tight_layout(rect=(0, 0.03, 1, 0.98), h_pad=1.5)
    fig.savefig(f"{OUT}/3-size10-scaling.png", dpi=200)


def percentiles():
    """Resume p50 / p90 / p95 / p99 against target rate, one small panel per
    setup on a shared log scale, the region past each ceiling shaded."""
    setups = [("size0", lab, steps(p), sat) for lab, p, sat in SIZE0]
    setups += [("size10", f"{n} × 8", steps(p), sat) for n, p, sat, _ in SIZE10]
    qs = [("resume_server_p50_ms", "p50"), ("resume_server_p90_ms", "p90"),
          ("resume_server_p95_ms", "p95"), ("resume_server_p99_ms", "p99")]
    fig = plt.figure(figsize=(14, 13.5))
    gs = fig.add_gridspec(4, 4, hspace=0.62, wspace=0.18, top=0.9, bottom=0.07, left=0.07, right=0.98)
    slots = [(0, 0), (0, 1), (0, 2), (0, 3), (1, 0), (1, 1)] + [(r, c) for r in (2, 3) for c in range(4)]
    rows, first = [], None
    for (pg, label, st, sat_from), (r, c) in zip(setups, slots):
        ax = fig.add_subplot(gs[r, c], sharey=first)
        first = first or ax
        sat = [d for d in st if d["target_rps"] >= sat_from]
        ceiling = sum(d["resume_rps"] for d in sat) / len(sat)
        xs = [d["target_rps"] for d in st]
        ax.axvspan(ceiling, xs[-1] * 1.05, color="#f3f2ef", zorder=0, linewidth=0)
        for (key, name), color in zip(qs, RAMP4):
            ys = [d[key] for d in st]
            ax.plot(xs, ys, color=color, linewidth=1.8, label=name, zorder=3)
            ax.plot(xs, ys, "o", markersize=3.5, color=color, markeredgecolor=SURFACE, markeredgewidth=0.8, zorder=4)
        ax.plot(xs, [d["resume_server_mean_ms"] for d in st], color=MEAN, linewidth=1.4, label="mean", zorder=5)
        rows += [{"postgres": pg, "setup": label, "ceiling_cycles_per_s": round(ceiling),
                  "target_cycles_per_s": round(d["target_rps"]), "achieved_cycles_per_s": round(d["resume_rps"], 1),
                  **{f"resume_server_{n}_ms": round(d[k]) for k, n in qs},
                  "resume_server_mean_ms": round(d["resume_server_mean_ms"], 1)} for d in st]
        ax.set_yscale("log")
        ax.set_xlim(xs[0] - (xs[-1] - xs[0]) * 0.05, xs[-1] * 1.05 - xs[0] * 0.05)
        ax.set_title(f"{label}  ·  ceiling ~{ceiling:,.0f}/s", loc="left", fontsize=10)
        ax.xaxis.set_major_formatter(compact)
        ax.tick_params(axis="x", labelsize=8)
        if c == 0:
            ax.set_ylabel("Resume server latency")
        else:
            ax.tick_params(axis="y", labelleft=False)
        if r in (1, 3) or (r == 0 and c in (2, 3)):
            ax.set_xlabel("Target cycles/s", fontsize=9)
    first.set_ylim(120, 10000)
    first.yaxis.set_major_locator(LogLocator(base=10, subs=(1, 2, 5)))
    first.yaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v / 1000:g} s" if v >= 1000 else f"{v:g} ms"))
    first.yaxis.set_minor_formatter(NullFormatter())

    key = fig.add_subplot(gs[1, 2:])
    key.axis("off")
    handles = [plt.Line2D([], [], color=c, linewidth=2.5, label=n) for (_, n), c in zip(qs, RAMP4)]
    handles.append(plt.Line2D([], [], color=MEAN, linewidth=1.6, label="mean (exact)"))
    handles.append(plt.Rectangle((0, 0), 1, 1, color="#f3f2ef", label="past the ceiling"))
    key.legend(handles=handles, loc="upper left", ncol=1, fontsize=10, title="Resume server latency", title_fontsize=10,
               alignment="left")
    key.text(0.42, 0.95, "Quantiles are interpolated within the server\nhistogram's doubling buckets (…, 102, 205,\n"
             "410, 819, 1638, 3277, 6554 ms), so flat runs\nat one value mean \"somewhere in that bucket\".\n\n"
             "Past the ceiling every user is busy, and\nlatency there is set by the user count.",
             transform=key.transAxes, va="top", color=TEXT2, fontsize=9)

    fig.text(0.07, 0.935, "size0 Postgres", fontsize=12, fontweight="bold", color=TEXT)
    fig.text(0.07, 0.475, "size10 Postgres, replicas × 8 CPUs", fontsize=12, fontweight="bold", color=TEXT)
    fig.text(0.36, 0.4765, "Past the ceiling at 3–6 × 8, resumes split: most stay near 0.2–0.3 s while the slowest "
             "take 3–5 s, so the median falls back\nwhile the mean (~0.9–1 s) and the tails rise. "
             "At 2 × 8 all percentiles rise together; at 7–9 × 8 the median settles at 0.4–1.2 s.",
             fontsize=9, color=TEXT2, va="center")
    fig.suptitle("Resume latency percentiles by setup: tails spread as each setup nears its ceiling",
                 x=0.01, y=0.985, ha="left", fontsize=13, fontweight="bold", color=TEXT)
    fig.text(0.01, 0.012, "One cycle = one cold resume + one pause; each point is a 3-minute ladder step. "
             "Setups are ate-api-server replicas × CPUs per replica; fake-atelet adds 150 ms to every resume.",
             color=TEXT2, fontsize=8)
    fig.savefig(f"{OUT}/4-resume-percentiles.png", dpi=200)
    write_csv("4-resume-percentiles.csv", rows)


def cpu():
    """ate-api-server and Postgres CPU used at each setup's ceiling, against
    each container's CPU limit (size10 Postgres has a request, no limit)."""
    rows = []
    for lab, path, sat_from in SIZE0:
        r, c = (int(v) for v in lab.split(" × "))
        rows.append(("size0", lab, steps(path), sat_from, r * c, 16, "limit"))
    for n, path, sat_from, _ in SIZE10:
        rows.append(("size10", f"{n} × 8", steps(path), sat_from, n * 8, 80, "request"))
    data = []
    for pg, lab, st, sat_from, ate_lim, pg_lim, pg_kind in rows:
        sat = [d for d in st if d["target_rps"] >= sat_from]
        mean = lambda k: sum(d[k] for d in sat) / len(sat)
        data.append({"postgres": pg, "setup": lab, "ceiling_cycles_per_s": round(mean("resume_rps")),
                     "ateapi_cores_used": round(mean("ateapi_cores"), 2), "ateapi_cpu_limit": ate_lim,
                     "postgres_cores_used": round(mean("postgres_cores"), 2),
                     "postgres_cpu_limit": pg_lim if pg_kind == "limit" else "",
                     "postgres_cpu_request": 2 if pg_kind == "limit" else pg_lim})
    write_csv("5-cpu-limit-usage.csv", data)

    # Rows top to bottom, with a gap between the two Postgres sizes.
    ys, y = [], 0
    for i, d in enumerate(data):
        if i and d["postgres"] != data[i - 1]["postgres"]:
            y += 0.8
        ys.append(y)
        y += 1
    fig, axes = plt.subplots(1, 2, figsize=(13, 7.4), sharey=True)
    panels = [
        (axes[0], "ate-api-server", "ateapi_cores_used", CATEGORICAL[0]),
        (axes[1], "Postgres", "postgres_cores_used", CATEGORICAL[1]),
    ]
    for ax, name, key, color in panels:
        for d, yy in zip(data, ys):
            used = d[key]
            if name == "ate-api-server":
                lim, kind = d["ateapi_cpu_limit"], "limit"
            else:
                lim, kind = (d["postgres_cpu_limit"], "limit") if d["postgres_cpu_limit"] != "" else (d["postgres_cpu_request"], "request")
            ax.barh(yy, used, height=0.56, color=color, zorder=3)
            hollow = kind == "request"
            ax.plot([lim, lim], [yy - 0.38, yy + 0.38], color=TEXT if not hollow else TEXT2, linewidth=2.2 if not hollow else 1.2,
                    linestyle="-", zorder=4, solid_capstyle="butt")
            if hollow:
                ax.plot([lim], [yy], marker="o", markersize=5, color=SURFACE, markeredgecolor=TEXT2, markeredgewidth=1.2, zorder=5)
            note = f"{used:.1f} / {lim:g} ({used / lim:.0%})" if kind == "limit" else f"{used:.1f}, no limit"
            ax.annotate(note, (max(used, lim if kind == "limit" else used), yy), xytext=(6, 0), textcoords="offset points",
                        va="center", ha="left", fontsize=8.5, color=TEXT)
        ax.set_title(f"{name} cores at the ceiling", loc="left")
        ax.set_xlim(0, 100)
        ax.set_xlabel("CPU cores")
        ax.grid(axis="y", visible=False)
    axes[0].set_yticks(ys, [f"{d['setup']}  ·  ~{d['ceiling_cycles_per_s']:,}/s" for d in data])
    axes[0].invert_yaxis()
    axes[0].tick_params(axis="y", length=0, labelsize=9)
    axes[1].tick_params(axis="y", length=0)
    for label, d in zip(axes[0].get_yticklabels(), data):
        label.set_color(TEXT)
    first0 = ys[0]; first10 = ys[[d["postgres"] for d in data].index("size10")]
    for ax in axes:
        ax.annotate("size0 Postgres", (0, first0 - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
        ax.annotate("size10 Postgres", (0, first10 - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
    key = [plt.Rectangle((0, 0), 1, 1, color=CATEGORICAL[0], label="ate-api-server cores used"),
           plt.Rectangle((0, 0), 1, 1, color=CATEGORICAL[1], label="Postgres cores used"),
           plt.Line2D([], [], color=TEXT, linewidth=2.2, label="CPU limit"),
           plt.Line2D([], [], color=TEXT2, linewidth=1.2, marker="o", markerfacecolor=SURFACE, markeredgecolor=TEXT2,
                      label="CPU request (size10 Postgres has no limit)")]
    axes[1].legend(handles=key, loc="upper right", fontsize=8.5, bbox_to_anchor=(1.0, 0.97))
    fig.suptitle("ate-api-server runs at its CPU limit up to 10 CPUs; past that, neither container is CPU-bound",
                 x=0.01, ha="left", fontsize=13, fontweight="bold", color=TEXT)
    fig.text(0.01, 0.006, "Cores used = the container's mean CPU over each ceiling's saturated steps, all replicas together. "
             "Setups are ate-api-server replicas × CPUs per replica, with the ceiling in cycles/s.\n"
             "ate-api-server's request equals its limit. size0 Postgres requests 2 cores with a 16-core limit; "
             "size10 Postgres requests 80 with no limit.",
             color=TEXT2, fontsize=8)
    fig.tight_layout(rect=(0, 0.04, 1, 0.96), w_pad=3)
    fig.savefig(f"{OUT}/5-cpu-limit-usage.png", dpi=200)


def connections():
    """Postgres active connections at each setup's ceiling, split by wait
    event (data/postgres-wait-events.csv, from fetch_wait_events.py),
    against the connections open."""
    with open(f"{OUT}/data/postgres-wait-events.csv") as f:
        waits = {(r["postgres"], r["setup"]): r for r in csv.DictReader(f)}
    rows = [("size0", lab, steps(p), sat, int(lab.split(" × ")[0])) for lab, p, sat in SIZE0]
    rows += [("size10", f"{n} × 8", steps(p), sat, n) for n, p, sat, _ in SIZE10]
    parts = [("walwrite", "waiting on the WAL flush (LWLock: WALWrite)"),
             ("lock", "waiting on a row lock (Lock: transactionid, tuple)"),
             ("clientread", "waiting on ate-api-server (Client: ClientRead)"),
             ("running", "running a query (no wait)"),
             ("other", "other waits")]
    data = []
    for pg, lab, st, sat_from, replicas in rows:
        sat = [d for d in st if d["target_rps"] >= sat_from]
        mean = lambda k: sum((d.get(k) or 0) for d in sat) / len(sat)
        w = waits[(pg, lab)]
        active = sum(float(w[k]) for k, _ in parts)
        data.append({"postgres": pg, "setup": lab, "ceiling_cycles_per_s": round(mean("resume_rps")),
                     "open_conns": round(mean("pg_conns"), 1), "active_conns": round(active, 1),
                     "active_per_replica": round(active / replicas, 1),
                     **{f"{k}_conns": round(float(w[k]), 1) for k, _ in parts}})
    write_csv("6-postgres-connections.csv", data)

    ys, y = [], 0
    for i, d in enumerate(data):
        if i and d["postgres"] != data[i - 1]["postgres"]:
            y += 0.8
        ys.append(y)
        y += 1
    fig, ax = plt.subplots(figsize=(11.5, 7.8))
    for d, yy in zip(data, ys):
        left = 0
        for (key, _), color in zip(parts, CATEGORICAL):
            v = d[f"{key}_conns"]
            ax.barh(yy, v, left=left, height=0.56, color=color, edgecolor=SURFACE, linewidth=1.5, zorder=3)
            left += v
        ax.plot([d["open_conns"]] * 2, [yy - 0.38, yy + 0.38], color=TEXT, linewidth=2.2, zorder=4, solid_capstyle="butt")
        ax.annotate(f"{d['active_conns']:.0f} active of {d['open_conns']:.0f} open · {d['active_per_replica']:.0f} per replica",
                    (max(left, d["open_conns"]), yy), xytext=(6, 0), textcoords="offset points",
                    va="center", ha="left", fontsize=8.5, color=TEXT)
    ax.set_yticks(ys, [f"{d['setup']}  ·  ~{d['ceiling_cycles_per_s']:,}/s" for d in data])
    ax.invert_yaxis()
    ax.tick_params(axis="y", length=0, labelsize=9)
    ax.grid(axis="y", visible=False)
    ax.set_xlim(0, 400)
    ax.set_xlabel("Postgres connections (mean over the ceiling's saturated steps)")
    first10 = ys[[d["postgres"] for d in data].index("size10")]
    ax.annotate("size0 Postgres", (0, ys[0] - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
    ax.annotate("size10 Postgres", (0, first10 - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
    handles = [plt.Rectangle((0, 0), 1, 1, color=c, label=l) for (_, l), c in zip(parts, CATEGORICAL)]
    handles.append(plt.Line2D([], [], color=TEXT, linewidth=2.2, label="connections open"))
    ax.legend(handles=handles, loc="upper right", fontsize=8.5, bbox_to_anchor=(1.0, 0.97))
    ax.set_title("Active Postgres connections at the ceiling, by wait event", loc="left")
    fig.suptitle("Active Postgres connections grow with replicas; lock waits grow from 5 × 8",
                 x=0.01, ha="left", fontsize=13, fontweight="bold", color=TEXT)
    fig.text(0.01, 0.006, "About half to two-thirds wait on the WAL flush in every setup, including the CPU-bound single replicas: "
             "a queue behind the flush is normal, and alone it does not mean the flush\nlimits throughput (the fsync busy "
             "share in the scaling chart does). Only 11–15% are running a query (23% at 1 × 2). Snapshots of pg_stat_activity every 15 s "
             "(postgres-exporter), averaged.",
             color=TEXT2, fontsize=8)
    fig.tight_layout(rect=(0, 0.045, 1, 0.965))
    fig.savefig(f"{OUT}/6-postgres-connections.png", dpi=200)


def memory():
    """ate-api-server and Postgres peak memory against each container's
    memory limit, as a share of the limit; labels carry the sizes."""
    rows = [("size0", lab, steps(p), 2048) for lab, p, _ in SIZE0]
    rows += [("size10", f"{n} × 8", steps(p), 140 * 1024) for n, p, _, _ in SIZE10]
    ATE_LIMIT = 4096
    data = []
    for pg, lab, st, pg_limit in rows:
        peak = lambda k: max(d[k] for d in st)
        data.append({"postgres": pg, "setup": lab,
                     "ateapi_ws_peak_mib_per_replica": round(peak("ateapi_mem_ws_max_mib")),
                     "ateapi_rss_peak_mib_per_replica": round(peak("ateapi_mem_rss_max_mib")),
                     "ateapi_limit_mib_per_replica": ATE_LIMIT,
                     "postgres_ws_peak_mib": round(peak("postgres_mem_ws_mib")),
                     "postgres_rss_peak_mib": round(peak("postgres_mem_rss_mib")),
                     "postgres_limit_mib": pg_limit})
    write_csv("7-memory-limit-usage.csv", data)

    def size(mib):
        return f"{mib / 1024:.1f} GiB" if mib >= 1024 else f"{mib:.0f} MiB"

    def limit(mib):
        return f"{mib / 1024:g} GiB"

    ys, y = [], 0
    for i, d in enumerate(data):
        if i and d["postgres"] != data[i - 1]["postgres"]:
            y += 0.8
        ys.append(y)
        y += 1
    fig, axes = plt.subplots(1, 2, figsize=(13, 7.4), sharey=True)
    panels = [
        (axes[0], "ate-api-server, largest replica", "ateapi_ws_peak_mib_per_replica", "ateapi_rss_peak_mib_per_replica",
         "ateapi_limit_mib_per_replica", CATEGORICAL[0]),
        (axes[1], "Postgres", "postgres_ws_peak_mib", "postgres_rss_peak_mib", "postgres_limit_mib", CATEGORICAL[1]),
    ]
    for ax, title, ws_key, rss_key, lim_key, color in panels:
        for d, yy in zip(data, ys):
            ws, rss, lim = d[ws_key], d[rss_key], d[lim_key]
            ax.barh(yy, 100 * ws / lim, height=0.56, color=color, zorder=3)
            ax.plot([100 * rss / lim], [yy], marker="o", markersize=6, color=SURFACE, markeredgecolor=TEXT,
                    markeredgewidth=1.4, zorder=5)
            ax.annotate(f"{size(ws)} / {limit(lim)} ({ws / lim:.0%})", (100 * max(ws, rss) / lim, yy), xytext=(8, 0),
                        textcoords="offset points", va="center", ha="left", fontsize=8.5, color=TEXT)
        ax.axvline(100, color=TEXT, linewidth=2.2, zorder=4)
        ax.set_title(f"{title}: peak over each run, share of limit", loc="left")
        ax.set_xlim(0, 112)
        ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:.0f}%"))
        ax.set_xlabel("Share of the container's memory limit")
        ax.grid(axis="y", visible=False)
    axes[0].set_yticks(ys, [d["setup"] for d in data])
    axes[0].invert_yaxis()
    axes[0].tick_params(axis="y", length=0, labelsize=9.5)
    axes[1].tick_params(axis="y", length=0)
    first10 = ys[[d["postgres"] for d in data].index("size10")]
    for ax in axes:
        ax.annotate("size0 Postgres", (0, ys[0] - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
        ax.annotate("size10 Postgres", (0, first10 - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
    key = [plt.Rectangle((0, 0), 1, 1, color=CATEGORICAL[0], label="ate-api-server working set, peak"),
           plt.Rectangle((0, 0), 1, 1, color=CATEGORICAL[1], label="Postgres working set, peak"),
           plt.Line2D([], [], linestyle="none", marker="o", markersize=6, markerfacecolor=SURFACE, markeredgecolor=TEXT,
                      markeredgewidth=1.4, label="resident memory (RSS), peak"),
           plt.Line2D([], [], color=TEXT, linewidth=2.2, label="memory limit")]
    axes[0].legend(handles=key, loc="center", fontsize=9, bbox_to_anchor=(0.62, 0.5))
    fig.suptitle("Memory limits nothing: ate-api-server peaks at 15% of its limit, Postgres at 31%",
                 x=0.01, ha="left", fontsize=13, fontweight="bold", color=TEXT)
    fig.text(0.01, 0.006, "Peaks over each whole run, from cAdvisor (the CPU chart uses each ceiling's steps). Working set counts toward the limit, "
             "but its page-cache part can be reclaimed before an OOM kill; Postgres's shared buffers cannot.\nPostgres's working set "
             "is mostly shared buffers and page cache, which keep growing through a run even on steps where throughput is flat; its RSS stays small.\n"
             "Limits: ate-api-server 4 GiB per replica (request = limit); Postgres 2 GiB on size0 (1 GiB request), "
             "140 GiB on size10.",
             color=TEXT2, fontsize=8)
    fig.tight_layout(rect=(0, 0.06, 1, 0.965), w_pad=3)
    fig.savefig(f"{OUT}/7-memory-limit-usage.png", dpi=200)


def cost():
    """ate-api-server and Postgres CPU per cycle at each setup's ceiling,
    and at 5000/s where the ladder held that step."""
    rows = [("size0", lab, steps(p), sat) for lab, p, sat in SIZE0]
    rows += [("size10", f"{n} × 8", steps(p), sat) for n, p, sat, _ in SIZE10]
    data = []
    for pg, lab, st, sat_from in rows:
        sat = [d for d in st if d["target_rps"] >= sat_from]
        per = lambda d, k: 1000 * d[k] / d["resume_rps"]
        at = [d for d in st if int(d["target_rps"]) == 5000 and abs(d["resume_rps"] - 5000) < 100]
        ceiling = sum(d["resume_rps"] for d in sat) / len(sat)
        # Plot the 5000/s point only where it sits clearly below the ceiling:
        # at the knee, saturation itself moves the cost.
        below = bool(at) and 5000 <= 0.9 * ceiling
        data.append({"postgres": pg, "setup": lab, "ceiling_cycles_per_s": round(ceiling),
                     "share_of_ceiling_at_5000": round(5000 / ceiling, 2) if at else "",
                     "plotted_at_5000": below,
                     "ateapi_ms_per_cycle_at_ceiling": round(sum(per(d, "ateapi_cores") for d in sat) / len(sat), 2),
                     "postgres_ms_per_cycle_at_ceiling": round(sum(per(d, "postgres_cores") for d in sat) / len(sat), 2),
                     "ateapi_ms_per_cycle_at_5000": round(per(at[0], "ateapi_cores"), 2) if at else "",
                     "postgres_ms_per_cycle_at_5000": round(per(at[0], "postgres_cores"), 2) if at else ""})
    write_csv("8-cpu-per-cycle.csv", data)

    ys, y = [], 0
    for i, d in enumerate(data):
        if i and d["postgres"] != data[i - 1]["postgres"]:
            y += 0.8
        ys.append(y)
        y += 1
    fig, axes = plt.subplots(1, 2, figsize=(13, 7.4), sharey=True, sharex=True)
    panels = [(axes[0], "ate-api-server", "ateapi", CATEGORICAL[0]), (axes[1], "Postgres", "postgres", CATEGORICAL[1])]
    for ax, name, key, color in panels:
        for d, yy in zip(data, ys):
            v = d[f"{key}_ms_per_cycle_at_ceiling"]
            ax.barh(yy, v, height=0.56, color=color, zorder=3)
            v5 = d[f"{key}_ms_per_cycle_at_5000"] if d["plotted_at_5000"] else ""
            if v5 != "":
                ax.plot([v5], [yy], marker="o", markersize=6, color=SURFACE, markeredgecolor=TEXT, markeredgewidth=1.4, zorder=5)
            ax.annotate(f"{v:.2f} ms" + (f"  ·  {v5:.2f} at 5,000/s" if v5 != "" else ""), (max(v, v5 or 0), yy),
                        xytext=(8, 0), textcoords="offset points", va="center", ha="left", fontsize=8.5, color=TEXT)
        ax.set_title(f"{name} CPU per cycle", loc="left")
        ax.set_xlim(0, 9)
        ax.xaxis.set_major_formatter(FuncFormatter(lambda v, _: f"{v:g} ms"))
        ax.set_xlabel("CPU time per cycle (one cold resume + one pause)")
        ax.grid(axis="y", visible=False)
    axes[0].set_yticks(ys, [f"{d['setup']}  ·  ~{d['ceiling_cycles_per_s']:,}/s" for d in data])
    axes[0].invert_yaxis()
    axes[0].tick_params(axis="y", length=0, labelsize=9)
    axes[1].tick_params(axis="y", length=0)
    first10 = ys[[d["postgres"] for d in data].index("size10")]
    for ax in axes:
        ax.annotate("size0 Postgres", (0, ys[0] - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
        ax.annotate("size10 Postgres", (0, first10 - 0.75), fontsize=9.5, fontweight="bold", color=TEXT2, annotation_clip=False)
    key = [plt.Rectangle((0, 0), 1, 1, color=CATEGORICAL[0], label="ate-api-server, at the ceiling"),
           plt.Rectangle((0, 0), 1, 1, color=CATEGORICAL[1], label="Postgres, at the ceiling"),
           plt.Line2D([], [], linestyle="none", marker="o", markersize=6, markerfacecolor=SURFACE, markeredgecolor=TEXT,
                      markeredgewidth=1.4, label="at 5,000/s (67–84% of the ceiling)")]
    axes[1].legend(handles=key, loc="upper right", fontsize=8.5, bbox_to_anchor=(1.02, 0.9))
    fig.suptitle("ate-api-server's CPU per cycle grows with replicas; Postgres's grows with the rate",
                 x=0.01, ha="left", fontsize=13, fontweight="bold", color=TEXT)
    fig.text(0.01, 0.006, "CPU per cycle = the container's cores over a step divided by its cold resumes per second, all replicas together; "
             "bars average each ceiling's saturated steps. At the ceiling it rises\nfrom 2.5 to 6.1 ms for ate-api-server and "
             "from 2.7 to 4.5 ms for Postgres. At the same 5,000/s, ate-api-server's still rises with each replica (4.6 → 5.7 ms from "
             "4 to 9 × 8) while Postgres's stays at 3.6–4.0 ms.\nThe dots hold the rate constant, not the load: 5,000/s is 84% of 4 × 8's "
             "ceiling and 67% of 9 × 8's. 3 × 8's 5,000/s step was at its ceiling and is left out; the size0 setups and size10 2 × 8 "
             "never reach 5,000/s.",
             color=TEXT2, fontsize=8)
    fig.tight_layout(rect=(0, 0.06, 1, 0.965), w_pad=3)
    fig.savefig(f"{OUT}/8-cpu-per-cycle.png", dpi=200)


def write_csv(name, rows):
    with open(f"{OUT}/{name}", "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0]))
        w.writeheader()
        w.writerows(rows)


if __name__ == "__main__":
    knee()
    scaling()
    percentiles()
    cpu()
    connections()
    memory()
    cost()
