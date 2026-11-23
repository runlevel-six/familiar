# SPDX-License-Identifier: Apache-2.0
# All four read-only MCP tool servers in one static, distroless, non-root
# image (design §4.1, T8). Base images pinned by multi-arch index digest
# (resolved 2026-11-22; see model-gate/Dockerfile for the tags).
FROM --platform=$BUILDPLATFORM docker.io/library/golang@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
WORKDIR /src
COPY go.mod ./
COPY internal ./internal
COPY cmd ./cmd
ARG TARGETOS TARGETARCH VERSION=dev
RUN go vet ./... && go test ./... && \
    for c in cluster-read metrics-query ceph-health cert-probe; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/$c ./cmd/$c || exit 1; \
    done

FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/ /
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/runlevel-six/familiar" \
      org.opencontainers.image.title="familiar-tools" \
      org.opencontainers.image.description="Read-only MCP tool servers for familiar: cluster-read, metrics-query, ceph-health, cert-probe" \
      org.opencontainers.image.version="${VERSION}"
USER 65532:65532
