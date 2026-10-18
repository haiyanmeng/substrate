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


# Single-stream write+fdatasync latency on each ext4 target given as
# name:mountpath, alone and with competing random writes. Every result
# file is printed to stdout after "### FILE <name>", so the pod's log
# keeps the results after the pod exits.
set -u
apk add --no-cache fio >/dev/null
mkdir -p /out
for spec in "$@"; do
  name=${spec%%:*}; t=${spec#*:}
  echo "== $(date -u +%FT%TZ) $name pg_test_fsync"
  pg_test_fsync -f "$t/pgtf" -s 10 > /out/$name-pg_test_fsync.txt 2>&1; rm -f "$t/pgtf"
  echo "== $(date -u +%FT%TZ) $name fio alone"
  fio --name=wal --filename="$t/walfile" --rw=write --bs=32k --ioengine=sync --iodepth=1 --fdatasync=1 \
      --runtime=30 --time_based --size=8G --output-format=json --output=/out/$name-alone.json
  echo "== $(date -u +%FT%TZ) $name fio with background random writes"
  fio --output-format=json --output=/out/$name-contended.json \
      --name=wal --filename="$t/walfile" --rw=write --bs=32k --ioengine=sync --iodepth=1 --fdatasync=1 \
        --runtime=30 --time_based --size=8G \
      --name=data --filename="$t/datafile" --rw=randwrite --bs=8k --ioengine=libaio --iodepth=8 --direct=1 \
        --rate=80m --runtime=30 --time_based --size=16G
  rm -f "$t/walfile" "$t/datafile"
done
mount | grep " /mnt/" > /out/mounts.txt
for f in /out/*; do echo "### FILE $(basename $f)"; cat "$f"; echo; done
echo "### END"
sleep 3600
