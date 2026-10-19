#!/bin/sh
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


# Drives the write patterns whose block requests tracer.sh records. Each
# phase is logged to /shared/phases.txt with its UTC start time.
set -eu
apk add --no-cache coreutils >/dev/null
mark() { echo "$(date -u +%FT%TZ) $*" | tee -a /shared/phases.txt; }
until [ -e /shared/tracer-ready ]; do sleep 1; done
PG=/data/pg
mkdir -p $PG && chown postgres $PG
su postgres -c "initdb -D $PG/db" >/dev/null
su postgres -c "pg_ctl -D $PG/db -l $PG/log -w start -o '-c shared_buffers=2GB -c max_connections=200 -c max_wal_size=16GB'"
su postgres -c "pgbench -i -q -s 50 postgres" >/dev/null 2>&1
su postgres -c "psql -qc CHECKPOINT postgres"
mark "pgbench-c64 start"   # group commit, like ate-api-server's many connections
su postgres -c "pgbench -N -c 64 -j 8 -T 60 -P 10 postgres" 2>&1 | tail -12
mark "pgbench-c8 start"    # fewer commits per flush
su postgres -c "pgbench -N -c 8 -j 4 -T 30 -P 10 postgres" 2>&1 | tail -10
mark "pgbench end"
su postgres -c "pg_ctl -D $PG/db -m fast stop" >/dev/null

# Fixed patterns on a preallocated, flushed 16 MiB file, as a WAL segment is.
f=/data/seg
dd if=/dev/zero of=$f bs=1M count=16 conv=fsync 2>/dev/null
i=0; mark "dd-aligned-32k start"   # 32 KiB at 32 KiB-aligned offsets
while [ $i -lt 2000 ]; do dd if=/dev/zero of=$f bs=32k count=1 seek=$((i % 512)) conv=notrunc,fdatasync 2>/dev/null; i=$((i+1)); done
i=0; end=0; mark "dd-pg-like start"   # rewrite the last partial 8 KiB page, then 29 KiB more, as XLogWrite does
while [ $i -lt 2000 ]; do
  start=$(( end / 16 * 16 )); end=$(( end + 58 )); [ $end -ge 32768 ] && { start=0; end=58; }
  dd if=/dev/zero of=$f bs=512 count=$((end - start)) seek=$start conv=notrunc,fdatasync 2>/dev/null; i=$((i+1)); done
i=0; mark "dd-8k-same start"   # 8 KiB at one offset, as pg_test_fsync does
while [ $i -lt 2000 ]; do dd if=/dev/zero of=$f bs=8k count=1 seek=0 conv=notrunc,fdatasync 2>/dev/null; i=$((i+1)); done
mark "done"
touch /shared/done
sleep 600
