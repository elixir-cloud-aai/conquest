# Conquest

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)

Workflow execution service. Runs Nextflow, WDL, and Snakemake
pipelines on Kubernetes through GA4GH-standard APIs.

Conquest implements a [WES](https://ga4gh.github.io/workflow-execution-service-schemas/) (Workflow Execution Service) control plane
with an internal [TES](https://ga4gh.github.io/task-execution-schemas/) (Task Execution Service) backend. A single Go binary
handles all roles — API server, workflow runner, and task file staging —
depending on how it's invoked.

## Architecture

```
                           +----------+
                           |  Client  |
                           +----+-----+
                                |
                           WES API v1.1
                                |
  +-----------+     +-----------v----------+     +----------+
  |  Adapter  |<----|    Run Controller    |---->| Postgres |
  |  Registry |     |                      |<----|  (state) |
  |  NF / SMK |     +-----------+----------+     +----^-----+
  +-----------+                 |                      |
                                | K8s Job              | poll
                      +---------v----------+     +-----+------+
                      |    Runner Pod      |     | Reconciler |
                      | (NF / WDL / SMK)   |     | (K8s > DB) |
                      +---------+----------+     +------------+
                                |
                         TES v1.1 (internal)
                                |
                      +---------v----------+
                      |   Task K8s Jobs    |
                      |                    |
                      |  init: stage-in    |
                      |  main: executor    |
                      |  post: stage-out   |
                      +---------+----------+
                                |
                      +---------v----------+
                      |     S3 / MinIO     |
                      |   (object store)   |
                      +--------------------+
```

**How a workflow runs:**

1. Client submits a workflow via the **WES API**.
2. The **Run Controller** picks the right engine adapter, persists the run in
   Postgres, and creates a **Runner Job** on Kubernetes.
3. The runner executes the workflow engine (Nextflow, WDL, or Snakemake), which
   submits individual tasks back to Conquest's **internal TES API**.
4. Each task becomes a **K8s Job** — an init container stages inputs from S3,
   the executor runs the user command, then outputs are uploaded back to S3.
5. **Reconcilers** poll Kubernetes in the background and sync Job/Pod state
   back to Postgres.

Postgres is the system of record. S3 is the data plane. Kubernetes is the
compute layer.

## Supported Engines

| Engine    | Version  | Default | Execution Mode                                       |
|-----------|----------|---------|------------------------------------------------------|
| Nextflow  | 24.10.4  | Yes     | TES dispatch via [`nf-ga4gh`][nf-ga4gh] plugin       |
| Nextflow  | 25.04.3  |         | TES dispatch via [`nf-ga4gh`][nf-ga4gh] plugin       |
| Nextflow  | 25.10.2  |         | TES dispatch via [`nf-ga4gh`][nf-ga4gh] plugin       |
| Nextflow  | 25.10.4  |         | TES dispatch via [`nf-ga4gh`][nf-ga4gh] plugin       |
| WDL       | 1.12.1   | Yes     | [`miniwdl`][miniwdl] runner with TES-dispatch container backend |
| Snakemake | 9.17.3   | Yes     | TES dispatch via `conquest-tes` executor plugin      |
| Snakemake | 7.32.4   |         | Local execution in runner pod                        |

[nf-ga4gh]: https://github.com/nextflow-io/nf-ga4gh
[miniwdl]: https://miniwdl.readthedocs.io/

Nextflow versions use the upstream [`nf-ga4gh`][nf-ga4gh] plugin to submit
tasks to Conquest's TES API.

WDL uses a custom [`miniwdl`][miniwdl] container backend plugin (`python/miniwdl_conquest_tes`)
which dispatches every WDL task to Conquest's internal TES API. The WDL engine
name is `wdl`, and the current supported WDL language versions are `1.0` and
`1.1`.

The `conquest-tes` executor plugin (under `python/` in this repo) is a custom
Snakemake executor based on the upstream
[`snakemake-executor-plugin-tes`](https://github.com/snakemake/snakemake-executor-plugin-tes),
adapted for Conquest's internal TES API and S3 staging conventions.

Snakemake 7.x predates the executor plugin system
([introduced in 8.0](https://snakemake.readthedocs.io/en/stable/getting_started/migration.html)),
so the `conquest-tes` plugin cannot be used. The entire workflow runs locally
inside the runner pod rather than being broken into individual TES tasks.

Each engine version gets its own Docker image with pinned dependencies. The
version is selected at submission time via `workflow_engine_version` in the WES
request — if omitted, the default is used.

## Task Resource Control

All supported engines dispatch work through TES, so users control CPU, memory,
and disk for each task pod directly in their workflow. Nextflow uses process
directives (`cpus`, `memory`, `disk`); WDL uses task runtime attributes such as
`cpu`, `memory`, `disks`/`disk`, and `docker`; Snakemake uses rule resources
(`cpus`, `mem_gb`, `disk_gb`). The engine plugin translates these to TES
resource fields, and Conquest maps them to Kubernetes resource requests/limits.

A task policy layer fills in defaults when omitted (1 CPU, 2 GB RAM, 8 GB
disk) and enforces platform-level floors and ceilings. Task memory is floored
at 128 MiB by default (`CONQUEST_TASK_MIN_RAM_GB`): workflows compute memory
from input sizes, and a container can't start under a few MiB. See
`CONQUEST_TASK_DEFAULT_*`, `CONQUEST_TASK_MIN_*`, and `CONQUEST_TASK_MAX_*`
in [`internal/config/config.go`](internal/config/config.go).

These resources apply to the executor container. The task pod's helper init
containers (`copy-tools`, `stage-in`) get none by default, so a namespace
LimitRange, if any, fills them in. Where a ResourceQuota requires requests or
limits on every container and no LimitRange provides them, the API server
refuses such pods (`must specify limits.cpu for: copy-tools,stage-in`): set
`CONQUEST_TASK_HELPER_RESOURCES` to JSON in the shape of a container's
`resources`, e.g.
`{"requests":{"cpu":"100m","memory":"128Mi"},"limits":{"cpu":"500m","memory":"256Mi"}}`
(chart: `conquest.taskHelperResources`; the chart's `migrate` init container
takes `conquest.migration.resources`, the objectfs CSI driver containers
`objectfs.resources`). Conquest refuses to start if the value
isn't valid (only `cpu`, `memory` and `ephemeral-storage` are accepted).

## Workspace Modes

Controls how the runner pod and task pods share (or don't share) a filesystem.

| Mode          | Volume Type          | Shared Filesystem | Notes                                                                                                     |
|---------------|----------------------|-------------------|-----------------------------------------------------------------------------------------------------------|
| `objectfs`    | S3 via FUSE          | Yes               | Run workspace is an S3 prefix mounted into every pod. No block storage needed. See below.                  |
| `pvc` + `RWX` | PVC (ReadWriteMany)  | Yes               | Requires NFS or a similar StorageClass.                                                                   |
| `pvc` + `RWO` | PVC (ReadWriteOnce)  | Yes               | Good fallback when NFS isn't available. Co-locates tasks on the same node as the runner via pod affinity. |
| `emptydir`    | Kubernetes emptyDir  | No                | Each task stages inputs from S3 and uploads outputs back to S3 independently. Works for Nextflow, Snakemake 8+ and WDL (each WDL task's inputs, command and work directory go through S3, and the work directory is downloaded back to the runner for miniwdl). The mode for federated runs. |

```bash
CONQUEST_WORKSPACE_MODE=objectfs     # "emptydir", "pvc", or "objectfs"
CONQUEST_WORKSPACE_ACCESS_MODE=RWX   # "RWX" or "RWO" (pvc only)
CONQUEST_WORKSPACE_SIZE=10Gi
```

### `objectfs` — S3 as the workspace filesystem

`objectfs` gives the runner and every task pod one shared POSIX filesystem
that *is* an S3 prefix. It needs no NFS, no RWX StorageClass and no block
storage, which makes it the mode that works on clusters where the other
shared options are unavailable.

**How it is put together.** Conquest creates a static `PersistentVolume` per
run whose `volumeHandle` is the run's own S3 prefix, plus a matching PVC:

```
/workspace/results/a.bam
  -> s3://<bucket>/v1/workflow-runs/<runID>/workspace/results/a.bam
```

The mount is performed by [GeeseFS][geesefs] (Apache 2.0) through the
[Yandex S3 CSI driver][csi-s3] (Apache 2.0). Both are implementation detail:
the only user-facing surface is `CONQUEST_WORKSPACE_MODE=objectfs`.

The layout is deliberately transparent — one object per file, human-readable
keys, no chunking or opaque metadata store. Anything written through the
mount can be read with `mc`, the AWS SDK or the MinIO console, and anything
uploaded by other means appears in the mount. This is why JuiceFS and
similar chunk-plus-metadata designs were rejected despite better POSIX
fidelity.

**Mount options and why each is required** (`internal/backend/k8s/workspace.go`):

| Flag | Why |
|---|---|
| `--no-systemd` | Talos has no systemd; GeeseFS must not try to register a unit. |
| `--dir-mode 0777` / `--file-mode 0666` | Fallback modes for objects with no stored mode. |
| `--enable-perms` | Stores real modes as `x-amz-meta-mode`. Without it `chmod()` is silently discarded and executable bits on `bin/` scripts are lost. |
| `--enable-mtime` | Preserves mtime. Without it miniwdl dies in `bump_atime` with `EPERM`, and Snakemake's mtime-driven DAG logic is unreliable. |
| `--enable-specials` | Enables named pipes. GeeseFS only auto-enables this for endpoints containing `yandex`, so it must be explicit on MinIO. Without it STAR cannot create its FIFO and every `STAR_ALIGN` task fails. |

The last three all carry the same upstream caveat — they need the S3 backend
to return `UserMetadata` in listings. MinIO does; verified by round-tripping
mode, mtime and file type across a remount, at no measurable listing cost.

Conquest also clears the process `umask` in the runner and in every task
executor when the workspace is shared (`internal/sharedfs`). The runner is
typically root while task images often run as a non-root UID; with the
default `umask 022` the runner's files land `0644`/`0755` and tasks get
`EACCES`. See that package for the full reasoning.

**Known limitations.** These are properties of S3-backed FUSE, not bugs:

- No hard links (`ENOTSUP`). Harmless here — the WDL runner sets
  `output_hardlinks = false`, so miniwdl uses symlinks, which do work.
- No "invisible" deleted files: unlinking a file that is still open yields
  `ESTALE`, so `tempfile.NamedTemporaryFile(delete=True)` patterns break.
- Concurrent writes to the same object from multiple pods are unsafe.
- Advisory locks (`flock`, `fcntl` `F_SETLK`) are handled locally by the
  kernel per mount, so they serialise within a pod but not across pods.

**Cleanup.** When a run reaches a terminal state Conquest deletes the run's
Jobs, then the PVC and PV. **S3 objects are deliberately retained** — data
lifecycle is separate from workspace lifecycle. The Job deletion has to come
first: `pvc-protection` blocks PVC deletion while any Pod still *references*
the claim, including Pods that have already completed.

[geesefs]: https://github.com/yandex-cloud/geesefs
[csi-s3]: https://github.com/yandex-cloud/k8s-csi-s3

## Pod Security

Runner and task pods satisfy the [restricted Pod Security Standard][pss] by
default (`CONQUEST_POD_SECURITY=restricted`), so Conquest runs in namespaces
labelled `pod-security.kubernetes.io/enforce=restricted`. Every container runs
as `CONQUEST_POD_RUN_AS_USER`:`CONQUEST_POD_RUN_AS_GROUP` (default
`1000:1000`, also the pod `fsGroup`) with seccomp `RuntimeDefault`, no
privilege escalation and all capabilities dropped.

Executor images therefore run as that UID, not the `USER` they declare. This
is the same model as nf-core's docker profile (`-u $(id -u):$(id -g)`), which
nf-core modules are built to run under. Tools that need root, or write to a
root-owned `$HOME`, will fail. Set `CONQUEST_POD_SECURITY=baseline` to let executors run as their
image's user (often root) on clusters that allow it.

Snakemake has a separate task image, `conquest-smk-9.17.3-task`: the runner
image (`docker/smk-runner/Dockerfile`, target `task`) as root, for tasks on
remote TES servers, which stage task files as their own user and run executors
as the image's user. Conquest's own task pods run as
`CONQUEST_POD_RUN_AS_USER` under `restricted` regardless; the runner image
stays non-root (UID 1000), like Conquest's other images.

In both profiles, runner and task pods do not mount a ServiceAccount token.
Only the server talks to the Kubernetes API, so workflow code never holds
the Conquest ServiceAccount's rights to create and delete Jobs and PVCs.
`internal/backend/k8s/podsecurity_test.go` checks every generated pod against
the upstream Pod Security Admission checks.

[pss]: https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted

## Storage Isolation

By default every task pod gets the bucket-wide S3 credentials: its stage-in
container and the filer that uploads outputs after the executor need them,
and the executor (the user's image and command) can read them too. Any task,
and so anyone who may submit a run, can then read and write every run's
objects.

With `CONQUEST_STORAGE_ISOLATION=true` (chart: `storageIsolation.enabled`),
task pods get no S3 credentials and no S3 endpoint. Each gets one capability
URL of the [storage gateway](#federation-remote-tes-endpoints), on Conquest's
in-cluster address, in `CONQUEST_TASK_STORAGE_URL`:

```
<CONQUEST_INTERNAL_BASE_URL>/storage/v1/t/<task ID>/<HMAC>
```

The executor (the user's code) can read this URL too; it reaches only what
the task itself may. The filer reads `<that URL>/<bucket>/<key>` and follows the `307` to an S3 URL
presigned for that object and method; a `GET` of a prefix URL (ending in `/`)
below a `DIRECTORY` input lists the keys there. What a task may reach is
decided from its stored TES request as for remote endpoints, and is bounded to
its run's root, `s3://<bucket>/<layout version>/workflow-runs/<run ID>/`: a
request that names another run's objects gets `403`. The bound applies to the
tasks of remote endpoints with `s3_url_style: gateway` as well (endpoints with
their own access to the bucket are not bounded). URLs stop working shortly
after the task ends.

- Needs `CONQUEST_STORAGE_GATEWAY_SECRET` (the chart reuses
  `storageGateway.existingSecret` or generates one) and the server's own S3
  credentials; `CONQUEST_STORAGE_GATEWAY_URL` is needed only for remote
  endpoints.
- Task pods must reach Conquest's Service at `CONQUEST_INTERNAL_BASE_URL`
  (the chart sets it; its built-in default fits only the `services`
  namespace) and the presign endpoint: `CONQUEST_S3_PUBLIC_ENDPOINT` if set,
  else `CONQUEST_S3_ENDPOINT`.
- `s3://` inputs and outputs must lie below the run's root. Inputs elsewhere
  in the bucket (e.g. shared reference data) are refused; standalone TES
  tasks without a `conquest_run_id` tag reach only their own stub run's
  prefix. Listings answer at most 100000 keys (`413` beyond).
- The bound is only as strong as the task's run ID, which comes from the
  caller: the runner callback API (`/internal/runs/<run ID>/...`) and the
  `conquest_run_id` tag on the public TES API attach a task to a run. With
  `CONQUEST_RUNNER_SECRET` set (always with the chart), the callback API
  takes only the runner token of the run in its path (see
  [Runner callbacks](#runner-callbacks)); with authentication on, the tag
  attaches tasks only to the caller's own runs (see Run owners). Without
  them, isolation holds only where callers that can reach those APIs are
  trusted.
- The bound uses the current `CONQUEST_LAYOUT_VERSION`: drain runs before
  changing it.
- Task staging depends on Conquest being up: the filer retries connection
  errors and `429`/`5xx` for about a minute, so a restart or a rolling
  upgrade is fine, a longer outage fails the tasks that stage meanwhile.
- Uploads are single presigned `PUT`s, so S3's limit for one `PUT` applies to
  each output file (5 GiB on AWS and Ceph RGW by default).
- Not covered: runner pods. The engines read and write the run's work
  directory with their own S3 clients, so the runner still gets the
  credentials, and workflow code that runs there (Nextflow scripts, Snakefiles)
  can read them.

## GA4GH APIs

Conquest implements both GA4GH v1.1 specifications.

**WES** is the external-facing API for submitting and managing workflow runs:

| Endpoint                               | Method | Description                |
|----------------------------------------|--------|----------------------------|
| `/ga4gh/wes/v1/service-info`           | GET    | Service info + state counts|
| `/ga4gh/wes/v1/runs`                   | POST   | Submit a workflow run      |
| `/ga4gh/wes/v1/runs`                   | GET    | List runs (paginated)      |
| `/ga4gh/wes/v1/runs/{id}`              | GET    | Full run log               |
| `/ga4gh/wes/v1/runs/{id}/status`       | GET    | Run state only             |
| `/ga4gh/wes/v1/runs/{id}/cancel`       | POST   | Cancel a run               |
| `/ga4gh/wes/v1/runs/{id}/tasks`        | GET    | List tasks for a run       |
| `/ga4gh/wes/v1/runs/{id}/tasks/{tid}`  | GET    | Get task details           |

**TES** is used internally by the workflow runners, but also exposed publicly:

| Endpoint                               | Method | Description     |
|----------------------------------------|--------|-----------------|
| `/ga4gh/tes/v1/tasks`                  | POST   | Create a task   |
| `/ga4gh/tes/v1/tasks`                  | GET    | List tasks      |
| `/ga4gh/tes/v1/tasks/{id}`             | GET    | Get task        |
| `/ga4gh/tes/v1/tasks/{id}:cancel`      | POST   | Cancel a task   |

Conquest manages the full task lifecycle — data staging, resource allocation,
state tracking, and log capture. The workflow engine submits tasks; Conquest
handles the rest.

### Run owners

Every run can record the caller that created it (`runs.owner`, migration
000009; package `internal/ownership`). When a request's context is scoped to
an owner, new runs are stamped with that owner, and the repository returns
only that owner's runs and their tasks: another owner's run or task, or one
created without an owner, reads as not found. A context without an owner is
unscoped and sees every run, as before. With authentication on (below), each
API request is scoped to its caller; with it off, nothing sets an owner and
Conquest behaves as before. Only reads are scoped: writes address a
run or task that a scoped read has returned, and aggregates (the state
counts in `service-info`) and internal callers (reconcilers, the dispatcher,
the runner callback API) stay unscoped. A public TES task that names a run
in the tag `conquest_run_id` is refused with 404 unless the caller can read
that run.

### Authentication

Off by default: the APIs are open, as before, and the server logs a warning
at startup. With `CONQUEST_AUTH_ENABLED=true`, every request to the public
WES and TES routes needs an OAuth 2.0 access token in an
`Authorization: Bearer` header (package `internal/auth`). Probes, the runner
callback API (`/internal/runs/...`, which takes runner tokens instead, see
[Runner callbacks](#runner-callbacks)) and the storage gateway are not
affected. `service-info` stays open unless
`CONQUEST_AUTH_SERVICE_INFO_PUBLIC=false`; a token sent there is ignored.

A token is accepted when:

- it is a JWT signed with an allowed asymmetric algorithm (default RS256,
  ES256; never `none` or HMAC) by a key of the issuer it names; `iss` must
  equal a configured issuer exactly, and keys come only from that issuer's
  key set (`jwks_uri`, or discovered from
  `<issuer>/.well-known/openid-configuration`);
- its `typ` header is `at+jwt` or `application/at+jwt` (RFC 9068, any
  case), which refuses ID tokens;
- `exp` is present and not past, `nbf` and `iat` are not in the future
  (with `clock_skew_seconds` leeway), and `sub` is set;
- its audience fits the caller: a **service client** (its `client_id` is
  listed under the issuer's `service_clients` and `sub == client_id`) must
  present `aud` equal to its own client ID only; any other token is a user
  token, and its `aud` must contain one of the issuer's `audiences` (the
  client IDs of the front ends whose tokens Conquest accepts). A token whose
  `sub` is a listed client ID without being that client's own token is
  refused.

The caller owns the runs it creates, as `<iss>|<sub>` (see Run owners), and
sees only those. Failures answer 401 with `WWW-Authenticate: Bearer` (no
token) or `Bearer error="invalid_token"`; the cause is logged, never the
token. Tokens must name their key (`kid`); keys marked for encryption
(`use: enc`) are not used. If an issuer's discovery document or key set
can't be fetched, requests answer 503 (also while the first fetch is in
progress) and the fetch is retried 10 seconds after the failed attempt.
An unknown key ID refetches the key set at most once a minute; within that
minute, further unknown key IDs are refused at once (401), so a burst of
tokens with made-up key IDs can delay the pickup of a rotated key by up to a
minute.

The issuers live in a YAML file (`CONQUEST_AUTH_CONFIG`, default
`/etc/conquest/auth/config.yaml`); unknown keys are refused, and the server
does not start with an invalid file:

```yaml
issuers:
  - url: https://login.aai.lifescience-ri.eu/oidc/  # exactly as in iss
    # jwks_uri: https://...                        # default: discovery
    # userinfo_uri: https://...                    # default: discovery
    audiences: [<front-end client ID>]             # required
    service_clients:                               # optional
      - client_id: <client ID>
create:                     # who may create runs and tasks (below)
  entitlements: [<entitlement>]               # exact values
  entitlement_claim: eduperson_entitlement    # default
algorithms: [RS256, ES256]  # default
clock_skew_seconds: 60      # default
timeout_seconds: 10         # default and maximum; discovery, key set and userinfo requests
```

Issuer, key set and userinfo URLs must use https (plain http only to a
loopback host), also after redirects, and issuer URLs must not contain `|`
(it separates issuer and subject in run owners). Tokens must carry `exp`,
`iat` and `sub`.

#### Who may create runs and tasks

Creating a run (`POST /ga4gh/wes/v1/runs`) or a task through the public
TES API (`POST /ga4gh/tes/v1/tasks`) needs, besides a valid token, one of:

- the caller is a listed **service client** using its own token (as
  above): it may create without an entitlement;
- the caller is a **user** whose entitlements contain one of
  `create.entitlements`. Conquest asks the issuer's userinfo endpoint
  (`userinfo_uri`, or `userinfo_endpoint` from discovery) with the
  request's own token, and reads the claim `create.entitlement_claim`
  (a list of strings or one string). Values are compared as exact strings,
  never as prefixes; the userinfo `sub` must equal the token's `sub`.

Any other caller with a valid token is refused with 403. That includes a
client that isn't listed (`sub == client_id`; the issuer isn't asked for
it) whose token passes validation because its `aud` contains one of the
issuer's `audiences`; otherwise its token fails validation already (401).
Without
`create.entitlements`, only listed service clients create. The server
doesn't start if neither entitlements nor service clients are configured,
since nobody could create runs.

Only creates check entitlements: listing, reading and cancelling own runs
and tasks need a valid token only, so a caller whose entitlement is revoked
keeps access to the runs they created, and an outage of the userinfo
endpoint doesn't block them. Nothing is cached: every create asks the
userinfo endpoint again, so a revoked entitlement takes effect on the next
create. Answers:

- 403 (body `forbidden`, the reason only in the log): no configured
  entitlement, or a token of a client that isn't listed;
- 401 with `WWW-Authenticate: Bearer error="invalid_token"`: the userinfo
  endpoint refuses the token (401 or 403), or its `sub` is missing or
  differs from the token's;
- 503: the userinfo endpoint can't be reached, times out
  (`timeout_seconds`), answers anything else than 200 with a single JSON
  object of at most 1 MiB,
  or isn't configured and can't be discovered (retried 10 seconds after a
  failed discovery). The userinfo request follows no redirects (a 3xx
  answers 503), so the user's token goes only to the userinfo endpoint.

Userinfo discovery is separate from the key set and happens on the first
create, so its failure never affects reads.

The entitlements must be ones the issuer releases at userinfo for the
front ends' clients (an IdP may release them only there, not in access
tokens).

With the Helm chart, set `auth.enabled` and `auth.config` (see
[Securing a deployment with the chart](#securing-a-deployment-with-the-chart)).

Upgrading: runs created before authentication have no owner and are no
longer visible through the API (operators can still inspect and delete them
in PostgreSQL). Drain active runs before switching authentication on, so
that no run straddles the change. Authentication needs
`CONQUEST_RUNNER_SECRET` (see [Runner callbacks](#runner-callbacks)): the
server refuses to start with `CONQUEST_AUTH_ENABLED=true` without it.

Upgrading from a version without the create rule: an auth config needs
`create.entitlements` or at least one service client, or the server
doesn't start; until then, any authenticated caller could create. Replicas
still running the old version accept creates by any authenticated caller
until they are replaced, so with more than one replica, roll out with
the Deployment strategy `Recreate` (or scale to one first).

### Runner callbacks

Workflow runners submit, poll and cancel their tasks on the runner callback
API, `/internal/runs/<run ID>/ga4gh/tes/v1/...` (package
`internal/runnerauth`). With `CONQUEST_RUNNER_SECRET` set (at least 32
bytes; the chart always sets it):

- Each run has its own runner token, `HMAC-SHA256(secret, "runner:" + run
  ID)` in hex. Conquest derives it when it starts the runner Job and gives
  it to the runner as `CONQUEST_RUNNER_TOKEN`; it is stored nowhere.
- Every callback, `service-info` included, needs `Authorization: Bearer
  <runner token>` of the run in its path (compared in constant time), else
  `401`. User and service tokens are not accepted there. Once the run is
  terminal its token is refused (`401`); a missing run is `404`.
- Getting or cancelling a task of another run through a run's path is
  `404`, as is any task ID that doesn't exist.
- The engines send the token on every call: Nextflow through nf-ga4gh's
  `tes.oauthToken` in the generated config, Snakemake and miniwdl through
  Conquest's plugins (py-tes `token`).

So a runner, and workflow code running next to it (the token is readable
in the runner pod and, in `pvc` and `objectfs` mode, in the run's
`nextflow.config` on the workspace), can act on its own run only. The
token is in the runner Job's spec: who can read Jobs in the namespace can
act on active runs; runner pods don't mount a service account token.

Without `CONQUEST_RUNNER_SECRET` the callback API stays open, as before, and
the server logs a warning: keep it off any public route and restrict
in-cluster access to the server's port with a NetworkPolicy where you can.
Either way, expose only `/ga4gh/wes/` (and the storage gateway if needed)
publicly.

Upgrading: runs started before the secret was set (or before it was
rotated) have no valid token, so their callbacks fail and the runs fail.
Drain active runs before setting or rotating the secret; with more than one
replica, roll out with the Deployment strategy `Recreate`, since all
replicas must use the same secret.

## Federation: Remote TES Endpoints

Conquest can send individual tasks to other GA4GH TES servers (Funnel, TESK,
Poiesis, proTES, another Conquest) instead of its own Kubernetes cluster.
Engines are unchanged: they still submit to Conquest's internal TES API, and
Conquest decides per task where the task runs. Without a federation config,
every task runs locally, as before.

```
Engine ──TES──> Conquest internal TES API
                       │
                RoutingBackend ──> local Kubernetes (default)
                       ├─────────> remote TES "funnel-a"
                       └─────────> remote TES "tesk-b"  ...
```

Enable it with chart values (`federation.enabled`, `federation.config`) or
`CONQUEST_FEDERATION_CONFIG=<file>`:

```yaml
gateway_id: conquest-prod            # Via header; requests carrying it are loops (508)
endpoints:
  - name: funnel-a
    url: https://funnel.example.org/v1   # tasks are created with POST <url>/tasks
    auth: {type: client_credentials, token_url: https://idp/token,
           client_id: conquest, client_secret_env: FUNNEL_A_SECRET}
    tes_version: "1.1"                   # "1.0" endpoints only get tasks 1.0 can express
    s3_url_style: endpoint               # Funnel: s3://<s3_host>/<bucket>/<key>
    s3_host: s3.example.org:443          # S3 as the endpoint reaches it
    optional_file_outputs: true          # missing FILE outputs are skipped, as locally
    data_classes: [public]               # run tag conquest.data_class must be listed
    client_selectable: true              # clients may pick it with a hint
    max_cpu_cores: 16
routing:
  client_hints: true
  rules:                                 # first match wins
    - name: qc-remote
      match: {engines: [nextflow], task_names: ["*:FASTQC (*"], run_tags: {site: eu}}
      backends: [funnel-a, local]        # in order; local is appended unless no_local_fallback
```

**How a task is routed** (the decision is stored with the task and shown in
the TES FULL view: `logs[0].metadata["conquest.backend"]` and a `route:`
system log line):

1. **Local only**: tasks of shared-workspace runs (`pvc`, `objectfs`) and
   tasks with inputs or outputs outside Conquest's bucket. WDL runs in
   `emptydir` mode stage through S3 (see Workspace Modes) and can go remote.
2. **Rule**: the first matching rule gives the ordered backends.
3. **Hint**: tag `conquest.tes_backend` (`"funnel-a,local"`) on the task or
   the WES run narrows and reorders the rule's backends, or, without a rule,
   picks client-selectable endpoints. Snakemake rules set it with
   `resources: tes_backend="funnel-a"`.
4. **Eligibility**: an endpoint is dropped if the run's data class
   (`conquest.data_class`, default `unclassified`) isn't in its
   `data_classes`, the task exceeds its caps, or it's a TES 1.0 endpoint and
   the task uses 1.1 features (wildcard outputs, streamable inputs,
   ignore_error, backend_parameters).
5. **Submission**: backends are tried in order. The next one is tried only if
   an endpoint certainly refused (connection refused, 4xx, 503). If it may
   have created the task (timeout, other 5xx), the task fails instead of
   risking a second run.

**What forwarded tasks look like.** Conquest's own tags are removed and
`conquest_task_id` is added; S3 URLs are written in the endpoint's style;
Nextflow brace globs (`*{1,2}.fq.gz`) are expanded into plain wildcards, which
TES servers' glob implementations lack. For Nextflow tasks, TES 1.1 endpoints
are also asked for every `.command.*` file (e.g. `.command.env`, which
nf-ga4gh doesn't declare), as Conquest's own filer uploads them.

**Storage gateway: no S3 credentials at remote sites.** With
`s3_url_style: gateway` (chart: `storageGateway.*`), a forwarded task's S3
URLs become capability URLs on Conquest itself:

```
<storageGateway.publicURL>/storage/v1/t/<task ID>/<HMAC>/<bucket>/<key>
```

A request to one is answered with `307` to an S3 URL presigned at that
moment for that object and method, so URLs can't expire while a task waits
in a remote queue. What a task may touch comes from its stored request:
GET/HEAD for its inputs, PUT for its outputs (including any file below
wildcard and directory outputs). URLs stop working shortly after the task
ends. Directory inputs are expanded into one input per object, since HTTP
can't list. Remote sites then need only to reach the gateway and the
presigned S3 endpoint (`storageGateway.s3PublicEndpoint`), and must follow
`307` on PUT: Funnel (since #1472), tesk-core (since #61) and Poiesis (since
#103) do. The gateway works over plain HTTP, but the URLs are bearer
capabilities: use HTTPS outside trusted networks. Conquest's own task pods
can use the gateway too (see [Storage Isolation](#storage-isolation)).

**Requirements on remote sites** (none of these can be expressed in TES):

- Access to Conquest's objects: through the storage gateway, or else read and
  write access to Conquest's bucket configured on the remote side.
- Executor images pullable from there (Conquest's Snakemake task image too).
- Files staged so the executor's image user can write next to them:
  Conquest's Snakemake task image runs as UID 1000.

Verified on 2026-11-06 with Funnel develop `a3be3b20` and TESK 1.1.0 /
tesk-core v0.10.6: `nextflow-io/hello` and `snakemake-hello-world` on each
endpoint, and nf-core/methylseq 4.2.0 split across local, Funnel and TESK.
TESK implements TES 1.0: Conquest doesn't send it tasks with wildcard
outputs (most nf-core processes), and Nextflow tasks with optional or `env`
outputs fail there, so route only simple Nextflow processes and Snakemake
jobs to TES 1.0 endpoints.

## Adapter Architecture

Engine support is pluggable. Each engine implements a `RunAdapter` interface
and registers itself in a `Registry` at startup.

```go
type RunAdapter interface {
    Name() string
    PrepareRun(runID string, req SubmissionRequest, layout storage.Layout) (*PreparedRun, error)
}
```

When a workflow is submitted, the controller looks up the adapter by engine
name. The adapter handles everything engine-specific: resolving the workflow
source, selecting the right runner image, building CLI arguments, and
generating config files. The rest of the system — controller, K8s backends,
reconcilers, storage layout — works the same regardless of engine.

Adding a new engine means implementing this interface and registering it.

## State Machine

Runs and tasks follow the GA4GH v1.1 state model:

```
QUEUED --> INITIALIZING --> RUNNING --> COMPLETE
                                   --> EXECUTOR_ERROR
                                   --> SYSTEM_ERROR
                                   --> CANCELING --> CANCELED   (runs)
                                   --> CANCELED                (tasks)
```

Runs pass through `CANCELING` before `CANCELED` to cascade cancellation to
their tasks. Tasks go directly to `CANCELED` — the K8s Job is deleted
immediately. Transitions are enforced in code; invalid transitions return an
error.

### Task dispatch

Creating a TES task stores it as `QUEUED` and answers at once. A background
dispatcher then claims the task, routes it, submits it to its backend and
moves it to `INITIALIZING`; the reconciler follows it from there. A slow
remote endpoint, or a Kubernetes API that times out, therefore delays tasks
instead of failing the requests that create them, which workflow engines treat
as fatal.

- **Retries:** a submission that certainly created nothing (the Kubernetes API
  failed, or every candidate endpoint refused) is retried. The delay starts at
  2s and doubles up to 1 min, for `CONQUEST_DISPATCH_MAX_ATTEMPTS` attempts
  (about 15 minutes); then the task is `SYSTEM_ERROR`. A submission that may
  have created the task (an endpoint's answer was lost) is not retried, so
  that the task never runs twice.
- **Idempotent local submission:** Kubernetes Jobs are named after the task,
  so a retried submission adopts a Job that an earlier attempt created.
- **Several servers:** dispatchers claim tasks with `FOR UPDATE SKIP LOCKED`
  and a lease, so several server replicas share the queue. A task whose
  dispatcher stops mid-submission is claimed again when its lease expires.
- **Cancellation:** cancelling a task that hasn't been dispatched needs no
  backend. If it is being submitted at that moment, the dispatcher cancels the
  submission when it finds the task `CANCELED`.
- **Workers:** `CONQUEST_DISPATCH_WORKERS` submissions run at once, so one slow
  endpoint doesn't hold up tasks bound elsewhere.

`CONQUEST_ASYNC_DISPATCH=false` restores submitting before answering.

## Testing

Tests use Go's standard `testing` package with no third-party frameworks. All
test doubles — stubs, fakes, recorders — are hand-written and live alongside
the code they test.

~180 test functions across 14 packages covering state machine transitions, K8s
job spec generation, workspace strategies, task policy enforcement, storage
layout paths, adapter logic, reconciler behavior, and more.

```bash
make test          # go test -v -race -count=1 ./...
make test-short    # go test -short -race ./...
```

The PostgreSQL repository tests carry the build tag `integration` and need a
migrated database; without `CONQUEST_TEST_POSTGRES_DSN` they skip. CI runs
them in the `Integration tests` job, which also rolls the newest migration
back and forth. Locally, with
[migrate](https://github.com/golang-migrate/migrate) installed:

```bash
export CONQUEST_TEST_POSTGRES_DSN=postgres://...
migrate -path migrations -database "$CONQUEST_TEST_POSTGRES_DSN" up
go test -tags integration ./internal/repository/pg/
```

The storage isolation tests (`internal/isolation`, same build tag) need an S3
server that checks signatures and skip without `CONQUEST_TEST_S3_ENDPOINT`;
CI runs them against SeaweedFS in the `Storage integration test` job (see the
comment at the top of `internal/isolation/s3_integration_test.go`).

## Configuration

All settings come from environment variables with the `CONQUEST_` prefix.
No config files.

| Variable                            | Default              | Description                       |
|-------------------------------------|----------------------|-----------------------------------|
| `CONQUEST_HTTP_ADDR`                | `:8080`              | Server listen address             |
| `CONQUEST_POSTGRES_DSN`             | —                    | Postgres connection string        |
| `CONQUEST_S3_BUCKET`                | `conquest-workflows` | Object storage bucket             |
| `CONQUEST_S3_ENDPOINT`              | —                    | S3 / MinIO endpoint               |
| `CONQUEST_K8S_NAMESPACE`            | `conquest`           | Kubernetes namespace for Jobs     |
| `CONQUEST_AUTH_ENABLED`             | `false`              | Require bearer tokens             |
| `CONQUEST_AUTH_SERVICE_INFO_PUBLIC` | `true`               | Open `service-info` with auth on  |
| `CONQUEST_AUTH_CONFIG`              | see Authentication   | Issuers, audiences, create rule (YAML) |
| `CONQUEST_WORKSPACE_MODE`           | `emptydir`           | `emptydir`, `pvc`, or `objectfs`  |
| `CONQUEST_WORKSPACE_ACCESS_MODE`    | `RWX`                | `RWX` or `RWO` (when mode=pvc)     |
| `CONQUEST_WORKSPACE_SIZE`           | `10Gi`               | PVC / objectfs volume size        |
| `CONQUEST_OBJECTFS_CSI_SECRET_NAME` | `csi-s3-secret`      | S3 creds for the FUSE mount       |
| `CONQUEST_POD_SECURITY`             | `restricted`         | `restricted` or `baseline`        |
| `CONQUEST_POD_RUN_AS_USER`          | `1000`               | UID for runner/task containers    |
| `CONQUEST_POD_RUN_AS_GROUP`         | `1000`               | GID and fsGroup for those pods    |
| `CONQUEST_TASK_HELPER_RESOURCES`    | —                    | Task init container resources     |
| `CONQUEST_STORAGE_ISOLATION`        | `false`              | No S3 credentials in task pods (see Storage Isolation) |
| `CONQUEST_STORAGE_GATEWAY_SECRET`   | —                    | Signs storage gateway URLs (>= 32 bytes) |
| `CONQUEST_RUNNER_SECRET`            | —                    | Derives runner tokens (>= 32 bytes; see Runner callbacks) |
| `CONQUEST_RECONCILER_INTERVAL`      | `5s`                 | How often reconcilers poll K8s    |
| `CONQUEST_ASYNC_DISPATCH`           | `true`               | Submit TES tasks in the background |
| `CONQUEST_DISPATCH_WORKERS`         | `8`                  | Task submissions in parallel      |
| `CONQUEST_DISPATCH_MAX_ATTEMPTS`    | `20`                 | Submissions per task before SYSTEM_ERROR |
| `CONQUEST_RUNNER_IMAGE`             | —                    | Default Nextflow runner image     |
| `CONQUEST_WDL_RUNNER_IMAGE`         | —                    | Default WDL runner image          |
| `CONQUEST_SNAKEMAKE_RUNNER_IMAGE`   | —                    | Default Snakemake runner image    |

See [`internal/config/config.go`](internal/config/config.go) for the full list.

## Development

```bash
make build         # go build ./...
make test          # Run all tests
make fmt           # gofmt
make lint          # go vet
make tidy          # go mod tidy
```

## Deployment

Docker images and a Helm chart:

```bash
make docker-server          # Server image
make docker-nf-runner-all   # All Nextflow runner images (4 versions)
make docker-wdl-runner      # WDL runner image
make docker-smk-runner-all  # All Snakemake runner images (2 versions)
make docker-all             # Everything
```

The Helm chart at `deploy/charts/conquest/` includes RBAC, init-container DB
migrations, health probes, and a non-root security context.

### Securing a deployment with the chart

Token authentication and storage isolation are off by default; turn them on for any deployment
that others can reach.

- **Token authentication** (see Authentication): `auth.enabled: true` sets
  `CONQUEST_AUTH_ENABLED`, `auth.serviceInfoPublic` (default `true`) sets
  `CONQUEST_AUTH_SERVICE_INFO_PUBLIC`, and `auth.config` is rendered
  verbatim (snake_case keys, as in the Authentication section) into the
  ConfigMap `<conquest.appname>-auth` (default `conquest-auth`), mounted at `/etc/conquest/auth/config.yaml`
  (`CONQUEST_AUTH_CONFIG`). A checksum annotation restarts the server when
  the config changes. `auth.config.issuers` is required when enabled
  (`helm template` fails without it), and the server refuses to start
  without `create.entitlements` or at least one service client.
- **Runner callbacks** (see Runner callbacks): always authenticated. The
  chart generates the Secret `<fullname>-runner` (default `conquest-runner`, key
  `runner-secret`) once and keeps it across upgrades, or uses
  `runner.existingSecret` (same key, at least 32 bytes).
- **Storage isolation** (see Storage Isolation): `storageIsolation.enabled:
  true` keeps S3 credentials out of task pods.
  Runner pods still get the bucket-wide S3 credentials (see Storage
  Isolation, "Not covered"): workflow code in a runner can read and write
  every run's objects in the bucket. Until that is fixed, let only trusted
  callers create runs (`create.entitlements`), process only data that
  isn't sensitive, and tell users about this limit.
- **Routes**: the chart creates no Ingress or HTTPRoute. Route publicly
  only what callers outside the cluster need: `/ga4gh/wes/v1/` for
  workflow submitters and, with remote TES endpoints
  (`storageGateway.enabled`), the storage gateway's `/storage/v1/t/`.
  Keep `/ga4gh/tes/`, `/internal/` and the probes off public routes.

```yaml
auth:
  enabled: true
  config:
    issuers:
      - url: https://login.example.org/oidc/    # exactly as in iss
        audiences: [<front-end client ID>]
        service_clients:                        # optional
          - client_id: <client ID>
    create:
      entitlements: [<exact entitlement>]
storageIsolation:
  enabled: true
```

Upgrading: drain active runs before switching authentication on (see
Authentication) and before the first upgrade to a chart with runner
tokens (see Runner callbacks: runs started before fail), and set `conquest.strategy: {type: Recreate}` for that
upgrade: with the default rolling update, old pods keep serving without
authentication (or without the new create rule) until they are replaced.

CI publishes the images of every push to `master`: to Docker Hub
(`docker.io/anurag1201/*`, `.github/workflows/images.yaml`, this repository
only) and to the GitHub Container Registry of the repository's owner
(`ghcr.io/<owner>/*`, `.github/workflows/ghcr.yaml`, also in forks), tagged
`sha-<short commit>`. The GHCR images (linux/amd64) are scanned with Trivy,
signed with cosign (keyless) and carry build provenance attestations; pin
them by digest (the chart's `tag` values take `sha-<short>@sha256:<digest>`).
GHCR has `sha-*` tags only (no `latest` or version tags yet).
The workflow's header shows how to verify them.

## Design Decisions

- **WES** as the external API, **TES** as the internal execution contract
- **GA4GH v1.1** for both specs (unified state enum)
- **Postgres** as system of record
- **S3/MinIO** as the data plane for inputs, outputs, logs, and metadata
- **UUIDv7** for all IDs — time-sortable, DNS-safe, works as Postgres PKs
  and K8s labels
- Versioned storage layout (`v1/`) so the format can evolve without migration
- **Transparent** object layout in every mode — one object per file, readable
  keys, no chunking. Rules out JuiceFS-style chunk+metadata designs.
- Permissively licensed Go dependencies (Apache 2.0 / MIT / BSD). Two
  non-Go exceptions: the Nextflow runtime bases derive from `amazoncorretto`
  (GPLv2 with Classpath Exception) and the objectfs CSI driver image comes
  from `cr.yandex`

## Tech Debt

Tracked as GitHub issues under the
[`tech-debt`](https://github.com/anuragxxd/conquest/issues?q=is%3Aissue+label%3Atech-debt)
label, triaged `P0` / `P1` / `P2`. Each carries the verified code evidence
for its claim.

Design records that outlived their documents live there too — see
[#33](https://github.com/anuragxxd/conquest/issues/33) for objectfs.

## License

Conquest is licensed under the [Apache License 2.0](LICENSE).
