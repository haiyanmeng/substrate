# Enabling man-in-the-middle (MITM) interception for Actor Egress policy

Under an sdsmint install, the egress gateway terminates every TLS connection an
actor opens and re-originates it. The certificate the actor sees is therefore
not the origin's — it is a per-SNI leaf the gateway minted, which chains to the
gateway's own CA and to no public root. An actor that validates against only the
public roots rejects it, and every HTTPS request the actor makes fails with a
certificate error.

This guide covers how to project the gateway's CA into an actor's filesystem
and how to point the actor's TLS client at it.

## When you need this

You need it when **all** of the following hold:

* The cluster runs the sdsmint egress gateway (`hack/install-ate.sh
  --deploy-atenet --experimental-use-sdsmint`).
* The actor makes **HTTPS** (or any TLS) requests.

If you configure this on a non-sdsmint install you will *break* the actor:
the steps below make the gateway CA the actor's only trust anchor, and without
a MITM gateway in front of it nothing the actor dials will chain to that CA.

Some destinations cannot be reached through interception no matter how the
actor's trust store is configured — see
[Exempting a destination](#exempting-a-destination-from-interception).

## Project the bundle

Add a `systemInfo` volume with a `trustBundle` data source, and mount it:

```yaml
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: my-actor
  namespace: my-namespace
spec:
  volumes:
  - name: system-info
    systemInfo:
      dataSources:
      # The trust anchors for the per-SNI leaves the egress gateway mints.
      - trustBundle:
          name: egress-mitm.ate.dev
          path: trust-bundle.pem
  containers:
  - name: app
    image: ...
    volumeMounts:
    - name: system-info
      mountPath: /run/ate   # the bundle lands at /run/ate/trust-bundle.pem
```

`trustBundle.name` selects a bundle substrate knows how to fetch.

`trustBundle.path` is relative to the root of the volume, so the file's absolute path is
`mountPath` + `path`. It must be a clean relative Unix path: no leading or
trailing `/`, no `//`, `:`, `.`, or `..` segments. A `systemInfo` volume takes
at most 8 data sources and their paths must not collide.

The projected PEM contains `CERTIFICATE` blocks only, deduplicated and
deliberately shuffled — order carries no meaning, so do not write anything that
depends on the first block being a particular certificate.

## Point the runtime at it

Projecting the file is not enough; each TLS stack has to be told to use it.

### Go, and anything linked against OpenSSL

```yaml
    env:
    - name: SSL_CERT_FILE
      value: /run/ate/trust-bundle.pem
    - name: SSL_CERT_DIR
      value: /run/ate
```

Set **both**. `SSL_CERT_FILE` replaces the default certificate *file* list, but
the default certificate *directory* list is still scanned, and most base images
keep their public roots in `/etc/ssl/certs`. With `SSL_CERT_FILE` alone the
actor trusts the gateway CA *in addition to* every public CA. Pointing
`SSL_CERT_DIR` at the projection as well makes the anchor set exactly the
gateway CA — so a successful HTTPS fetch proves the projected bundle is what
validated the minted leaf, rather than a public root happening to work.

Under sdsmint the public roots are useless anyway: every TLS origin the actor
can reach is fronted by the gateway.

### Other runtimes

| Runtime | Variable | Note |
|---|---|---|
| Node.js | `NODE_EXTRA_CA_CERTS=/run/ate/trust-bundle.pem` | *Adds* to Node's bundled roots rather than replacing them; the actor keeps trusting public CAs. |
| Python `requests` | `REQUESTS_CA_BUNDLE=/run/ate/trust-bundle.pem` | `requests` defaults to certifi and ignores `SSL_CERT_FILE`. |
| Python `ssl` / `urllib` | `SSL_CERT_FILE`, `SSL_CERT_DIR` | Honored via OpenSSL's default verify paths. |
| Python `httpx`, other Certifi-based clients | — | No environment variable; Certifi is pinned in code. Pass the path explicitly, e.g. `httpx.Client(verify="/run/ate/trust-bundle.pem")`. |
| curl | `CURL_CA_BUNDLE=/run/ate/trust-bundle.pem` | |
| git over HTTPS | `GIT_SSL_CAINFO=/run/ate/trust-bundle.pem` | |
| Java | — | No environment variable. Convert the PEM to a PKCS#12 or JKS truststore at startup and pass `-Djavax.net.ssl.trustStore`. |

## Verify

`demos/egress/egress-mitm.yaml.tmpl` is a complete working template that does
exactly this. Deploy it against an sdsmint install:

```bash
./hack/install-ate.sh --deploy-demo-egress-mitm
```

Then drive an actor's egress at an HTTPS URL and confirm it returns a response
rather than a certificate error. Because the demo sets `SSL_CERT_DIR` as well,
a `200` is positive evidence that the projected bundle did the validating.

## Exempting a destination from interception

Projecting the bundle works when the actor's TLS client is something you can
configure. Some destinations are not:

* The client pins a certificate or a public key, so no minted leaf validates.
* The origin requires mutual TLS and the actor holds the client key — the
  gateway re-originates the connection and has no way to present it.
* The protocol inside the tunnel is not something the gateway can carry.

List those hostnames in `EgressPolicy.tls_interception_exemptions`, alongside
the policy's `rules`, and the gateway tunnels them through untouched, exactly
as a non-sdsmint install would. The policy is a subresource of the actor, set
over the ate API with `CreateActorEgressPolicy` or `UpdateActorEgressPolicy`:

```textproto
metadata { atespace: "my-namespace" name: "default" }
rules { hostnames { patterns: "api.example.com" patterns: "*.cdn.example.com" } }
tls_interception_exemptions: "api.example.com"
tls_interception_exemptions: "*.cdn.example.com"
```

Patterns use the same grammar as `HostnameRule.patterns`: an exact DNS name, or
a wildcard covering the complete leftmost label. `*.cdn.example.com` matches
`assets.cdn.example.com`, and matches neither `cdn.example.com` nor
`a.b.cdn.example.com`. They are matched against the SNI the actor sends rather
than a Host header, so a connection carrying no SNI is never exempt.

Note what this does and does not do:

* **It is not an authorization.** `rules` still decides which destinations the
  actor may reach at all. Exempting a hostname the policy denies does not open
  it; denying one the policy allows does not close it.
* **It costs you the visibility.** An exempted connection is a byte pipe: the
  gateway sees the SNI and the connection, and nothing inside it.
* **Only the sdsmint gateway intercepts anything**, so on a plain install the
  field is inert — every connection is already a passthrough.
* **It is implemented on the Envoy gateway only.** Under
  `--atenet-router=agentgateway` the MITM variant intercepts every connection
  and the field has no effect.

Anything that goes wrong falls back to intercepting: an unreachable control
plane, a gateway that has not yet picked up the policy, an SNI that does not
match. If an exemption does not seem to be taking effect, the actor sees a
certificate error rather than an open tunnel, and the gateway keeps working.

## Operational notes

**A bundle that does not resolve fails the actor start.** If the name is not on
the allowlist, the backing ClusterTrustBundle is missing, or the bundle is empty
or unparseable, the actor does not start — an actor that declared a trust bundle
must not run without one.

atelet logs it on the node that was going to host the actor, as the `err` field
of the interceptor's `Handle RPC` record, at INFO, with
`method=/atelet.AteomHerder/Run` (or `/atelet.AteomHerder/Restore` when a
suspended actor is coming back):

```
while populating system-info volume "system-info": system-info projection "trust-bundle.pem": trust bundle "egress-mitm.ate.dev": ClusterTrustBundle "egress-mitm.ate.dev:mitm:primary-bundle" not found
```

ateapi surfaces the same text to the caller that asked for the actor, wrapped
once by the resume step and once by gRPC:

```
while creating workload from spec: rpc error: code = Internal desc = while populating system-info volume "system-info": system-info projection "trust-bundle.pem": trust bundle "egress-mitm.ate.dev": ClusterTrustBundle "egress-mitm.ate.dev:mitm:primary-bundle" not found
```

That is the common case: projecting `egress-mitm.ate.dev` on an install without
`--experimental-use-sdsmint`, where nothing creates the `egress-mitm-ca-pool`
Secret the bundle derives from. `system-info` is the volume's `name` from your
template and `trust-bundle.pem` its `path`, so those two vary with what you
wrote. The other failure modes differ only in the innermost clause:

| Cause | Innermost clause |
|---|---|
| Name not on the allowlist | `trust bundle "my-own-bundle" is not supported by this deployment (supported: egress-mitm.ate.dev)` |
| Bundle present but empty or unparseable | `trust bundle "egress-mitm.ate.dev": unusable ClusterTrustBundle "egress-mitm.ate.dev:mitm:primary-bundle": …` |

**Rotation is picked up on the next resume.** atelet re-resolves the bundle on
both `Run` and `Restore`, so a suspended actor gets the current anchors when it
comes back. A long-running actor that never suspends keeps the copy made when it
started — and a process that has already loaded the file into memory (Go caches
its system pool after first use) will not see a change on disk either way. Plan
CA rotation around a resume, with an overlap window that covers the actors that
do not suspend.

**The bundle is not a substitute for authenticating the actor.** It lets the
actor verify the *gateway*. It says nothing to an origin about which actor is
calling; see `cmd/atenet/internal/router/README.md` for that direction.

## See also

* [API Configuration Guide](api-guide.md) — the full `systemInfo` volume
  reference.
* `demos/egress/README.md` — how tunneled egress and actor-identity
  authentication fit together.
* `cmd/atenet/internal/router/README.md` — the gateway side of the MITM leg.
