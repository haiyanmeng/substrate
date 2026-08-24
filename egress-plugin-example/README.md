# Testing extproc end to end

`egress-plugin-example` is an example plugin server that shows how to integrate
custom logic into the egress traffic path such as custom authorization,
credential injection, custom telemetry.

## Where your code goes

`policy/` is the part you are meant to change. A policy is a function from a
`policy.Request` to a `policy.CalloutResult`; nothing in the package imports
Envoy, so a policy can be written and tested without a gateway, a cluster, or a
protobuf. `AllowAll` in `policy/policy.go` is the one shipped here.

`internal/extproc/` is the glue that speaks ext_proc to Envoy: it projects a
`ProcessingRequest` into a `policy.Request`, calls the policy, and turns the
result back into header mutations or an immediate denial. It is `internal` on
purpose — shipping your own policy should not require editing it.

To ship one: implement `policy.Policy`, and pass it to `extproc.NewServer` in
`main.go` in place of `policy.AllowAll{}`.

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

## Step 3: Make your actors trust the egress MITM trust bundle

To make your actors trust the egress MITM trust bundle, update your `ActorTemplate` to include
the `system-info` volume, and the `system-info` volumeMount, and several environment variables, similar to:

```
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: egress
  namespace: ate-demo-egress
spec:
  # Requires an sdsmint install (--experimental-use-sdsmint): only that path
  # creates the egress-mitm-ca-pool Secret the bundle is derived from, and an
  # actor referencing a bundle that does not resolve fails to start.
  volumes:
  - name: system-info
    systemInfo:
      dataSources:
      # The trust anchors for the per-SNI leaves the MITM egress gateway mints.
      - trustBundle:
          name: egress-mitm.ate.dev
          path: trust-bundle.pem
  containers:
  - name: egress
    image: ko://github.com/agent-substrate/substrate/demos/egress
    command: ["/ko-app/egress"]
    # SSL_CERT_FILE alone is not enough to make the projected bundle the whole
    # story: it replaces the default cert FILE list, but the default cert
    # DIRECTORY list is still scanned, and the base image keeps its public
    # roots in /etc/ssl/certs. Pointing SSL_CERT_DIR at the projection too
    # makes the anchor set exactly the gateway CA, so a successful HTTPS fetch
    # proves the projected bundle validated the minted leaf. Under sdsmint the
    # public roots are useless anyway: every origin is fronted by the gateway.
    env:
    - name: SSL_CERT_FILE
      value: /run/ate/trust-bundle.pem
    - name: SSL_CERT_DIR
      value: /run/ate
    - name: NODE_EXTRA_CA_CERTS    # Node.js
      value: /run/ate/trust-bundle.pem
    - name: REQUESTS_CA_BUNDLE     # Python requests
      value: /run/ate/trust-bundle.pem
    - name: GIT_SSL_CAINFO          # git over https
      value: /run/ate/trust-bundle.pem
    volumeMounts:
    - name: system-info
      mountPath: /run/ate   # the bundle lands at /run/ate/trust-bundle.pem
```

After updating your ActorTemplate, Apply the ActorTemplate and create new actors using it.

## Step 4: Send egress traffic from an actor

## Step 5: Observe the policy decision and enforcement

```shell
# 1. Check the log of the egress plugin to see the allow/deny decision for a request.
kubectl -n nous-dev logs -l app=egress-plugin --prefix --tail=-1 \
  | grep 'egress allowed\|egress denied'
```