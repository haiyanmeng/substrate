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


"""Per-step load, WAL flush and diskstats for one run directory.

/proc/diskstats counts each flush's empty request as a write: writes/s here
subtract flushes, and the mean write subtracts their time (the flush time).
The mean write is printed only for a volume holding the WAL alone; on a
shared volume data-file writeback dominates it.
"""
import csv, json, sys
D = sys.argv[1]
w = [r for r in csv.DictReader(open(f'{D}/wal-io.csv')) if r['wal_fsyncs']]
io = []
for l in open(f'{D}/io.jsonl'):
    try: io.append(json.loads(l))
    except ValueError: pass
walvol = bool(io and io[-1].get('wal_diskstats'))
def ds(s):
    v = list(map(int, s.split()[3:])); return dict(w=v[4], wsect=v[6], wms=v[7], ioms=v[9], fl=v[15], flms=v[16])
print('target resume/s mean_ms p50/p99 commits/s fsyncs/s fsync_ms commits/fsync busy | WALdisk: writes/s MiB/s mean_write flush/s flush_ms busy | DATAdisk: writes/s MiB/s busy')
for x in map(json.loads, open(f'{D}/steps.jsonl')):
    t0, t1 = x['window']; a = [r for r in w if t0 <= int(r['ts']) <= t1]
    y, z = a[0], a[-1]; d = lambda k: float(z[k]) - float(y[k]); dt = d('ts')
    b = [r for r in io if t0 <= r['pg']['ts'] <= t1]; T = b[-1]['pg']['ts'] - b[0]['pg']['ts']
    def disk(key):
        p, q = ds(b[0][key]), ds(b[-1][key]); e = {k: q[k] - p[k] for k in p}; n = e['w'] - e['fl']
        return e, n
    row = f"{int(x['target_rps'])} {x['resume_rps']:.0f} {x['resume_server_mean_ms']:.0f} {x['resume_server_p50_ms']:.0f}/{x['resume_server_p99_ms']:.0f} {x['pg_commits_per_s']:.0f} {d('wal_fsyncs')/dt:.0f} {d('wal_fsync_ms')/d('wal_fsyncs'):.3f} {d('xact_commit')/d('wal_fsyncs'):.0f} {d('wal_fsync_ms')/dt/10:.0f}%"
    if walvol:
        e, n = disk('wal_diskstats'); f, m = disk('diskstats')
        row += f" | {n/T:.0f} {e['wsect']*512/2**20/T:.1f} {(e['wms']-e['flms'])/n:.3f} {e['fl']/T:.0f} {e['flms']/max(e['fl'],1):.3f} {e['ioms']/T/10:.0f}% | {m/T:.0f} {f['wsect']*512/2**20/T:.1f} {f['ioms']/T/10:.0f}%"
    else:
        e, n = disk('diskstats')
        row += f" | shared: {n/T:.0f} {e['wsect']*512/2**20/T:.1f} - {e['fl']/T:.0f} {e['flms']/max(e['fl'],1):.3f} {e['ioms']/T/10:.0f}%"
    print(row, x['failures'] or '')
