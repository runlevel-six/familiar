# Public repository, private details

familiar is built for one home cluster, but nothing about its design is specific
to it. So the repository is public and holds the design, the code and generic
manifests, while everything that describes one particular network or the people
around it stays private: hostnames and domains, addresses, Matrix IDs, which
applications run on the cluster, node access, hardware identifiers, runbooks,
the cluster inventory and incident notes.

None of that is a credential, but together it's a ready-made reconnaissance map
of a home network. Keeping it out of a public repository costs little if it's
done from the start.

## How it's split

The private side lives in a `local/` directory at the top of the working tree.
It's gitignored by this repository and kept as its own git repository, which is
never pushed anywhere. It holds the real kustomize overlay for
[`deploy/familiar`](../../deploy/familiar), the deployments of the surrounding
infrastructure (monitoring, Matrix, database dumps), host scripts and the
operational documents. Public files may refer to paths under `local/`; a reader
without it sees what exists, not what it says.

The public manifests are a [kustomize base](../../deploy/familiar/kustomization.yaml)
with `example.org` placeholders where a site's values go, and a
[worked example overlay](../../deploy/overlays/example). Code that needs a
site-specific value takes it from configuration (for example metrics-query's
`BACKUP_METRIC`). Kubernetes label keys use the prefix `familiar/` rather than a
domain.

A pre-commit check enforces the split: see
[Keep site details out of the repository](../how-to/keep-site-details-private.md).

## History

The repository's history was squashed into a single commit before it went public
on 2026-11-23, because the earlier history contained everything that now lives
in `local/`. A force push wouldn't have been enough: GitHub keeps commits that no
branch points to reachable by their SHA, and the image tags (`0.2.0-86b0d42`, …)
name some of those SHAs. So the old repository was deleted and re-created. The
full history is kept privately as a git bundle in `local/`. Image tags may
therefore name commits that don't exist in this repository, and image versions
built from the old history that contained site names were deleted from the
registry the same day.
