# Which environment variables actually redirect a tool's trust anchors

An actor under a `--experimental-use-sdsmint` install talks to every HTTPS origin
through the MITM egress gateway, which presents a per-SNI leaf signed by the
`egress-mitm.ate.dev` CA. Projecting that CA into the actor's filesystem is only
half the job — see `docs/dev/trust-bundle-projection.md` for that half. The other
half is pointing each tool at it, and tools disagree about how they can be
pointed.

This records which variables were measured to work, on the `alpine` base image
the sandbox demo uses (`.ko.yaml:22`). The short version: `SSL_CERT_FILE` plus
`SSL_CERT_DIR` covers more than expected, including `apk` and busybox `wget`, and
misses `git`, which is the one most people assume is covered.

## Results

Measured, not inferred. Method and caveats below.

| Tool | `SSL_CERT_FILE` | `SSL_CERT_DIR` | Own variable | Notes |
| --- | --- | --- | --- | --- |
| curl | **honoured** | honoured | `CURL_CA_BUNDLE` (redundant) | `SSL_CERT_FILE` alone is a hard failure on a bad bundle |
| apk-tools 3.x | **honoured** | ignored | `--ca-cert` (flag only) | `SSL_CERT_FILE` alone is sufficient |
| busybox wget | **honoured** | not isolated | `--ca-certificate` (flag only) | via its `ssl_client` helper |
| python stdlib `ssl` | only with **both** | only with **both** | — | either alone still leaves public roots reachable |
| Go (`crypto/x509`) | honoured | honoured | — | reads both on Unix |
| **git** | **ignored** | **ignored** | **`GIT_SSL_CAINFO`** | see below |
| Node.js | ignored | ignored | `NODE_EXTRA_CA_CERTS` | **appends**, does not replace |
| python `requests` | ignored | ignored | `REQUESTS_CA_BUNDLE` | **fails closed** on a bad path |
| Java | ignored | ignored | none usable | needs a keystore, not a PEM |

## The three findings worth knowing

### git ignores the OpenSSL variables

The one real gap. git sets `CURLOPT_CAINFO` from its own configuration
(`http.sslCAInfo`, defaulting to the build-time path) before libcurl consults the
environment, so the OpenSSL variables never get a say:

```
$ SSL_CERT_FILE=/tmp/empty-ca.pem SSL_CERT_DIR=/tmp/emptydir \
    git ls-remote https://github.com/git/git HEAD
1a3e64c6c4a623626ff0687008732a8e007e2a1c    HEAD      # succeeded against empty anchors

$ GIT_SSL_CAINFO=/tmp/empty-ca.pem git ls-remote https://github.com/git/git HEAD
fatal: unable to access '...': error adding trust anchors from file: /tmp/empty-ca.pem
```

An actor configured with only `SSL_CERT_FILE`/`SSL_CERT_DIR` will fail `git clone`
under sdsmint while `curl` to the same host works — a confusing pair of symptoms
that points at the network rather than at trust configuration.

### apk and busybox wget *are* covered, contrary to the usual assumption

Both are routinely written off as reading `/etc/ssl/certs/ca-certificates.crt`
from a compiled-in path. On Alpine 3.24 neither does exclusively: apk-tools 3.0.6
honours `SSL_CERT_FILE`, and busybox `wget` shells out to an `ssl_client` helper
that calls `SSL_CTX_set_default_verify_paths()`, which reads it too.

```
$ docker run --rm alpine sh -c ': > /tmp/e.pem; SSL_CERT_FILE=/tmp/e.pem apk update'
WARNING: ... TLS: server certificate not trusted
2 unavailable, 0 stale; 16 distinct packages available
```

This matters more than the others: Alpine 3.24 ships `https://dl-cdn.alpinelinux.org`
in `/etc/apk/repositories`, so under sdsmint the *first* `apk add` in a fresh
sandbox fails if the CA is not in scope.

`docs/dev/non-http-egress-manual-test.md:56-63` hits exactly this and works
around it by rewriting the repositories to HTTP, on the grounds that "HTTPS
egress does not work yet". Given that apk honours `SSL_CERT_FILE`, that
workaround should no longer be needed on a template that projects the bundle and
sets the variable — worth re-testing before the `sed -i 's|https|http|g'` step is
removed there.

### `SSL_CERT_FILE` alone does not narrow the anchor set

It replaces the default cert *file*; the default cert *directory* is still
scanned, and the base image keeps its public roots there. Isolating the two
against python's stdlib:

```
SSL_CERT_FILE=empty only      -> OK          # public roots still reachable via the default dir
SSL_CERT_DIR=emptydir only    -> OK          # public roots still reachable via the default file
BOTH empty                    -> CERTIFICATE_VERIFY_FAILED
```

So both are needed to make the projected bundle the *whole* trust story. Under
sdsmint leaving the public roots in place is harmless — no origin presents a
publicly-rooted leaf — but a test that means to prove the projected bundle
validated the minted leaf has to set both, or it proves nothing.

## Two asymmetries in the per-runtime variables

* **`NODE_EXTRA_CA_CERTS` appends.** Node keeps trusting its built-in roots
  alongside the projected CA. Harmless in production, but it means Node will not
  reproduce a trust failure that every other tool would catch — do not use Node
  as the probe in a test that asserts the anchor set.
* **`REQUESTS_CA_BUNDLE` fails closed.** A path that does not exist makes every
  request raise `OSError: Could not find a suitable TLS CA certificate bundle`,
  rather than falling back to certifi. Node, by contrast, prints
  `Warning: Ignoring extra certs ... load failed` and carries on. So a typo in
  the `requests` variable breaks Python egress outright, while the same typo in
  the Node variable is nearly silent — the failure modes are worth keeping in
  mind when these values drift from the volume's `mountPath`.

## Not fixable with an environment variable

Java reads a PKCS12/JKS keystore, not a PEM bundle. Pointing
`JAVA_TOOL_OPTIONS=-Djavax.net.ssl.trustStore=` at the projection will not load
it. A JVM in an actor needs `keytool -importcert` against the projected PEM at
startup.

The general fallback for any such tool is to append the projected PEM to
`/etc/ssl/certs/ca-certificates.crt` at actor start. Mounting over that path
directly does not work: the mount shadows the whole directory.

## Applies only if the toolchain is installed

None of these are in the `alpine` base, so the sandbox template does not set
them. Each is a one-line addition when the relevant toolchain is:

| Variable | Tool |
| --- | --- |
| `CARGO_HTTP_CAINFO` | cargo |
| `AWS_CA_BUNDLE` | AWS CLI and SDKs |
| `PIP_CERT` | pip |
| `NPM_CONFIG_CAFILE` | npm (replaces, unlike `NODE_EXTRA_CA_CERTS`) |
| `DENO_CERT` | Deno |
| `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH` | gRPC C-core (Python, Ruby, C++) |

## What the sandbox demo sets

`demos/sandbox/sandbox.yaml.tmpl` projects the bundle at `/run/ate/trust-bundle.pem`
and sets `SSL_CERT_FILE`, `SSL_CERT_DIR`, `GIT_SSL_CAINFO`, `NODE_EXTRA_CA_CERTS`,
and `REQUESTS_CA_BUNDLE`. These reach client-supplied commands as well as the
server: `demos/sandbox/main.go:80-91` leaves `cmd.Env` nil when a request carries
no `envvars` (so `os/exec` inherits the parent environment) and rebuilds it from
`os.Environ()` when it does, so both branches propagate.

`demos/egress/egress.yaml.tmpl` sets only `SSL_CERT_FILE` and `SSL_CERT_DIR`,
which is correct for it — its actor runs one Go binary and no subprocesses.

## Method

Each variable was pointed at an empty PEM file or an empty directory and the tool
run against a public HTTPS origin. A tool that still succeeds is not consulting
that variable; a tool that fails verification is. This measures the property that
matters — *does this variable control the trust anchor set* — which is necessary
and sufficient for the MITM case, without needing a gateway.

```sh
: > /tmp/empty-ca.pem
mkdir -p /tmp/emptydir
SSL_CERT_FILE=/tmp/empty-ca.pem SSL_CERT_DIR=/tmp/emptydir <tool> <https url>
```

Two things to watch when re-running it:

* Use an **empty but existing** file, not a missing one, to tell "variable ignored"
  apart from "variable honoured, and the tool fails closed on a bad path".
* Isolate `SSL_CERT_FILE` from `SSL_CERT_DIR`. Setting only one leaves the other
  default in place, and the tool succeeds for a reason unrelated to the variable
  under test.

Inside a real actor, `docs/dev/sandbox-actor-git-clone-test.md` has the `ate_run`
helper for running these through the sandbox `/process` endpoint.

## Caveats

* Versions: Alpine 3.24.1, apk-tools 3.0.6-r0, BusyBox 1.37.0; and on the host
  curl 8.21.0 (OpenSSL 3.6.3), git 2.55.0, Python 3.13.14 with requests 2.34.2,
  Node v22.17.0. The git, curl, Node and Python results are from the host
  toolchain, not from Alpine builds of the same tools — the mechanisms are
  build-independent, but an Alpine curl linked against a different TLS backend
  could differ.
* Not run against a live sdsmint gateway. What is verified is which variable
  governs each tool's anchor set, not an end-to-end fetch through a minted leaf.
* busybox `wget` was confirmed to honour `SSL_CERT_FILE`; `SSL_CERT_DIR` was not
  isolated separately for it.
