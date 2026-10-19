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


"""Per phase of writer.sh: sampled fdatasyncs from trace.bt's output.

Usage: python3 analyze.py results/trace.txt

Each sampled line is assigned to the 10 s interval whose header follows it,
and each interval to the phase running at its start. The dd phases last a few
seconds each, shorter than an interval, so they are pooled and told apart by
write size: 8 KiB (dd-8k-same), 32 KiB (dd-aligned-32k), larger (dd-pg-like).
Completions with no sectors (a flush's original request) are not data writes.
"""
import datetime as dt
import re
import statistics as st
import sys


def ts(s):
    return dt.datetime.strptime(s, '%Y-%m-%dT%H:%M:%SZ').timestamp()


def report(name, fds):
    lat = sorted(x[0] for f in fds for x in f['w'])
    kib = [x[1] for f in fds for x in f['w']]
    dur = sorted(f['dur'] for f in fds)
    q = lambda a, p: a[int(p * (len(a) - 1))]
    print(f"{name} | {len(fds)} | {st.mean(len(f['w']) for f in fds):.2f} | {st.mean(kib):.1f} | "
          f"{st.mean(lat):.0f} / {q(lat, .5)} / {q(lat, .9)} | {st.mean(dur):.0f} / {q(dur, .5)} | "
          f"{sum(f['splits'] for f in fds)}")


lines = open(sys.argv[1]).read().splitlines()
phases = []
for l in lines[lines.index('### PHASES') + 1:]:
    if l == '### END':
        break
    t, name = l.split(' ', 1)
    phases.append((ts(t), name.replace(' start', '')))


def phase(t):
    cur = 'setup'
    for pt, name in phases:
        if t - 10 >= pt:
            cur = name
    return cur


pending, by = [], {}
for l in lines:
    if l.startswith('=== '):
        by.setdefault(phase(ts(l[4:])), []).extend(pending)
        pending = []
    elif re.match(r'(S|I|C|E|Q|SPLIT|WP|WPR) ', l):
        pending.append(l)

print('phase | sampled fdatasyncs | data writes per fdatasync | KiB per write | '
      'write latency mean / p50 / p90 us | fdatasync mean / p50 us | splits')
for name, ls in by.items():
    if name in ('setup', 'pgbench end'):
        continue
    fds, cur = [], None
    for l in ls:
        if l.startswith('S '):
            cur = {'w': [], 'splits': 0}
        elif cur is None:
            continue
        elif l.startswith('C ') and 'rwbs=WS' in l and not l.endswith(' n=0'):
            m = re.search(r'lat=(\d+) .* n=(\d+)', l)
            cur['w'].append((int(m.group(1)), int(m.group(2)) / 2))
        elif l.startswith('SPLIT'):
            cur['splits'] += 1
        elif l.startswith('E '):
            cur['dur'] = int(re.search(r'dur=(\d+)', l).group(1))
            fds.append(cur)
            cur = None
    if not fds:
        continue
    if name.startswith('dd') or name == 'done':
        groups = {}
        for f in fds:
            k = f['w'][0][1] if f['w'] else 0
            groups.setdefault('dd-8k-same' if k <= 8 else 'dd-aligned-32k' if k == 32 else 'dd-pg-like', []).append(f)
    else:
        groups = {name: fds}
    for g, gf in groups.items():
        report(g, gf)
