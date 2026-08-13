# Envoy behavior with and without resource TTL

**TL;DR**  
sdsmint stamps a TTL on every resource it sends; Envoy runs a timer per resource, drops the secret when it fires, and the next handshake for that name re-subscribes and gets a fresh leaf.

**Without the resource TTL, nothing happened at all**: Envoy held the secret forever, went on serving the leaf past its `notAfter`, and every handshake still succeeded — silently, with no error on either side. A downstream client verifying a stale certificate would fail. 

Both behaviours were measured here, against Envoy **1.37.5**.

## Running it

```
./run.sh
```

It builds everything it needs, generates a fresh CA, starts sdsmint and Envoy, mints one name, handshakes that name every 10s out to 150s, then handshakes a never-before-seen SNI as a health control, prints a verdict and tears down.

The cert leaf TTL is 1m. The resource TTL is derived at half the leaf lifetime, so a run should show a re-mint every \~30s. 

[https://github.com/haiyanmeng/substrate/tree/sds-3/poc/sdsmint/expiry](https://github.com/haiyanmeng/substrate/tree/sds-3/poc/sdsmint/expiry) 

Artifacts: `ttl-run.txt`, `ttl-sds.log`, `ttl-envoy.log`, and `ttl-probes.jsonl` (one JSON object per probe, so a run can be re-read without rerunning it).

## Result

Full output of one run is in `ttl-run.txt`. Abridged:

```
############################################################
# scenario: ttl   --leaf-cert-ttl=1m
#   SNI aged-5661.example.com
############################################################

       t  result
      0s  connected | serial 4e9d35eb6e0d | fresh for 60s | leaf is valid
     20s  connected | serial 4e9d35eb6e0d | fresh for 39s | leaf is valid
     30s  connected | serial 35791f99a44a | fresh for 59s | leaf is valid   <- re-minted
     50s  connected | serial 35791f99a44a | fresh for 39s | leaf is valid
     60s  connected | serial 7584d829f1e3 | fresh for 59s | leaf is valid   <- re-minted
     90s  connected | serial f248c556ec27 | fresh for 59s | leaf is valid   <- re-minted
    120s  connected | serial 755bc39fc9a7 | fresh for 59s | leaf is valid   <- re-minted
    150s  connected | serial 16d418915b8d | fresh for 59s | leaf is valid   <- re-minted

  control (never-before-seen SNI)
         connected | serial 43f4161e01d0 | fresh for 59s | leaf is valid

  --- verdict ---
    ok: 6 leaves across 16 handshakes, never an expired one
  --- what sdsmint did ---
    certificates issued: 7                        <- 6 for the aged name, 1 control
  --- ssl / on-demand stats (nonzero only) ---
    listener.mitm.on_demand_secret.cert_requested: 7
    listener.mitm.on_demand_secret.cert_updated: 12
    listener.mitm.ssl.handshake: 17               <- 17 probes, 17 completed
  --- envoy warnings and errors ---
                                                  <- none
```

Six leaves in 150s, issued on a 30s cadence to the tenth of a second (`08:59:05.444, 08:59:35.537, 09:00:05.625, 09:00:35.714, …`), never an expired one served, 17/17 handshakes completing, nothing logged. Two consecutive runs produced this same shape.

## Before the resource TTL

These are kept because they are why the TTL exists, and because a regression would look exactly like them again.

**With nothing refreshing a leaf** — no rotation, no sweep, no resource TTL. Full output in `stuck-run.txt`:

```
      0s  connected | serial 14b7af9e02e6 | fresh for 60s   | leaf is valid
     60s  connected | serial 14b7af9e02e6 | EXPIRED 0s ago  | served anyway, actor accepted it
    120s  connected | serial 14b7af9e02e6 | EXPIRED 60s ago | served anyway, actor accepted it
    150s  connected | serial 14b7af9e02e6 | EXPIRED 91s ago | served anyway, actor accepted it
```

One mint, one serial, 91 seconds past expiry, every handshake completing. Envoy neither refused the stale secret nor re-subscribed; no `ssl.connection_error` or `ssl.fail*` stat was nonzero, and `cert_active` still counted the dead leaf as live. A reconnect did not help either, since Envoy replays what it holds and the server adopts it.  