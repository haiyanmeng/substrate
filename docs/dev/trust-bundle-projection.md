# The trustBundle SystemInfo data source, end to end

Notes from reviewing PR #941 ("Add trustBundle as a SystemInfo volume data
source"). Covers what the feature delivers, how each component handles it, what
the e2e proves, and what an actor has to do to adopt it.

Reference points: `pkg/api/v1alpha1/actortemplate_types.go`,
`cmd/ateapi/internal/controlapi/trust_bundle.go`, `internal/pemutil/`,
`cmd/atelet/main.go`, `internal/e2e/suites/egressmitm/`.

## The chain

```
egress-mitm-ca-pool Secret   (ate-system; written by `kubectl-ate admin make-ca-pool`)
   │  watched by atecontroller's EgressMITMTrustReconciler (#946)
   ▼
ClusterTrustBundle "egress-mitm.ate.dev:mitm:primary-bundle"
   │  read by ateapi at Run/Restore, sanitized by internal/pemutil
   ▼
WorkloadSpec.…TrustBundle{path, pem_bundle}  →  atelet  →  /run/ate/trust-bundle.pem
   │
   ▼
actor: x509 CertPool ← that file, used as tls.Config.RootCAs
   │
   ▼
handshake against the per-SNI leaf sdsmintd mints from the SAME pool
```

The same pool sits at both ends — signing the leaf inside the gateway,
validating it inside the actor.

## It is a data source, not a volume

A common first confusion: the probe fixture projects the bundle but adds no new
`volumeMount`. It does not need one. `trustBundle` is a second *data source
inside an existing systemInfo volume*:

```yaml
volumes:
- name: system-info          # one volume
  systemInfo:
    dataSources:             # a list; each entry contributes files
    - actorMetadata: {items: [...]}   # actor-id, atespace, actor-uid
    - trustBundle:
        name: egress-mitm.ate.dev
        path: trust-bundle.pem
containers:
- name: probe
  volumeMounts:
  - name: system-info
    mountPath: /run/ate      # → /run/ate/trust-bundle.pem
```

`SystemInfoDataSource` is a oneof of `{actorMetadata, trustBundle}` (enforced by
`+kubebuilder:validation:ExactlyOneOf`), and every data source's `path` is
relative to the volume root. Same shape as a Kubernetes projected volume, where
`secret`/`configMap`/`downwardAPI` sources compose into one mount.

This is also why `SystemInfoVolumeSource` carries two duplicate-path CEL rules:
all data sources in a volume share one flat filename namespace under the single
mount, so `trustBundle.path` must be checked against `actorMetadata.items[].path`.

## What ateapi does

`resolveTrustBundles` (`trust_bundle.go:73`) runs on the resume path, before the
spec is sent to atelet:

- The wire type carries only `{path, pem_bundle}` — the bundle **name** never
  crosses to atelet, which holds no RBAC on `certificates.k8s.io` and needs none.
- Names are allowlisted (`supportedTrustBundles`), mapping each supported name
  to its backing ClusterTrustBundle object. Enforced here rather than in the CRD
  schema so a future backend registry (#932) widens it without an API change.
- Contents are sanitized kubelet-style by `internal/pemutil`: `CERTIFICATE`
  blocks only, re-encoded without headers, deduplicated by SHA-256 of the DER.
- Failures — unsupported name, unavailable backend, missing/empty/unusable
  bundle — fail the actor start with an error naming the bundle.

It is called from `ensureAteletRestored`, which is the sole origin of all three
atelet entry points (`RestoreRequest` local and external, `RunRequest`), so
coverage is complete. Pause and suspend build specs too but never reach
`writeSystemInfoVolume`, so their empty `PemBundle` is harmless.

## What atelet does

Nothing PEM-specific. It treats the bundle as **opaque bytes** — never parses
it, never talks to any bundle backend, never knows it is a certificate.

1. **Fail closed on empty** (`main.go:1558-1565`). Empty bytes mean ateapi did
   not resolve; better to fail the start than hand the workload an empty trust
   file that turns into an inscrutable handshake error later.
2. **Write it** via `writeSystemInfoFile` (`:1598`) — re-validate the relative
   path (belt-and-braces behind the CEL rules; atelet is the last thing before
   the host filesystem), `MkdirAll` the parent, write-to-temp-and-rename at
   `0644` under `SystemInfoVolumeRoot(actorUID, volumeName)`.

   The rename discipline is load-bearing (`:1534-1542`): kubelet's atomic writer
   swaps a symlink to a new timestamped directory, which would break both the
   micro-VM virtiofsd's find-paths migration (re-binds guest FUSE state by paths
   recorded at suspend) and gVisor's gofer (re-opens by path on restore).
   Per-file rename keeps the visible path fixed.
3. **Expose it read-only**, per sandbox class:
   - gVisor — `oci.go:371-375`, bind mount with `{"bind", "ro"}`.
   - micro-VM — `ateom-microvm/systeminfo.go`, bind into the kata shared tree at
     `SharedDir(actorUID)/system-info`, remounted `ro`, plus `{"rbind","ro"}` on
     each container mount. Read-only twice over.
4. **Regenerate on every Run/Restore**, before the sandbox starts; the restore
   path wipes and rebuilds the roots dir first (`:2023`). SystemInfo volumes are
   deliberately excluded from the checkpoint path, so a restored actor gets the
   bundle as of its resume, not whatever was in the golden snapshot.

Explicitly absent: no `x509.ParseCertificate`, no `CertPool` (the actor builds
that), no live refresh for running actors (`TODO(#932)`, PR 2), no
re-sanitization.

## How an actor "trusts" the bundle

It does not verify it. There is no signature on the file, no attestation, no
pinning — the actor trusts those bytes for the same reason it trusts
`/etc/ssl/certs` in its own image: **because of where the file came from**.
Trust is positional and rests on the integrity of the delivery channel.

The feature introduces **no new trust assumption**. The actor already trusts the
platform completely — the platform chose its kernel, assembled its rootfs, wrote
its binary. Anyone who could forge the trust bundle could more easily replace
the actor's TLS library.

Chain of custody:

| Hop | What protects it |
| :--- | :--- |
| `egress-mitm-ca-pool` Secret | Namespace RBAC; only atecontroller reads it. Contains the CA **signing key** — never leaves the control plane. |
| → ClusterTrustBundle | Cluster-scoped, RBAC-gated write. The reconciler owns it and reverts hand-written contents. ateapi holds `get,watch,list` only. |
| → ateapi resolve | Only allowlisted names resolve; the actor cannot request an arbitrary bundle. |
| → atelet | mTLS gRPC (`atelet/main.go:353`, `ateapi/main.go:487`). |
| → host file | Root-owned, `0644` under a `0755` dir, outside any actor-writable area, write-temp-and-rename. |
| → sandbox | Read-only bind; twice over on micro-VM. |

Three things the actor is really trusting, worth separating:

1. **That the file is what the platform intended** — positional trust, from the
   chain above.
2. **That the platform's MITM is legitimate.** The bundle is precisely what
   makes interception invisible: any TLS origin the gateway fronts presents a
   leaf the actor accepts. Loading it is the actor consenting to have its egress
   TLS terminated and inspected. Design intent (#871), not a leak.
3. **That the pool's private key stays in the control plane.** The load-bearing
   secret: whoever holds it can impersonate *any* origin to *any* actor
   projecting this bundle — no name constraints. Hence the pool blob (cert
   **and** signing key) must never be handed to an actor; only the sanitized
   certificate half is projected.

Deliberately absent: no revocation or freshness signal (a rotated-out CA stays
trusted by a running actor until it resumes); no cryptographic binding to the
actor — this is a shared cluster-wide anchor set authenticating the *gateway to
the actor*, and says nothing about who the actor is. Actor identity travels the
other direction, on the `ate.actor.identity` filter state on the CONNECT leg.

## Adopting it: what an actor must do

Something has to point the TLS client at the file. The platform delivers bytes
to a path and stops; it does not merge them into the image's trust store.
Hand-writing a `tls.Config` is the *probe's* approach, driven by test
requirements (toggle modes per request, prove the anchors in isolation) — not
the expected one.

**Tier 1 — environment variable (normal case, template-only).**

```yaml
containers:
- name: main
  env:
  - name: SSL_CERT_FILE          # Go, OpenSSL, curl, git
    value: /run/substrate/certs/ca.pem
  - name: NODE_EXTRA_CA_CERTS    # Node.js
    value: /run/substrate/certs/ca.pem
  - name: REQUESTS_CA_BUNDLE     # Python requests
    value: /run/substrate/certs/ca.pem
  volumeMounts:
  - name: trust
    mountPath: /run/substrate/certs
```

Semantics differ and it matters:

- `SSL_CERT_FILE` **replaces** the default root set (Go's `x509.SystemCertPool`
  and OpenSSL both treat it as *the* file). With a bundle holding only the MITM
  CA, the actor then trusts nothing but the gateway.
- `NODE_EXTRA_CA_CERTS` **appends** to Node's bundled roots — additive.

Under full MITM egress, replacing is arguably correct: every origin is fronted
by the gateway anyway, and a narrow root set is a smaller attack surface. If any
destination bypasses the gateway, use the additive form or those connections
break.

**Tier 2 — mount over the image's CA directory.**

```yaml
volumeMounts:
- name: trust
  mountPath: /etc/ssl/certs      # with path: ca-certificates.crt
```

Every OpenSSL-linked binary picks it up with no configuration. The catch: a bind
mount **shadows** the directory, so the public roots vanish — the same
replace-vs-append tradeoff, taken globally and irreversibly for that container.
It also consumes the volume's single mount point.

**Tier 3 — explicit `tls.Config`.** Only when you need exclusive anchors,
per-connection control, or a stack that honours no CA env var.

Adoption stays genuinely opt-in at every tier. An actor that ignores the file,
or pins its own roots, simply fails its handshakes under MITM; the platform
never forces the anchors on it.

## The e2e: `internal/e2e/suites/egressmitm`

One test, `TestActorEgressMITMTrust`. Everything else in the feature proves the
bytes get **delivered**; this proves they are the **right** bytes by completing
a real TLS handshake with them.

Steps: `EnsureEgressTrustBundle` → `DeployProbe` + `waitForGolden` → create and
resume an actor → two calls into the probe's `/fetch`, both against
`https://example.com/`.

### The positive/negative pair

`/fetch?roots=bundle` builds `tls.Config.RootCAs` **only** from the projected
file. `/fetch?roots=system` uses the image's roots only. Every cell inverts
between the two deployment modes:

| | `roots=bundle` | `roots=system` |
| :--- | :--- | :--- |
| **sdsmint (MITM)** | ✅ minted leaf chains to the pool CA | ❌ chains to no public CA |
| **passthrough** | ❌ bundle holds no public CAs | ✅ real example.com cert |

So the pair is diagnostic, not merely confirmatory: the test cannot quietly pass
against the wrong gateway. Drop the `--experimental-use-sdsmint` step and the
positive case fails immediately rather than degrading into a vacuous pass.
Without the negative control, a green `roots=bundle` could in principle mean
"the bundle happens to contain a public root". The assertion additionally
requires the negative failure to mention `certificate`/`x509`, so a connection
refused or DNS failure does not count as proof.

### The probe's handler

One branch, one field (`fixtures/probe/main.go:311-349`):

```go
tlsCfg := &tls.Config{}                                  // :319
if roots := r.URL.Query().Get("roots"); roots != "system" {
    b, _ := os.ReadFile(trustFile)                       // /run/ate/trust-bundle.pem
    pool := x509.NewCertPool()                           // EMPTY — not a system clone
    pool.AppendCertsFromPEM(b)
    tlsCfg.RootCAs = pool                                // :333
}
```

- **`roots=system`** — `RootCAs` stays `nil`, so `crypto/tls` falls back to
  `x509.SystemCertPool()`. The probe's ko base is
  `gcr.io/distroless/static-debian13` (`.ko.yaml:15`), which ships the full
  public root set, so this really is the standard public-CA path.
- **`roots=bundle`** — a **fresh, empty** pool loaded only from the projected
  file. Exclusive, not additive; that exclusivity is the whole proof, since a
  successful handshake can only be explained by the projected anchors.
- The default is the *strict* mode: the condition is `roots != "system"`, so
  `""`, `"bundle"`, and any typo take the bundle path. A misspelled parameter
  fails loudly instead of silently degrading to permissive.
- **Not** varied: no `InsecureSkipVerify`, no `ServerName` override, no custom
  `VerifyPeerCertificate`. Hostname verification stays on in both modes, so the
  gateway's leaf must actually carry `example.com` — which is what makes
  "per-SNI minted leaf" load-bearing.
- Transport errors, including every TLS verification failure, go into the JSON
  `error` field with **HTTP 200**. The status code is reserved for "the probe
  itself is broken", which lets `probeFetch` retry only on non-200 (covering the
  post-resume xDS window) while returning verification failures as data.

The `roots` parameter has no central definition — it is a stringly-typed HTTP
contract with exactly two sites: the reader at `fixtures/probe/main.go:320` and
the writer at `suites/egressmitm/egressmitm_test.go:136`. Note `"bundle"` is
never actually tested for; `"system"` is the sole real protocol token,
duplicated across two files in two binaries with nothing linking them.

### Gating and why it runs twice

`E2E_EGRESS_MITM` guards the test because sdsmint replaces the egress gateway
**cluster-wide**. That is also why the three CI steps
(`pr-workflow.yaml:110-133`) come after both standard lanes — once TLS is
intercepted, `TestActorEgressHTTPS`'s end-to-end-to-origin assumption is false.
It then runs twice, gVisor and `E2E_SANDBOX_CLASS=microvm`, because trust
*delivery* differs by sandbox class and a handshake is the only way to prove the
file survived that path intact.

Locally:

```
hack/install-ate-kind.sh --deploy-ate-system --experimental-use-sdsmint
E2E_EGRESS_MITM=1 hack/run-e2e-kind.sh ./internal/e2e/suites/egressmitm -v -args --no-color
E2E_EGRESS_MITM=1 E2E_SANDBOX_CLASS=microvm hack/run-e2e-kind.sh ./internal/e2e/suites/egressmitm -v -args --no-color
```

### Ensure vs. replace

`EnsureEgressTrustBundle` deliberately never touches an existing pool
(`egressmitm_test.go:84-91`): sdsmintd signs from the pool **mounted into the
gateway pod**, and kubelet propagates Secret updates on its own schedule, so
replacing it mid-test would race propagation and flake the handshake.

The identity suite can call `ReplaceEgressTrustPool` freely because it never
does a handshake — it byte-compares the projected file (`identity_test.go:135`)
and rotates across suspend/resume (`:167`, `:192`) to prove contents refresh on
every Run/Restore. The two suites are complementary: **identity proves delivery
and freshness, egressmitm proves validity.**

## Weak points noted during review

- **`internal/pemutil` never calls `x509.ParseCertificate`.** The
  `TrustBundleDataSource` godoc says start fails on an "unparseable" bundle;
  the code only checks PEM framing and non-emptiness. Unreachable today (the
  apiserver validates CTB contents) but real for the planned backend registry.
  Consumer-side consequence: `AppendCertsFromPEM` returns false only when
  *zero* certs were added and silently skips individual bad ones, so a
  `CERTIFICATE`-typed block with garbage DER yields a thin pool and an opaque
  handshake failure instead of a named start failure.
- **`"egress-mitm.ate.dev:mitm:primary-bundle"` is hardcoded in four places** —
  `egressmitmtrust_controller.go:99` (the owner, unexported),
  `trust_bundle.go:48`, `internal/e2e/trustbundle.go:40`,
  `trust_bundle_test.go:85`. Drift fails only at runtime. `CTBPrefix` in
  podcertcontroller is the export precedent.
- **Path prefix collisions escape the CEL rules.** Both duplicate-path rules
  compare for equality, so `trustBundle` at `a` plus an `actorMetadata` item at
  `a/b` is admitted and fails late in atelet's `MkdirAll` (ENOTDIR).
  Fail-closed, so it is message quality rather than a safety hole.
- **The e2e depends on the external origin `https://example.com/`** for both
  cases. Under MITM the origin cert is irrelevant, but the gateway still has to
  reach the real origin to return 200.
- **`waitForEgressTrustBundle` polls the apiserver while ateapi resolves from
  its informer cache** (`trustbundle.go:122-129` documents and accepts the
  race). Effectively unhittable, but the first suspect if a rotation assertion
  ever flakes.
- **The probe's transport is `&http.Transport{TLSClientConfig: tlsCfg}`** rather
  than a clone of `http.DefaultTransport`, so it drops `ProxyFromEnvironment`.
  Correct while egress is transparent via atunnel; a trap if that changes.
- **The negative control's strength depends on the base image shipping public
  roots.** If it ever lost them, `roots=system` would fail with an `x509`
  unknown-authority error regardless of interception, and the assertion would
  pass for the wrong reason. Nothing pins that today.
