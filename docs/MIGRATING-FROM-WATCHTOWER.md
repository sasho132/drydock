# Migrating from Watchtower

Watchtower is no longer maintained. Drydock covers the same workflow with a
different configuration surface. This guide maps the pieces.

## Labels

| Watchtower | Drydock |
|---|---|
| `com.centurylinklabs.watchtower.enable` | `drydock.enable` |
| `com.centurylinklabs.watchtower.monitor-only` | `drydock.monitor-only` |
| `com.centurylinklabs.watchtower.scope` | `drydock.scope` |
| `com.centurylinklabs.watchtower.stop-signal` | use the container's own `STOPSIGNAL` |
| `com.centurylinklabs.watchtower.lifecycle.pre-check` | not supported (use your own hooks) |
| `com.centurylinklabs.watchtower.lifecycle.post-check` | not supported |
| `com.centurylinklabs.watchtower.lifecycle.pre-update` | not supported |
| `com.centurylinklabs.watchtower.lifecycle.post-update` | not supported |

New in Drydock: `drydock.schedule`, `drydock.depends-on`,
`drydock.health-port`, `drydock.health-url`, `drydock.approve`,
`drydock.priority`, `drydock.rollback`, `drydock.cleanup`,
`drydock.health-timeout`, `drydock.settle`, `drydock.stop-timeout`.

The prefix is configurable with `label_prefix` / `DRYDOCK_LABEL_PREFIX`.

## Flags

| Watchtower | Drydock |
|---|---|
| `--interval 6h` | `--interval 6h` |
| `--schedule "0 4 * * *"` | `--schedule "0 4 * * *"` |
| `--cleanup` | `--cleanup` |
| `--stop-timeout` | `--stop-timeout` |
| `--label-enable` | `--label-enable` |
| `--scope` | `--scope` |
| `--monitor-only` | `--monitor-only` |
| `--include-stopped` | `--include-stopped` |
| `--rolling-restart` | not needed — updates are dependency-ordered |
| `--no-restart` | use `--monitor-only` |
| `--no-pull` | label `drydock.no-pull` |
| `--debug` / `--trace` | `--log-level debug` |
| `--notifications` (shoutrrr URL) | `notifications:` in the config file |
| `--http-api-*` | `--api`, `--api-addr`, `--api-token` |
| `--tlsverify` | automatic (`DOCKER_HOST`/`DOCKER_CERT_PATH`) |
| `--host` | `--docker-host` or `DOCKER_HOST` |
| `--revive-stopped` | not supported |
| `--remove-volumes` | `remove_volumes: true` in the config file |
| `--label-filter` | not supported |
| `--filter` | `--only` (repeatable) |

## Notifications

Watchtower accepts Shoutrrr URLs. Drydock has three sinks:

- `slack` — Slack-compatible incoming webhook;
- `webhook` — the full report as JSON, with custom headers;
- `json` — identical to webhook, kept as an explicit name.

Map `slack://...` to the Slack incoming-webhook URL. For email, Gotify, Teams,
and the rest, point a `webhook` sink at a small relay (or run
[Shoutrrr's CLI](https://containrrr.dev/shoutrrr/) as a sidecar).

Reports look like:

```json
{
  "started_at": "2026-01-01T04:00:00Z",
  "duration": "12.4s",
  "dry_run": false,
  "updated": 1,
  "failed": 0,
  "rolled_back": 0,
  "up_to_date": 4,
  "pending": 0,
  "results": [
    {"container":"web","image":"ghcr.io/acme/web:latest","status":"updated",
     "old_image_id":"sha256:...","new_image_id":"sha256:..."}
  ]
}
```

## Behaviour differences to know about

1. **Drydock keeps the old container until the new one is healthy.** Update
   downtime is slightly longer than Watchtower's stop-remove-create sequence,
   because the old container is renamed rather than deleted.
2. **Health checks are enforced.** If a container defines `HEALTHCHECK` and the
   new image is unhealthy, the update is rolled back. Watchtower would have
   accepted it.
3. **A failed update is retried on the next cycle.** The broken tag stays on the
   registry, so expect a rollback each cycle until a fixed image is pushed.
   Use `drydock.monitor-only` if you would rather be notified only.
4. **Details that merely mirror the old image are dropped**, so new image
   defaults take effect. If you intentionally pinned a `CMD` that equals the
   old image's `CMD`, you will now inherit the new image's `CMD`; repeat the
   pin explicitly in your compose file.
5. **Drydock does not update itself.** Update it deliberately.

## Moving the state

There is nothing to migrate: Watchtower keeps its history in memory. Point
Drydock at a writable volume:

```yaml
environment:
  DRYDOCK_STATE_FILE: /var/lib/drydock/state.json
volumes:
  - drydock-data:/var/lib/drydock
```

## Suggested rollout

1. Run `drydock check --json` on its own and compare the reported updates with
   what Watchtower would do.
2. Run with `--monitor-only` for a few days to see the planned updates in your
   notifications.
3. Switch to `rollback: true` (default) and let it update.
4. Label stateful services (`postgres`, `minio`, ...) with
   `drydock.monitor-only: "true"` and update them deliberately.
