# Build and publish the images

Two images: `ghcr.io/runlevel-six/model-gate` and
`ghcr.io/runlevel-six/familiar-tools` (one image holding all four tool servers).
Both are static Go binaries on distroless, built for amd64 and arm64. Each build
runs `go vet` and the tests first, so a failing test fails the build.

The manifests pin images by digest, never by tag alone.

## Build

With Podman (Docker's `buildx` works the same way):

```sh
TAG=0.2.1-$(git rev-parse --short HEAD)
for img in model-gate:model-gate familiar-tools:tools; do
  name=${img%%:*}; dir=${img##*:}
  podman build --platform linux/amd64,linux/arm64 --build-arg VERSION=$TAG \
    --manifest ghcr.io/runlevel-six/$name:$TAG $dir
done
```

Tags are `<version>-<commit>`. Bump the minor version when a configuration
change is incompatible (model-gate 0.2.0 made `allowed_betas` required).

## Push and pin

```sh
podman manifest push --all --digestfile model-gate.digest \
  ghcr.io/runlevel-six/model-gate:$TAG docker://ghcr.io/runlevel-six/model-gate:$TAG
```

Then put the digest in the manifests, with the tag as a comment:

```yaml
image: ghcr.io/runlevel-six/model-gate@sha256:… # 0.2.1-abc1234
```

model-gate is pinned in `deploy/familiar/10-model-gate.yaml`, the tool servers in
`deploy/familiar/20-tool-servers.yaml` (four places).

The packages are public, so the cluster pulls without credentials. Check a new
digest pulls anonymously before applying.

## Check what you ship

The images are public. Before pushing, make sure no site-specific string was
compiled in: build from a clean checkout and look through the binaries' strings
for your denylisted terms (see
[Keep site details out of the repository](keep-site-details-private.md)).
