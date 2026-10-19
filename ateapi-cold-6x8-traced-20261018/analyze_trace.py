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


"""Per ladder step: kernel trace (trace.txt), pg_stat_io + diskstats (io.jsonl) and commits.

Usage: python3 analyze_trace.py <data-dir>
Flush requests are not in the block histograms: they issue at sector 0 and complete at
sector 2^64-1, so the sector-keyed pairing never matches them; diskstats supplies flush counts
and time instead. Other block latency comes from the histograms (bucket geometric midpoints, buckets >= 1 s and
negative-delta buckets dropped) because the sector-keyed issue/complete pairing occasionally
mismatches and poisons @blk_ns.
"""
import json, re, sys, datetime as dt, math
W = sys.argv[1]
CODES = {1: 'FF', 2: 'WFSM', 3: 'WSM', 4: 'WS', 5: 'WM', 6: 'W', 7: 'WFS', 8: 'FWS', 9: 'FWFS', 10: 'R', 11: 'RM', 12: 'RA', 99: 'other'}
def ts(s): return dt.datetime.strptime(s, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=dt.timezone.utc).timestamp()
UNIT = {'': 1, 'K': 1024, 'M': 1024**2}
def bval(s):
    m = re.match(r'(\d+)([KM]?)', s); return int(m.group(1)) * UNIT[m.group(2)]

# Parse intervals.
ivs, cur, hist = [], None, None
for line in open(f'{W}/trace.txt'):
    line = line.rstrip('\n')
    if line.startswith('=== '):
        if line.startswith('=== END'): cur = None; continue
        cur = {'t': ts(line[4:]), 'scalars': {}, 'hists': {}}; ivs.append(cur); hist = None; continue
    if cur is None: continue
    m = re.match(r'@(\w+)(?:\[(\d+)\])?: ?(\d*)$', line)
    if m:
        name, key, val = m.groups()
        k = (name, CODES.get(int(key), key) if key else None)
        if val: cur['scalars'][k] = int(val); hist = None
        else: hist = cur['hists'].setdefault(k, []); continue
        continue
    m = re.match(r'\[([\dKM]+), ([\dKM]+)\)\s+(\d+)', line)
    if m and hist is not None:
        lo, hi = bval(m.group(1)), bval(m.group(2)); hist.append((lo, hi, int(m.group(3))))
def hstats(h):
    pts = [(math.sqrt(lo * hi) if lo else hi / 2, n) for lo, hi, n in h if hi <= 1_000_000]
    n = sum(c for _, c in pts)
    if not n: return 0, None, None
    mean = sum(v * c for v, c in pts) / n
    acc, p50 = 0, None
    for lo, hi, c in h:
        acc += c
        if acc >= n / 2: p50 = f'{lo}-{hi}'; break
    return n, mean, p50

steps = [json.loads(l) for l in open(f'{W}/steps.jsonl')]
io = []
for l in open(f'{W}/io.jsonl'):
    try: io.append(json.loads(l))
    except ValueError: pass  # samples taken while Postgres was shutting down
def ds(rec):
    f = rec['diskstats'].split()
    # fields after the name: reads, rmerged, rsect, rms, writes, wmerged, wsect, wms, inflight, ioms, weighted, 4 discard fields, flushes, flush ms
    v = list(map(int, f[3:]))
    return {'w': v[4], 'wsect': v[6], 'wms': v[7], 'ioms': v[9], 'fl': v[15], 'flms': v[16]}
def pgio(rec):
    out = {}
    for r in rec['pg']['io']:
        key = (r['bt'], r['obj'], r['ctx'])
        out[key] = r
    return out
def pick(t0, t1):
    a = [r for r in io if t0 <= r['pg']['ts'] <= t1]
    return (a[0], a[-1]) if len(a) >= 2 else (None, None)

print('step target  rps  | fdatasync/s  mean_us | per-type: count/s, mean_us (from hist), KiB/req | jbd2/s | diskstats: w/s MiB/s flush/s flush_ms | pg wal-init/s datafile writes/s')
rows = []
for s in steps:
    t0, t1 = s['window']['start'] if isinstance(s['window'], dict) else s['window'][0], s['window']['end'] if isinstance(s['window'], dict) else s['window'][1]
    if isinstance(t0, str): t0, t1 = ts(t0[:19] + 'Z'), ts(t1[:19] + 'Z')
    sel = [iv for iv in ivs if t0 + 30 <= iv['t'] <= t1]
    secs = 30 * len(sel)
    if not secs: continue
    agg = lambda name, key=None: sum(iv['scalars'].get((name, key), 0) for iv in sel)
    fds_n, fds_ns = agg('fdatasync_n'), agg('fdatasync_ns')
    per = {}
    for code in CODES.values():
        hs = [b for iv in sel for b in iv['hists'].get(('blk_us', code), [])]
        cnt = agg('blk_n', code)
        if not cnt: continue
        merged = {}
        for lo, hi, c in hs: merged[(lo, hi)] = merged.get((lo, hi), 0) + c
        n, mean, p50 = hstats([(lo, hi, c) for (lo, hi), c in sorted(merged.items())])
        per[code] = (cnt / secs, mean, agg('blk_sectors', code) * 512 / 1024 / cnt, p50)
    a, b = pick(t0, t1)
    dsr = pgr = None
    if a:
        da, db, dtm = ds(a), ds(b), b['pg']['ts'] - a['pg']['ts']
        dsr = {k: (db[k] - da[k]) / dtm for k in da}
        pa, pb = pgio(a), pgio(b)
        def rate(key, f): return ((pb.get(key, {}).get(f) or 0) - (pa.get(key, {}).get(f) or 0)) / dtm
        init_w = rate(('client backend', 'wal', 'init'), 'writes')
        rel_w = sum(((pb[k].get('writes') or 0) - (pa.get(k, {}).get('writes') or 0)) / dtm for k in pb if k[1] == 'relation')
        rel_wb = sum(((pb[k].get('writebacks') or 0) - (pa.get(k, {}).get('writebacks') or 0)) / dtm for k in pb if k[1] == 'relation')
        ck = (b['pg']['ckpt']['num_timed'] + b['pg']['ckpt']['num_requested']) - (a['pg']['ckpt']['num_timed'] + a['pg']['ckpt']['num_requested'])
        pgr = (init_w, rel_w, rel_wb, ck)
    row = dict(step=s['step'], target=s['target_rps'], rps=s['resume_rps'], commits=s['pg_commits_per_s'],
               fdatasync_s=fds_n / secs, fdatasync_mean_us=fds_ns / fds_n / 1000 if fds_n else None,
               fsync_s=agg('fsync_n') / secs, jbd2_s=agg('jbd2_n') / secs,
               jbd2_mean_us=agg('jbd2_ns') / agg('jbd2_n') / 1000 if agg('jbd2_n') else None,
               blk={k: dict(per_s=round(v[0], 1), mean_us=round(v[1], 1) if v[1] else None, kib=round(v[2], 1), p50_us=v[3]) for k, v in per.items()},
               disk={k: round(v, 2) for k, v in dsr.items()} if dsr else None,
               pg=dict(wal_init_per_s=round(pgr[0], 2), rel_writes_per_s=round(pgr[1], 1), rel_writebacks_per_s=round(pgr[2], 1), checkpoints=pgr[3]) if pgr else None)
    rows.append(row)
    print(json.dumps(row))
