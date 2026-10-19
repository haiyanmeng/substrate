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

# Samples Postgres's WAL I/O, commit and wait counters every 15 s into
# wal-io.csv until the file named stop-sampler exists. Counters are
# cumulative since the last Postgres start; diff consecutive rows for rates.
# Failed samples are logged with a timestamp to sampler-errors.log.
out="$1/wal-io.csv"; stop="$1/stop-sampler"; errs="$1/sampler-errors.log"
ctx=gke_haiyanmeng-gke-dev_us-central1-a_nh-prow-parity
echo "ts,timing,wal_writes,wal_write_bytes,wal_write_ms,wal_fsyncs,wal_fsync_ms,wal_bytes,wal_buffers_full,xact_commit,walwrite_waiters,lock_waiters,active" > "$out"
q="SELECT extract(epoch from now())::bigint, current_setting('track_wal_io_timing'),
  (SELECT sum(writes) FROM pg_stat_io WHERE object='wal'),
  (SELECT sum(write_bytes) FROM pg_stat_io WHERE object='wal'),
  (SELECT round(sum(write_time)::numeric,1) FROM pg_stat_io WHERE object='wal'),
  (SELECT sum(fsyncs) FROM pg_stat_io WHERE object='wal'),
  (SELECT round(sum(fsync_time)::numeric,1) FROM pg_stat_io WHERE object='wal'),
  (SELECT wal_bytes FROM pg_stat_wal), (SELECT wal_buffers_full FROM pg_stat_wal),
  (SELECT xact_commit FROM pg_stat_database WHERE datname='atepg'),
  (SELECT count(*) FROM pg_stat_activity WHERE wait_event='WALWrite'),
  (SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock'),
  (SELECT count(*) FROM pg_stat_activity WHERE state='active' AND datname='atepg')"
while [ ! -e "$stop" ]; do
  if ! res=$(timeout 30 kubectl --context $ctx -n ate-system exec postgres-0 -c postgres -- \
      psql -U postgres -d atepg -At -F, -c "$q" 2>&1); then
    echo "$(date -u +%FT%TZ) ${res:0:300}" >> "$errs"
  else
    echo "$res" >> "$out"
  fi
  sleep 15
done
