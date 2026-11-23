# SPDX-License-Identifier: Apache-2.0
# model-gate: static binary on distroless, non-root (design §4.1, T8).
# Base images pinned by multi-arch index digest (resolved 2026-11-22):
#   golang:1.27.3                      sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e
#   distroless/static-debian12:nonroot sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
# The build stage runs on the build machine's arch and cross-compiles.
FROM --platform=$BUILDPLATFORM docker.io/library/golang@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
# The tests check the example config loads and matches what Hermes sends.
COPY config.example.json ./
ARG TARGETOS TARGETARCH VERSION=dev
RUN go vet ./... && go test ./... && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /model-gate .

FROM gcr.io/distroless/static-debian12@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /model-gate /model-gate
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/runlevel-six/familiar" \
      org.opencontainers.image.title="model-gate" \
      org.opencontainers.image.description="Holds the Anthropic API keys for familiar; meters and enforces budgets" \
      org.opencontainers.image.version="${VERSION}"
USER 65532:65532
ENTRYPOINT ["/model-gate"]
CMD ["-config", "/etc/model-gate/config.json"]
