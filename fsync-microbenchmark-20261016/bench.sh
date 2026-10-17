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


# Single-stream write+fdatasync latency on each target, alone and with
# competing random writes. Results as JSON under /out.
set -u
apk add --no-cache fio >/dev/null
mkdir -p /out
run() {  # name target kind(fs|block)
  name=$1; t=$2; kind=$3
  if [ "$kind" = fs ]; then
    f="$t/walfile"; bg="$t/datafile"
    echo "== $name pg_test_fsync"; pg_test_fsync -f "$t/pgtf" -s 10 > /out/$name-pg_test_fsync.txt 2>&1; rm -f "$t/pgtf"
    extra="--size=8G"
  else
    f="$t"; bg="$t"; extra="--size=8G --offset=0"
  fi
  echo "== $name fio alone"
  fio --name=wal --filename="$f" --rw=write --bs=32k --ioengine=sync --iodepth=1 --fdatasync=1 \
      --runtime=30 --time_based $extra --output-format=json --output=/out/$name-alone.json
  echo "== $name fio with background random writes"
  if [ "$kind" = fs ]; then bgsize="--size=16G"; else bgsize="--size=16G --offset=100G"; fi
  fio --output-format=json --output=/out/$name-contended.json \
      --name=wal --filename="$f" --rw=write --bs=32k --ioengine=sync --iodepth=1 --fdatasync=1 \
        --runtime=30 --time_based $extra \
      --name=data --filename="$bg" --rw=randwrite --bs=8k --ioengine=libaio --iodepth=8 --direct=1 \
        --rate=80m --runtime=30 --time_based $bgsize
  rm -f "$t/walfile" "$t/datafile" 2>/dev/null
}
run tmpfs /mnt/tmpfs fs
run pd-balanced-ext4 /mnt/pdb fs
run pd-balanced-rawblock /dev/pdb-block block
run pd-ssd-ext4 /mnt/pdssd fs
run hyperdisk-balanced-ext4 /mnt/hdb fs
mount | grep -E "/mnt/(pdb|pdssd|hdb)" > /out/mounts.txt
for f in /out/*; do echo "### FILE $(basename $f)"; cat "$f"; echo; done
echo "### END"
echo DONE
sleep 3600
