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

# Alternates the shared and separate-WAL layouts, one run each, in order.
R=/home/haiyanmeng/bench-runs/ateapi-cold-6x8-layout-repeats-20261019
$R/common/run-one.sh $R/r1-shared ateapi_cold_resume_pause_fake_6x8cpu_size10 ateapi-cold-resume-capacity
$R/common/run-one.sh $R/r2-walvol ateapi_cold_resume_pause_fake_6x8cpu_size10_walvol ateapi-cold-wal-volume
$R/common/run-one.sh $R/r3-shared ateapi_cold_resume_pause_fake_6x8cpu_size10 ateapi-cold-resume-capacity
$R/common/run-one.sh $R/r4-walvol ateapi_cold_resume_pause_fake_6x8cpu_size10_walvol ateapi-cold-wal-volume
$R/common/run-one.sh $R/r5-shared ateapi_cold_resume_pause_fake_6x8cpu_size10 ateapi-cold-resume-capacity
echo "all done $(date -u +%FT%TZ)" > $R/done
