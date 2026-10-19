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


# Local environment for the haiyanmeng-gke-dev / substrate-poc cluster.
# Copied from hack/ate-dev-env.sh.example and reconciled against what is
# actually deployed: the cluster location here is the one in the live
# ate-api-authentication issuer, not the example's us-west1-c.

export PROJECT_ID=${USER}-gke-dev
export PROJECT_NUMBER=$(gcloud projects describe ${PROJECT_ID} --format="value(projectNumber)")

export GCE_REGION=us-central1
export CLUSTER_LOCATION=us-central1-a

export NETWORK=nh-prow-parity
export SUBNETWORK=nh-prow-parity

export CLUSTER_NAME=nh-prow-parity
export CLUSTER_VERSION=1.36.3-gke.1767000
export NODE_POOL_NAME=default-pool
export NODE_POOL_VERSION=1.36.3-gke.1767000
export GVISOR_NODE_MACHINE_TYPE=c3-highcpu-88

# Set this if you are using an existing cluster with a different context name.
export KUBECTL_CONTEXT=

export BUCKET_NAME=snapshot-substrate-test-${PROJECT_ID}

export KO_DOCKER_REPO="gcr.io/${PROJECT_ID}/ate-images"

# Set this if you want to override the default build platforms
export KO_DEFAULTPLATFORMS=linux/amd64

# Required by install-ate.sh; the bundled Kubernetes Secrets provider, as in hack/ate-dev-env.sh.example.
export ATE_CREDENTIAL_PROVIDER='{"name":"k8s.io"}'
