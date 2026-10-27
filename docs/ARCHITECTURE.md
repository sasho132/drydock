# Architecture

## Goals

1. **Never leave a service down because of an update.** The previous container
   must survive until the new one is proven healthy.
2. **Never lose a container's identity.** Name, labels, environment, ports,
   networks, volumes, restart policy, and resource limits are reproduced
   exactly; only the image changes.
3. **Be explainable after the fact.** Every change is recorded with before and
   after image IDs and a snapshot that can be replayed.

## Packages

| Package | Responsibility |
|---|---|
| `internal/model` | Container introspection, config sanitisation, snapshots |
| `internal/config` | Layered configuration and validation |
| `internal/policy` | Labels → policy, selection, dependency planning |
| `internal/registry` | Manifest resolution, auth, retries, TLS |
| `internal/docker` | Engine API operations and the recreate/rollback protocol |
| `internal/engine` | Cycle orchestration, history, notifications |
| `internal/scheduler` | Per-container next-run computation |
| `internal/state` | Atomic persistence of history, images, approvals |
| `internal/api` | HTTP surface |
| `internal/notify`, `internal/metrics` | Outputs |
| `internal/ui` | Embedded dashboard, session auth, CSRF, job polling |

The engine depends on narrow interfaces (`engine.Docker`, `engine.Resolver`),
which is why the update logic is tested without a daemon.

## The recreate protocol

Creating a replacement container is not atomic, so the protocol is designed so
that every intermediate state has a safe recovery step.

```
        ┌─────────────────────────────────────────────────────────┐
        │ 1. pull new image        (old image and container intact)│
        │ 2. snapshot old container                                │
        │ 3. stop old container    (name still reserved)           │
        │ 4. rename old → backup   (name now free)                 │
        │ 5. create new with old's name, config, networks          │
        │ 6. start new                                             │
        │ 7. verify health                                         │
        └───────────────┬─────────────────────────┬───────────────┘
                        │ success                 │ failure
                        ▼                         ▼
           ┌──────────────────────┐   ┌──────────────────────────────┐
           │ remove backup        │   │ remove half-created new      │
           │ keep old image       │   │ rename backup → original     │
           │ record history       │   │ start original               │
           └──────────────────────┘   │ record rolled_back           │
                                      └──────────────────────────────┘
```

Why this order:

- **Stop before rename** — Docker refuses to rename a running container only in
  some versions; stopping first is unambiguous and also gives the app its
  graceful shutdown window.
- **Rename rather than remove** — the old container remains a complete,
  startable entity. Restoring is a rename plus a start, not a reconstruction.
- **Create with the old name** — anything that resolves containers by name
  (reverse proxies, Compose, monitoring) keeps working without DNS changes.
- **Verify the created image ID** — the create API takes a tag, not an image
  ID, so a push landing between the pull and the create could run an
  unverified image. The new container is inspected immediately and must
  reference the exact image ID that was pulled; otherwise it is rolled back.
- **Remove the new container before restoring** — a container that failed to
  start still owns its name; forgetting this is what makes naive rollbacks
  fail with "name already in use".
- **`--rm` containers** — the daemon deletes them on stop, so there is no
  backup to rename. The snapshot is used to rebuild the old container from its
  image ID instead.

## Config sanitisation

`container.Config` returned by `docker inspect` contains the *merged* result of
the image defaults and the user's `docker run` flags. Re-submitting it verbatim
would freeze the old image's `CMD`, `ENTRYPOINT`, `ENV`, `LABELS`, `VOLUMES`,
`EXPOSE`, `WORKDIR`, `USER`, and `HEALTHCHECK` into the new container, so image
updates could never change them.

Drydock subtracts the old image's defaults from the inspected config, keeping
only values that differ (i.e. user intent). The new image then supplies its own
defaults. The original inspect structs are deep-copied first—mutating them in
place is a classic source of "the config changed after we inspected it" bugs.

Host ports are the exception: every binding must be re-exposed on create, so
port bindings are added back to `ExposedPorts`.

Network endpoints are sanitised: runtime fields (IP, gateway, endpoint ID, MAC)
are cleared and stale container-ID aliases removed. Only one endpoint can be
attached at create time, so the rest are attached with `NetworkConnect`
immediately afterwards.

## Health verification

Order of precedence:

1. **Docker healthcheck** — wait for `healthy`; fail immediately on
   `unhealthy` (Docker has already applied its own retry policy).
2. **Explicit probe** — `drydock.health-port` (TCP) and optional
   `drydock.health-url` (HTTP path, 2xx/3xx).
3. **Settle window** — the container must remain running for `settle` without
   its restart count increasing.

A restart-count increase is always a failure, which catches crash loops even
when the container briefly appears `running` between restarts.

## Digest comparison

Two digests matter for a tag:

- the **index digest** (multi-arch manifest list), which is what
  `RepoDigests` usually stores for a pulled image;
- the **platform manifest digest**, which is what the container's architecture
  actually runs.

Drydock compares local `RepoDigests` against both. Watchtower compares only the
top-level digest, which produces false positives (and unnecessary pulls) when a
registry's index and platform manifests disagree.

Pinned references (`repo@sha256:...`) can never move, so they are skipped
without any network call.

## Concurrency

- Dependencies form a DAG. `policy.Plan` returns levels via Kahn's algorithm;
  a cycle is a hard error naming the containers involved.
- Levels run sequentially; within a level, up to `concurrency` containers are
  updated in parallel.
- A single engine mutex serialises the whole cycle, so the scheduler and API
  cannot interleave updates.

## State

`state.json` holds:

- `history`: append-only audit entries (capped at 2000), each optionally
  carrying the snapshot of the container *before* the change;
- `images`: per-container image-ID history used for pruning while keeping
  rollback versions;
- `pending`: detected updates awaiting approval.

Writes go to a temporary file in the same directory, are `fsync`ed, and are
then atomically renamed over the old file. A crash therefore leaves either the
old complete file or the new complete file, never a truncated one.

## Rollback target selection

`LastSuccessful` returns the newest `updated` entry with a snapshot. That
snapshot's `ImageID` is used to recreate the container, not its tag: tags move,
image IDs do not. The snapshot's tag is still preserved in the reserved label
`io.drydock.image-ref` so subsequent updates follow the original reference.

Manual rollback records the state it rolled back *from*, so an operator can
inspect (or reverse) the operation later.


## Dashboard and background jobs

The dashboard is server-rendered with `html/template`. Templates and static
assets are embedded with `go:embed`, which keeps the deployment an ordinary
static binary: no Node toolchain, no CDN, and no writable filesystem.

A full update can take tens of seconds (pulls, graceful stop, health
verification), which is too long for a browser form submission. Mutating
dashboard actions therefore call `Engine.StartJob`, which:

1. rejects the request with `ErrJobBusy` if another job is active;
2. records a job with `queued` status and returns immediately;
3. runs the existing `Run`/`Rollback` path in a goroutine, which still takes
   the engine's `runMu`, so scheduled runs and API calls cannot interleave;
4. records the report or outcome and flips the status to `succeeded`/`failed`.

The browser polls `GET /jobs/{id}` (a session-protected JSON endpoint) once per
second, renders the counts, and reloads the page with the `job` query parameter
removed. The JSON API exposes the same primitives at `/v1/jobs` for
automation.

### Dashboard security model

- No password configured → read-only. Action endpoints redirect with an
  explanatory error and never call the engine.
- Password configured → login issues an HMAC-SHA256 signed session cookie.
  The signing key is derived from the password, so rotating the password
  invalidates every session.
- Cookies are `HttpOnly`, `SameSite=Strict`, and `Secure` behind TLS.
- Every mutation carries a per-session CSRF token; the comparison is
  constant-time.
- Login attempts are limited per remote IP.
- `next` redirects are restricted to same-origin paths to prevent open
  redirects.
- A strict CSP blocks inline and third-party scripts and styles; all assets are
  served locally.
- Settings are rendered from an explicit view model. Tokens, passwords, and
  notification URLs (which are effectively secrets) are never sent to the
  browser.
