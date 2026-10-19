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

# Waits for postgres-0's container to be ready, then records its node, its
# volume claims and each disk's type and size into $1/postgres-disks.txt.
out="$1/postgres-disks.txt"; ctx=gke_haiyanmeng-gke-dev_us-central1-a_nh-prow-parity; k="kubectl --context $ctx"
until [ "$($k -n ate-system get pod postgres-0 -o jsonpath='{.status.containerStatuses[?(@.name=="postgres")].ready}' 2>/dev/null)" = true ]; do
  [ -e "$1/stop-sampler" ] && exit 1; sleep 15; done
{ echo "node $($k -n ate-system get pod postgres-0 -o jsonpath='{.spec.nodeName}')"
  for pvc in $($k -n ate-system get pvc -o name); do
    pv=$($k -n ate-system get $pvc -o jsonpath='{.spec.volumeName}'); h=$($k get pv $pv -o jsonpath='{.spec.csi.volumeHandle}')
    echo "$pvc $(gcloud compute disks describe "$(echo $h | cut -d/ -f6)" --zone "$(echo $h | cut -d/ -f4)" --format='value(name,type.basename(),sizeGb)')"
  done
  $k -n ate-system exec postgres-0 -c postgres -- sh -c 'ls -ld $PGDATA/pg_wal; df /var/lib/postgresql/data /var/lib/postgresql/wal 2>/dev/null'
} > "$out" 2>&1
