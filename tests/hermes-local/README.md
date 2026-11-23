# Local watch-officer harness

Runs Hermes, model-gate, the tool servers and memory-auditor with Podman, isolated
like the cluster isolates them, against a real cluster through a read-only
kubeconfig.

```sh
./run.sh up      # build the images, start everything
./run.sh down    # remove the containers (keeps volumes, images, secrets)
./run.sh purge   # also remove volumes, networks and the Podman secrets
```

- [Run the watch officer locally](../../docs/tutorials/run-the-watch-officer-locally.md):
  the walkthrough, including setup.
- [What the local Hermes test showed](../../docs/explanation/local-test-findings.md):
  results, including the Matrix round (`matrix/testclient.py` is the stand-in
  phone used there).
