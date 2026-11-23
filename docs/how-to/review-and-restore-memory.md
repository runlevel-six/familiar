# Review and restore GIDEON's memory

memory-auditor commits a normalised copy of the agent's persistent state to a git
repository on its own volume every five minutes: its memories, its scheduled jobs
(definitions only), its config, and an inventory of any other file on its volume.
The agent can't reach that repository. Memory poisoning can't be prevented, since
the agent has to be able to write its memory, so this is how you find it and
undo it ([why](../explanation/security-model.md)).

## Review what changed

```sh
kubectl -n familiar-agents exec deploy/memory-auditor -- git -C /audit/repo log -p -5
kubectl -n familiar-agents exec deploy/memory-auditor -- git -C /audit/repo log -p -- memories
```

The repository has four areas:

| Area | What's recorded | Alert on change |
|---|---|---|
| `memories/` | `MEMORY.md`, `USER.md` | `GideonMemoryChanged` (info): expected, review weekly |
| `cron/` | Job definitions from `cron/jobs.json`, run state stripped | `GideonControlStateChanged` (warning) |
| `config/` | `config.yaml`, and whether it differs from the ConfigMap copy | `GideonControlStateChanged` (warning) |
| `files/` | Path, size and SHA-256 of every other file on the volume, except known noisy ones | `GideonControlStateChanged` (warning) |

What counts as noisy, and what's never read (secrets, the Matrix keys), is in the
[memory-auditor reference](../reference/memory-auditor.md).

Look for instructions in memory that you didn't give, jobs you didn't create,
and files outside Hermes' own layout, especially executables.

## Restore a known-good state

1. Find the last good commit:

   ```sh
   kubectl -n familiar-agents exec deploy/memory-auditor -- git -C /audit/repo log --oneline -- memories
   ```

2. Stop the agent:

   ```sh
   kubectl -n familiar-agents scale deploy/hermes --replicas=0
   ```

3. Copy the files back. memory-auditor mounts the volume read-only, so start a
   one-off pod that mounts the `hermes-data` claim read-write as uid 10000, and
   stream each file into it from the auditor's repository:

   ```sh
   kubectl -n familiar-agents exec deploy/memory-auditor -- \
     git -C /audit/repo show <commit>:memories/MEMORY.md \
   | kubectl -n familiar-agents exec -i <restore-pod> -- sh -c 'cat > /opt/data/memories/MEMORY.md'
   ```

   Repeat for `USER.md`. The repository holds normalised copies, so check the
   result reads as expected.

4. Compare the agent's scheduled jobs with `cron/jobs.json` at the same commit,
   and delete or recreate any that differ with `hermes cron`.

5. Delete the restore pod and start the agent:

   ```sh
   kubectl -n familiar-agents scale deploy/hermes --replicas=1
   ```

The next audit run records the restored state as a change.
