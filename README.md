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

4) Query products with Scope 2 sort:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=;sort=price_asc' ws://127.0.0.1:3334
```

5) Query products with text search:

```bash
nak req -k 30402 -l 10 --search 'conduit-l2:q=apple;sort=newest' ws://127.0.0.1:3334
```

6) Show protected read behavior for `kind:1059`:

```bash
nak req -k 1059 ws://127.0.0.1:3334
```

Without NIP-42 auth, this should return an `auth-required` closure.

## One-command demo and validation

From this project root:

```bash
./run_demo.sh
```

The script starts the demo relay, publishes test products, validates sorting and text filtering, and checks auth-gating for `kind:1059`.
