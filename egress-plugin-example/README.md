# Testing extproc end to end

`egress-plugin-example` is an example plugin server that shows how to integrate
custom logic into the egress traffic path such as custom authorization,
credential injection, custom telemetry.

## Unit tests

```bash
go test ./egress-plugin-example/...
```

Covers the wire contract — message kinds, immediate responses, header mutation,
duplicate credential collapse — the actor-URI parser, and the serving mTLS
config It does not cover any Envoy configuration, which is
where most of the real failures live.

## E2E tests

## Step 1: Install Substrate with the additional egress extproc configured in the egress Envoy configuration

```shell
./hack/install-ate.sh --deploy-ate-system \
  --experimental-use-sdsmint \
  --experimental-additional-egress-extproc-service nous-dev/egress-plugin:50051
```

`--experimental-use-sdsmint` is needed because the default egress Envoy config does not include sdsmint currently.

## Step 2: Deploy the additional egress extproc

```
kubectl create namespace nous-dev

# Nous needs to replace KO_DOCKER_REPO
KO_DOCKER_REPO="gcr.io/<project-id>/ate-images" \
./hack/run-tool.sh ko apply -f egress-plugin-example/egress-plugin.yaml

kubectl -n nous-dev rollout status deployment/egress-plugin
```
Currently, the extproc allows all the HTTP requests.

Nous needs to run this step every time they modify the extproc Go code.

After reinstalling Substrate using `./hack/install-ate.sh --delete-ate-system`
and `./hack/install-ate.sh --deploy-ate-system ...`, the `eggress-plugin` Deployment
needs to be restarted to pick up new podCertificates and clusterTrustBundles for the Pod:

```shell
kubectl rollout restart deployment egress-plugin -n nous-dev
```

## Step 3: Send egress traffic from an actor

## Step 4: Observe the policy decision and enforcement

```shell
# 1. Check the log of the egress plugin to see the allow/deny decision for a request.
kubectl -n nous-dev logs -l app=egress-plugin --prefix --tail=-1 \
  | grep 'egress allowed\|egress denied'
```