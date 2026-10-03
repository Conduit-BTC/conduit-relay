# syntax=docker/dockerfile:1

# Build the static admin UI on the builder's native platform. npm/esbuild must
# not run under qemu (linux/arm64 multi-arch CI hits ETXTBSY in esbuild's
# postinstall). The output is architecture-independent.
FROM --platform=$BUILDPLATFORM node:24-bookworm AS admin-ui
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN npm ci && npm audit --audit-level=high
COPY web/admin/ ./
RUN npm run build

FROM golang:1.27.1-bookworm AS go-build
WORKDIR /src
RUN apt-get update \
	&& apt-get install -y --no-install-recommends gcc \
	&& rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
COPY sdk/plugin/go.mod sdk/plugin/go.sum ./sdk/plugin/
RUN go mod download
COPY . .
COPY --from=admin-ui /src/web/admin/build ./web/admin/build
ENV CGO_ENABLED=1
ARG VERSION
RUN test -n "$VERSION" && go build -trimpath -ldflags "-X github.com/michmich112/congee/internal/version.Version=${VERSION}" -o /out/congee ./cmd/congee

FROM debian:bookworm-slim
ARG VERSION
ARG GIT_REVISION
LABEL org.opencontainers.image.title="Congee" \
	org.opencontainers.image.description="Nostr relay" \
	org.opencontainers.image.version="${VERSION}" \
	org.opencontainers.image.revision="${GIT_REVISION}" \
	org.opencontainers.image.source="https://github.com/Conduit-BTC/conduit-relay"
RUN test -n "$VERSION" && test "${#GIT_REVISION}" -eq 40 \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates libgomp1 libstdc++6 \
	&& rm -rf /var/lib/apt/lists/*
WORKDIR /
# Admin UI is served from web/admin/build relative to the process working directory (WORKDIR /).
COPY --from=go-build /src/web/admin/build /web/admin/build
COPY --from=go-build /out/congee /usr/local/bin/congee
ENV CONGEE_DATA_DIR=/data
EXPOSE 3334 3335
VOLUME ["/data"]
ENTRYPOINT ["congee"]
