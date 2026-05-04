# deep-loom-kit Backend Reference

## state (default)

Files live in LangGraph checkpointed state. Ephemeral per-thread. Zero setup.

```yaml
# Omit backend entirely, or explicitly:
backend: state
```

Use when: pure reasoning, no persistent I/O needed.

---

## filesystem

Real files on disk. Persists across restarts.

```yaml
backend:
  type: filesystem
  root_dir: ./workspace     # relative to service dir, or absolute
  virtual_mode: true        # recommended — sandboxes paths under root_dir
  isolation:                # optional
    mode: thread_id         # none | thread_id | context
```

Use when: files need to survive between sessions but shell execution is not needed.

---

## local_shell

Filesystem + shell command execution on the host (`run_command` tool available).
Local dev / trusted CI only.

```yaml
backend:
  type: local_shell
  root_dir: ./workspace
  virtual_mode: true
  timeout: 120              # max seconds per shell command (must be > 0)
  max_output_bytes: 100000  # truncate large stdout (must be > 0)
  inherit_env: true         # recommended — gives agent PATH, system tools
  env:                      # optional: inject specific env vars
    MY_VAR: value
  isolation:                # optional
    mode: thread_id
```

Use when: agent needs to run shell commands (build, test, grep, sed, etc.) or write real files.

---

## store (durable key-value)

Persists across threads via LangGraph BaseStore. Good for memories and shared knowledge.

```yaml
backend: store

# For production persistence, also add at agent level in orchestrator.yaml:
store:
  type: postgres
  connection_string_env: DATABASE_URL   # env var NAME (not value)
  pool_size: 5
  max_overflow: 10
```

Requires: `pip install deep-loom-kit[postgres]` and `DATABASE_URL` env var.
Use when: data must be shared or remembered across separate conversation threads.

---

## s3

Files stored in Amazon S3 or compatible (MinIO). Credentials from env only.

```yaml
backend:
  type: s3
  bucket: my-bucket
  prefix: my-service/       # optional key prefix
```

Required env vars: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_DEFAULT_REGION`
Optional env vars: `AWS_ENDPOINT_URL` (MinIO/custom endpoint), `S3_USE_SSL`

Use when: files need cloud-scale storage or cross-deployment sharing.

---

## composite_backend

Route different virtual path prefixes to different backends.

```yaml
composite_backend:
  isolation:                    # optional top-level isolation applied to all routes
    mode: thread_id
  default:                      # backend for unmatched paths
    type: local_shell
    root_dir: ./workspace
    virtual_mode: true
    timeout: 120
    max_output_bytes: 100000
    inherit_env: true
  routes:
    documents:                  # /documents/* → separate filesystem root
      type: filesystem
      root_dir: /data/docs
      virtual_mode: true
      include_route: false      # omit route name from path (default: true)
      isolation:
        mode: context
        variable: project_id
    memories: store             # /memories/* → durable store (string shorthand)
    shell: local_shell          # /shell/* → local shell (string shorthand)
```

Use when: different parts of the agent's storage have different persistence / access needs.

---

## Isolation modes

```yaml
isolation:
  mode: thread_id     # one namespace per conversation thread (most common)
  mode: context       # one namespace per value of context.<variable>
  variable: tenant_id # required when mode: context
  mode: none          # shared storage (default when isolation key is omitted)
```

`include_route: false` — suppresses the route name from the storage path (composite only).

**Path formula:**

| Config | Isolation | Effective path |
|---|---|---|
| `root_dir: /data`, no isolation | — | `/data/` |
| `root_dir: /data`, `mode: thread_id` | thread `t-1` | `/data/t-1/` |
| Composite route `documents`, `root_dir: /data`, `include_route: true` | — | `/data/documents/` |
| Composite route `documents`, `root_dir: /data`, `include_route: false` | — | `/data/` |
