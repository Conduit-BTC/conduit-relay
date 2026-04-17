# Conduit L2 Scope 2 for Khatru

This directory contains the Scope 2 relay-layer extension for `khatru`.

It is organized as a self-contained module area with:

- extension implementation (`scope2.go`)
- unit and e2e tests
- runnable demo relay (`cmd/demo`)
- demo validation script (`run_demo.sh`)

## Build the demo relay

From this project root:

```bash
go build ./cmd/demo
```

## Build Docker image (for conduit-mono local stack)

From this project root:

```bash
docker build -t conduitl2:local .
```

## Deploy to Fly.io

This repository now includes a `fly.toml` tuned for production relay rollout:

- VM preset: `performance-1x`
- Memory per Machine: `4gb`
- Fly Proxy edge handlers on `80`/`443` mapped to internal `3334`

TLS and reverse-proxy behavior:

- `wss://conduitl2.fly.dev` is served through Fly Proxy TLS termination on port `443`
- The app only listens on plain HTTP/WebSocket on `PORT=3334`; no app-level TLS config is required
- Port `80` is redirected to HTTPS

For a custom domain (for example `relay.example.com`), Fly manages certificate issuance after DNS is configured:

```bash
fly certs add relay.example.com -a conduitl2
fly certs check relay.example.com -a conduitl2
```

Apply the DNS records shown by `fly certs add` (typically `A`, `AAAA`, or `CNAME`) at your DNS provider.

Typical deploy flow:

```bash
fly deploy -a conduitl2 --image registry.fly.io/conduitl2:<deployment-tag>
fly scale count 2 -a conduitl2
```

You can confirm sizing and Machine count with:

```bash
fly scale show -a conduitl2
```

You can verify secure relay connectivity with:

```bash
nak relay wss://conduitl2.fly.dev
```

## Run the demo relay

From this project root:

```bash
go run ./cmd/demo
```

Relay endpoint: `ws://127.0.0.1:3334`

## Quick `nak` showcase

1) Check relay capabilities (`conduit_l2` appears in NIP-11 `tags`):

```bash
nak relay ws://127.0.0.1:3334
curl -sS -H 'Accept: application/nostr+json' http://127.0.0.1:3334
```

2) Generate a key:

```bash
nak key generate
```

3) Publish product events (`kind:30402`):

```bash
export NOSTR_SECRET_KEY='<your nsec or hex secret key>'
nak event --sec "$NOSTR_SECRET_KEY" -k 30402 -d apple  -c '{"title":"Apple","summary":"Red fruit","price":19,"currency":"USD","updatedAt":1000}' ws://127.0.0.1:3334
nak event --sec "$NOSTR_SECRET_KEY" -k 30402 -d banana -c '{"title":"Banana","summary":"Yellow fruit","price":7,"currency":"USD","updatedAt":1001}' ws://127.0.0.1:3334
nak event --sec "$NOSTR_SECRET_KEY" -k 30402 -d carrot -c '{"title":"Carrot","summary":"Orange root","price":12,"currency":"USD","updatedAt":1002}' ws://127.0.0.1:3334
```

4) Query products with deterministic Scope 2 sort:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=;sort=price_asc' ws://127.0.0.1:3334
```

Price sorting is only allowed when the matched products are comparable. Mixed-currency results require either trusted normalization or `partial=1`.

5) Query products with text search:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=apple;sort=newest' ws://127.0.0.1:3334
```

6) Query a mixed-currency set with deterministic partial results:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=;sort=price_asc;partial=1' ws://127.0.0.1:3334
```

7) Show protected read behavior for `kind:1059`:

```bash
nak req -k 1059 ws://127.0.0.1:3334
```

Without NIP-42 auth, this should return an `auth-required` closure.

Deleted product revisions are filtered out of accelerated browse results. This package currently advertises browse/search/sort behavior plus protected `kind:1059` gating; it does not advertise separate product-detail or profile-lookup acceleration.

## One-command demo and validation

From this project root:

```bash
./run_demo.sh
```

The script starts the demo relay, publishes test products, validates sorting and text filtering, and checks auth-gating for `kind:1059`.
