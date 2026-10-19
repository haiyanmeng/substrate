#!/usr/bin/env bash

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

# Every 15 s, one JSON line: pg_stat_io (all rows with writes or fsyncs),
# pg_stat_checkpointer, pg_stat_wal, commits, and the /proc/diskstats line
# of the device holding Postgres's data directory, until stop-sampler exists.
out="$1/io.jsonl"; stop="$1/stop-sampler"; errs="$1/io-sampler-errors.log"
ctx=gke_haiyanmeng-gke-dev_us-central1-a_nh-prow-parity
q="SELECT json_build_object('ts', extract(epoch from now())::bigint,
 'io', (SELECT json_agg(json_build_object('bt',backend_type,'obj',object,'ctx',context,'writes',writes,
        'write_bytes',write_bytes,'write_time',write_time,'fsyncs',fsyncs,'fsync_time',fsync_time,
        'writebacks',writebacks,'writeback_time',writeback_time,'extends',extends,'extend_time',extend_time))
        FROM pg_stat_io WHERE coalesce(writes,0)+coalesce(fsyncs,0)+coalesce(writebacks,0)+coalesce(extends,0)>0),
 'ckpt', (SELECT row_to_json(c) FROM pg_stat_checkpointer c),
 'wal', (SELECT row_to_json(w) FROM pg_stat_wal w),
 'commits', (SELECT xact_commit FROM pg_stat_database WHERE datname='atepg'))"
script='dev=$(awk '"'"'$5=="/var/lib/postgresql/data"{print $3}'"'"' /proc/self/mountinfo); maj=${dev%:*}; min=${dev#*:}; ds=$(awk -v a=$maj -v b=$min '"'"'$1==a && $2==b'"'"' /proc/diskstats); j=$(psql -U postgres -d atepg -At -c "$Q"); printf "{\"pg\":%s,\"dev\":\"%s\",\"diskstats\":\"%s\"}\n" "$j" "$dev" "$ds"'
while [ ! -e "$stop" ]; do
  if ! res=$(timeout 30 kubectl --context $ctx -n ate-system exec postgres-0 -c postgres -- env Q="$q" sh -c "$script" 2>&1); then
    echo "$(date -u +%FT%TZ) ${res:0:300}" >> "$errs"
  else
    echo "$res" >> "$out"
  fi
  sleep 15
done
