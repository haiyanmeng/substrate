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


# Prints the volume's queue limits, then runs trace.bt on it until the writer
# touches /shared/done.
set -u
mount -t tracefs tracefs /sys/kernel/tracing 2>/dev/null
dev=$(awk '$5=="/data"{print $3}' /proc/self/mountinfo)
name=$(basename "$(readlink -f /sys/dev/block/$dev)")
echo "device $dev ($name)"
for f in logical_block_size physical_block_size minimum_io_size optimal_io_size max_sectors_kb max_hw_sectors_kb \
         max_segments max_segment_size chunk_sectors virt_boundary_mask write_cache fua nr_requests scheduler rotational; do
  echo "queue/$f: $(cat /sys/block/$name/queue/$f 2>/dev/null)"
done
devt=$(( ${dev%:*} << 20 | ${dev#*:} ))
bpftrace /scripts/trace.bt $devt &
bt=$!
sleep 15; touch /shared/tracer-ready
until [ -e /shared/done ]; do sleep 2; done
sleep 12; kill -INT $bt; wait $bt
echo "### PHASES"; cat /shared/phases.txt
echo "### END"
