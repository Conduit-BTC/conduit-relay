# conduit-relay

Two Go modules for different relay generations.

## Production — `congee/`

The relay deployed to production (Fly.io `conduit-congee`) builds from
`congee/` via `congee/Dockerfile`. Source provenance, release, and
deployment steps live in [docs/CONGEE_SOURCE.md](docs/CONGEE_SOURCE.md).

## Retired — `legacy/` (deprecated)

`legacy/` holds the retired Conduit L2 relay (khatru + Scope 2 product
browse + NIP-42 gift-wrap protection), formerly deployed as `conduitl2`.
It is not deployed anywhere and receives no maintenance; it is kept for
reference only. Its build and deploy configuration (`legacy/Dockerfile`,
`legacy/fly.toml`) remains so the build can be reproduced — do not deploy
it to new apps. See `legacy/README.md`.

Production coverage of the functionality that originated here —
recipient-protected messaging and marketplace product sync — lives in the
Congee baseline and its deployed plugins.
