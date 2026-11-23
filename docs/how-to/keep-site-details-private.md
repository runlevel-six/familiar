# Keep site details out of the repository

This repository is public; anything specific to your network or the people
around it belongs in the gitignored `local/` directory
([why](../explanation/public-and-private.md)). A pre-commit check keeps the two
apart.

## Set up a clone

```sh
git config core.hooksPath .githooks
mkdir -p local && git -C local init        # if you don't have a local/ yet
```

`.githooks/pre-commit` runs [`scripts/check-private.sh`](../../scripts/check-private.sh)
on the staged changes and, if it's installed, `betterleaks` for secrets.
`.githooks/commit-msg` runs the same check on the commit message.

## Write the denylist

`local/denylist.txt` lists the words that must never appear in a tracked file or a
commit message, one per line, `#` for comments. Matching is case-insensitive and
whole-word, using fixed strings. Put in your domain and node names, LAN address
prefixes, the names of the apps on your cluster, and people's names and email
addresses.

Whole-word matching misses a term joined to another word by an underscore
(`mynode_backup` doesn't match `mynode`), so add those forms too.

Without a denylist (for example in CI), the check only makes sure nothing under
`local/` is tracked.

## Check by hand

```sh
scripts/check-private.sh                  # staged changes (what the hook runs)
scripts/check-private.sh --all            # every tracked file
scripts/check-private.sh --range A..B     # a range of commits and their messages
betterleaks dir --redact .                # secrets in the working tree
```

A deliberate fake secret in a test can be marked with a trailing
`// gitleaks:allow` comment.

## Where things go

- **Site values in manifests:** keep a placeholder in the base and set the real
  value in your overlay; see [`deploy/overlays/example`](../../deploy/overlays/example).
- **Site values in code:** read them from configuration with a generic default.
- **Operational notes** (runbooks, inventory, incidents): `local/docs/`.
- **Public text:** describe it generically ("an app's database", "the node") and
  keep the specifics in `local/`.

## If something private gets pushed

Stop and remove it from the files first. If it reached a public commit, deleting
the file isn't enough: the commit stays reachable. Decide whether to rewrite the
history (and ask GitHub to purge cached views) or to rotate whatever leaked.

## New source files

New code and manifest files start with an `SPDX-License-Identifier: Apache-2.0`
header, like the existing ones.
