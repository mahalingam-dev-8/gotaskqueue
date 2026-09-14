# GoTaskQueue

A distributed job processing service in Go, using **PostgreSQL as the broker** —
no Redis, no SQS, no RabbitMQ. Jobs are rows; `SELECT ... FOR UPDATE SKIP LOCKED`
is the dequeue primitive that lets many workers pull disjoint work concurrently.

Two binaries share one set of internal packages:

| Binary | Role | Scales on |
|---|---|---|
| `cmd/api` | Producer — REST API that accepts and tracks jobs | request rate |
| `cmd/worker` | Consumer — goroutine pool that executes jobs | queue depth |

Guarantees: **at-least-once** delivery (a job can run twice if a worker dies after
finishing but before committing the result — so handlers should be idempotent),
exponential-backoff retries up to `max_attempts`, graceful drain on SIGTERM, and
automatic recovery of jobs orphaned by a dead worker.

---

## Architecture

```
                    ┌──────────────────────────────────────────────────────┐
   client           │                    AWS / local                       │
     │              │                                                      │
     │  POST /jobs  │   ┌─────────┐        ┌──────────────────────────┐    │
     ├─────────────────▶│   ALB   │───────▶│  cmd/api  (ECS service)  │    │
     │  GET /jobs/:id   │ :443    │        │  Gin + JWT middleware    │    │
     │              │   └─────────┘        │  /healthz  /readyz       │    │
     │              │    health: /healthz  └────────────┬─────────────┘    │
     │              │                                   │ INSERT           │
     │              │                                   ▼                  │
     │              │                     ┌──────────────────────────┐     │
     │              │                     │      PostgreSQL (RDS)    │     │
     │              │                     │  ┌────────────────────┐  │     │
     │              │                     │  │ jobs               │  │     │
     │              │                     │  │  id, type, payload │  │     │
     │              │                     │  │  status, attempts  │  │     │
     │              │                     │  │  run_at, locked_at │  │     │
     │              │                     │  └────────────────────┘  │     │
     │              │                     └───────┬──────────▲───────┘     │
     │              │   SELECT ... FOR UPDATE     │          │ UPDATE      │
     │              │   SKIP LOCKED  ─────────────┘          │ status      │
     │              │                     ┌──────────────────┴───────┐     │
     │              │                     │ cmd/worker (ECS service) │     │
     │              │                     │  N replicas × pool size  │     │
     │              │                     └──────────────────────────┘     │
                    └──────────────────────────────────────────────────────┘

  inside one worker process:

        ┌────────────┐   claimed jobs    ┌──────────┐
        │ dispatcher │══════════════════▶│ chan Job │  (unbuffered)
        │ goroutine  │  (never more than └────┬─────┘
        └────────────┘   idle workers)        │
              ▲                        ┌──────┴──────┬─────────────┐
              │ poll every             ▼             ▼             ▼
              │ WORKER_POLL_       ┌────────┐   ┌────────┐   ┌────────┐
              │ INTERVAL           │worker 1│   │worker 2│ … │worker N│
        ┌─────┴──────┐             └───┬────┘   └───┬────┘   └───┬────┘
        │  Postgres  │◀────────────────┴────────────┴───────────-┘
        └─────┬──────┘   complete / retry(+backoff) / fail
              ▲
        ┌─────┴───────────┐  every RECOVERY_INTERVAL:
        │ reaper goroutine│  processing + locked_at < now() - STUCK_JOB_TIMEOUT
        └─────────────────┘  → back to pending (or failed if attempts exhausted)
```

### Job lifecycle

```
  enqueue ──▶ pending ──claim(SKIP LOCKED, attempts++)──▶ processing
                 ▲                                            │
                 │                                            ├─ success ──▶ completed
                 │                                            │
                 ├── retry: run_at = now() + backoff ◀─────────┤ error, attempts < max
                 │                                            │
                 │                                            ├─ error, attempts >= max ──▶ failed
                 │                                            └─ permanent error ─────────▶ failed
                 │
                 └── reaper: locked_at older than STUCK_JOB_TIMEOUT
```

---

## Repository layout

```
cmd/
  api/        producer entrypoint (HTTP)
  worker/     consumer entrypoint (pool)
  token/      dev-only: mints a local JWT
internal/
  api/        Gin router, handlers, middleware (request id, slog access log, recovery)
  auth/       JWT issue/verify + Gin auth & scope middleware
  config/     env-var loading, one struct per entrypoint
  db/         pgx pool + embedded SQL migration runner
  jobs/       the actual job handlers (email:send, report:generate, debug:*)
  logging/    slog setup
  queue/      the queue itself: enqueue, dequeue, complete/retry/fail, recovery
  worker/     goroutine pool, handler registry, backoff/permanent-error policy
migrations/   plain .sql files, embedded into the binary with //go:embed
```

`internal/` is enforced by the Go toolchain: nothing outside this module can import
it. Dependencies point one way — `worker` never imports `jobs`, so the pool has no
knowledge of business logic.

---

## Run it locally

Requires Docker. (A local Go toolchain is optional — `make docker-tidy` runs the
toolchain in a container.)

```bash
docker compose up --build          # postgres + api + worker
```

The API applies migrations on boot (`RUN_MIGRATIONS=true`); workers do not.

```bash
# 1. mint a dev token
docker compose build token
TOKEN=$(docker compose run --rm -T token -sub demo | tail -1)

# 2. probes (unauthenticated)
curl localhost:8080/healthz     # {"status":"ok"}
curl localhost:8080/readyz      # {"status":"ready","database":"ok"}

# 3. submit a job
curl -X POST localhost:8080/jobs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"email:send","payload":{"to":"dev@example.com","subject":"hello"}}'
# 202 {"id":"6455d7b4-...","status":"pending","attempts":0,"max_attempts":5,...}

# 4. check status
curl localhost:8080/jobs/6455d7b4-... -H "Authorization: Bearer $TOKEN"
# 200 {"status":"completed","attempts":1,...}
```

Host ports are overridable if 5432/8080 are taken: `POSTGRES_PORT=5544 API_PORT=9090 docker compose up`.

### Things worth trying

```bash
# watch SKIP LOCKED hand disjoint work to 3 worker containers
docker compose up -d --scale worker=3
for i in $(seq 1 12); do curl -s -o /dev/null -X POST localhost:8080/jobs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"debug:sleep","payload":{"seconds":2}}'; done
docker compose exec postgres psql -U gotaskqueue -d gotaskqueue \
  -c "SELECT locked_by, status, count(*) FROM jobs GROUP BY 1,2;"

# watch exponential backoff (this one always fails)
curl -X POST localhost:8080/jobs -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"type":"debug:flaky","payload":{"failure_rate":1.0},"max_attempts":3}'
docker compose logs -f worker   # retry_in=5.5s, then 9.8s, then failed

# watch graceful shutdown finish an in-flight job
curl -X POST localhost:8080/jobs -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"type":"debug:sleep","payload":{"seconds":10}}'
docker compose stop worker      # SIGTERM -> "draining in-flight jobs" -> "stopped cleanly"

# watch stuck-job recovery: SIGKILL a worker mid-job, the reaper requeues it
```

### Useful make targets

```
make up | down | logs | psql | token     docker-compose helpers
make build | test | vet | tidy           needs a local Go 1.25+
make docker-tidy                         go mod tidy without installing Go
```

---

## API

All `/jobs` routes require `Authorization: Bearer <jwt>`; they are also mounted
under `/api/v1/jobs`. Probes are unauthenticated so the ALB can reach them.

| Method | Path | Scope | Response |
|---|---|---|---|
| `POST` | `/jobs` | `jobs:write` | `202` + job |
| `GET` | `/jobs/:id` | `jobs:read` | `200` + job, `404` if unknown |
| `GET` | `/healthz` | — | `200` liveness, never touches the DB |
| `GET` | `/readyz` | — | `200` / `503` — pings Postgres |

`POST /jobs` body:

```jsonc
{
  "type": "email:send",         // required, must match a registered worker handler
  "payload": { "...": "..." },  // arbitrary JSON, stored as JSONB
  "max_attempts": 5,            // optional, defaults to DEFAULT_MAX_ATTEMPTS
  "delay_seconds": 0            // optional, schedule the first attempt in the future
}
```

`healthz` vs `readyz` matters operationally: liveness must not depend on Postgres,
or a database blip makes ECS kill every healthy task and turns degradation into an
outage. Point the ECS/ALB *health check* at `/healthz` and deployment gating at
`/readyz`.

### Auth

HS256 JWTs, verified with the algorithm pinned (`jwt.WithValidMethods`) plus issuer,
audience and a required `exp`. Scopes (`jobs:read`, `jobs:write`) gate the routes.
In production your IdP (Cognito/Auth0) issues tokens and this service only verifies —
`cmd/token` exists solely so the local stack is usable.

---

## How the queue works

**Dequeue** (`internal/queue/queue.go`) is one statement:

```sql
WITH claimed AS (
    SELECT id FROM jobs
    WHERE status = 'pending' AND run_at <= now()
    ORDER BY run_at, created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED      -- other workers' rows are skipped, not waited on
)
UPDATE jobs j
SET status = 'processing', attempts = j.attempts + 1,
    locked_at = now(), locked_by = $2, updated_at = now()
FROM claimed c WHERE j.id = c.id
RETURNING j.*;
```

- **`SKIP LOCKED`** is what makes this scale horizontally: two workers polling in the
  same millisecond get disjoint row sets instead of one blocking on the other.
- **`attempts` is incremented at claim time**, not at failure time. A worker that is
  SIGKILLed still burns an attempt when the reaper requeues the job — otherwise a job
  that reliably crashes its worker would retry forever.
- **A partial index** (`WHERE status = 'pending'`) keeps the hot index small no matter
  how many completed rows accumulate.
- **`run_at`** is how retries and delayed jobs work: a failed job goes back to
  `pending` with `run_at = now() + backoff`, and the dequeue query simply ignores it
  until then. No separate scheduler.

**Retry policy** (`internal/worker/pool.go`): `base * 2^(attempt-1)`, capped at
`RETRY_MAX_BACKOFF`, with ±20% jitter so a batch of simultaneous failures doesn't
retry in lockstep. A handler can opt out of retries entirely by returning
`worker.Permanent(err)` — malformed payloads and 4xx-from-upstream should not burn
five attempts. An unregistered job type is treated as permanent too.

**Stuck-job recovery**: a reaper goroutine ticks every `RECOVERY_INTERVAL` and
requeues anything `processing` with `locked_at` older than `STUCK_JOB_TIMEOUT`
(burying it as `failed` if attempts are exhausted). This is the safety net that
makes at-least-once actually hold when a task is OOM-killed or the AZ goes away.
Keep `STUCK_JOB_TIMEOUT` comfortably above `WORKER_JOB_TIMEOUT`, or the reaper will
requeue jobs that are still legitimately running.

**Graceful shutdown**: on SIGTERM the dispatcher stops claiming, hands any
already-claimed-but-unstarted jobs straight back (`Release`), closes the jobs
channel, and waits up to `WORKER_SHUTDOWN_GRACE` for in-flight handlers. In-flight
jobs deliberately do *not* see the cancellation — `context.WithoutCancel` gives them
their own `WORKER_JOB_TIMEOUT` deadline so a payment call isn't severed mid-flight.
Set the ECS `stopTimeout` above `WORKER_SHUTDOWN_GRACE`.

---

## Adding a job type

```go
// internal/jobs/handlers.go
const TypeResizeImage = "image:resize"

type ResizeImage struct{ S3 *s3.Client }   // real dependencies live on the struct

func (h *ResizeImage) Handle(ctx context.Context, job queue.Job) error {
    var p struct{ Key string `json:"key"` }
    if err := job.DecodePayload(&p); err != nil {
        return worker.Permanent(err)       // never retry a malformed payload
    }
    return h.S3.Do(ctx, p.Key)             // always pass ctx down
}

// then, in Register():
r.Register(TypeResizeImage, &ResizeImage{S3: s3Client})
```

Handlers must be **idempotent** (at-least-once) and must honour `ctx`.

---

## Configuration

Everything is environment variables; see `.env.example`. Only two are required.

| Variable | Default | Notes |
|---|---|---|
| `DATABASE_URL` | — | **required** |
| `JWT_SECRET` | — | **required** for `cmd/api`, min 32 chars |
| `APP_ENV` | `development` | `production` switches Gin to release mode |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | slog |
| `DB_MAX_CONNS` | `10` | keep ≥ `WORKER_POOL_SIZE + 2` |
| `RUN_MIGRATIONS` | `true` | true on the API, false on workers |
| `HTTP_PORT` | `8080` | |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s` | request drain on SIGTERM |
| `DEFAULT_MAX_ATTEMPTS` | `5` | when the request omits `max_attempts` |
| `WORKER_POOL_SIZE` | `5` | goroutines per worker process |
| `WORKER_POLL_INTERVAL` | `1s` | sleep when the queue is empty |
| `WORKER_JOB_TIMEOUT` | `30s` | per-job deadline |
| `RETRY_BASE_BACKOFF` / `RETRY_MAX_BACKOFF` | `5s` / `10m` | |
| `STUCK_JOB_TIMEOUT` | `5m` | must exceed `WORKER_JOB_TIMEOUT` |
| `RECOVERY_INTERVAL` | `1m` | reaper tick |
| `WORKER_SHUTDOWN_GRACE` | `30s` | must be under the ECS `stopTimeout` |
| `WORKER_ID` | hostname | recorded in `jobs.locked_by` |

Bad values are collected and reported together at boot rather than one restart at a
time, and the process exits non-zero — fail fast, not halfway configured.

---

## Deploying to AWS ECS Fargate

```
        Route53 ──▶ ALB (public subnets, :443 ACM cert)
                      │  target group :8080, health check GET /healthz
                      ▼
        ┌──────────────────────────┐        ┌──────────────────────────┐
        │ ECS service: api         │        │ ECS service: worker      │
        │ Fargate, private subnets │        │ Fargate, private subnets │
        │ desired 2, autoscale on  │        │ desired 2, autoscale on  │
        │ ALB RequestCountPerTarget│        │ CPU or a queue-depth     │
        │ awsvpc SG: from ALB only │        │ CloudWatch metric        │
        └────────────┬─────────────┘        └────────────┬─────────────┘
                     └───────────────┬───────────────────┘
                                     ▼
                         RDS PostgreSQL (private subnets)
                         SG: 5432 from the two task SGs only
```

Two ECS services from two images, one ALB in front of the API only — the worker has
no inbound listener at all (no target group, no security group ingress).

**One-time setup**

1. `aws ecr create-repository --repository-name gotaskqueue-api` (and `-worker`).
2. RDS Postgres 16 in private subnets; store the connection string and JWT secret in
   Secrets Manager.
3. Task definitions: `api` (container name `api`, port 8080) and `worker` (no ports).
   Inject secrets via the task definition's `secrets` block so they never appear in
   plaintext env or in `describe-task-definition` output:

   ```json
   "secrets": [
     {"name": "DATABASE_URL", "valueFrom": "arn:aws:secretsmanager:...:gotaskqueue/db-url"},
     {"name": "JWT_SECRET",   "valueFrom": "arn:aws:secretsmanager:...:gotaskqueue/jwt"}
   ],
   "environment": [{"name": "APP_ENV", "value": "production"}],
   "logConfiguration": {"logDriver": "awslogs", "options": {"awslogs-group": "/ecs/gotaskqueue", ...}},
   "stopTimeout": 60
   ```

   `stopTimeout` (max 120s on Fargate) must exceed `WORKER_SHUTDOWN_GRACE`, or ECS
   SIGKILLs mid-drain. The `api` task also needs
   `healthCheck` → `/healthz`, and the ALB target group deregistration delay should
   be ≥ `HTTP_SHUTDOWN_TIMEOUT`.
4. GitHub OIDC provider + a deploy role trusted by your repo; put its ARN in the
   `AWS_DEPLOY_ROLE_ARN` repo secret. It needs `ecr:*` push permissions,
   `ecs:RegisterTaskDefinition`, `ecs:UpdateService`, `ecs:DescribeServices`,
   `ecs:DescribeTaskDefinition` and `iam:PassRole` for the task/execution roles.

**Migrations in production.** The API runs them at boot behind a
`pg_advisory_lock`, so simultaneous tasks can't race. That is fine for additive
changes. For anything destructive, set `RUN_MIGRATIONS=false` everywhere and run a
one-off `aws ecs run-task` with the migration step before the rollout.

**Scaling.** Workers are stateless and idempotent: scale the service to N tasks and
`SKIP LOCKED` keeps them from colliding. A good autoscaling signal is queue depth —
publish `SELECT count(*) FROM jobs WHERE status='pending' AND run_at <= now()` as a
CloudWatch metric and target-track it. Total concurrency is
`tasks × WORKER_POOL_SIZE`; make sure RDS `max_connections` covers
`tasks × DB_MAX_CONNS`.

---

## CI/CD

`.github/workflows/deploy.yml`:

1. **test** — `gofmt -l` check, `go vet`, `go build`, `go test -race`. Runs on PRs too.
2. **push** — matrix over `api`/`worker`, builds the multi-stage image with
   `--build-arg SERVICE=...`, pushes to ECR tagged with the short SHA and `latest`,
   with GHA layer caching. Auth via OIDC, so there are no static AWS keys.
3. **deploy** — `main` only: pulls the live task definition, swaps just the image,
   registers a revision, updates the service and waits for stability so a failing
   rollout fails the build.

---

## Operational notes

- **Retention.** `jobs` grows forever. Add a nightly `DELETE FROM jobs WHERE status
  IN ('completed') AND updated_at < now() - interval '30 days'` (keep `failed` rows
  longer — they are your incident evidence).
- **Dead letters.** `status='failed'` *is* the DLQ. Alarm on
  `count(*) WHERE status='failed'` growth; requeue by setting rows back to `pending`
  with `attempts = 0`.
- **Polling cost.** Each worker issues one query per `WORKER_POLL_INTERVAL` when idle.
  At 1s and a partial index that's negligible; if you ever have hundreds of workers,
  raise the interval or move to `LISTEN/NOTIFY` for wake-ups.
- **Observability.** Every log line is JSON with `service`, `worker_id`, `job_id`,
  `job_type`, `attempt`, `duration` — enough to build CloudWatch Logs Insights
  dashboards without adding a metrics stack. `request_id` is echoed as `X-Request-ID`.

---

## Go notes for the TypeScript/NestJS reader

The code comments call these out where they appear; the short version:

- **No DI container.** Dependencies are constructor arguments wired in `main()`.
  `api.New(cfg, queue, pool, auth, log)` is the whole "module system".
- **`context.Context` is the first argument everywhere** — it carries cancellation
  and deadlines, like an `AbortSignal` you're obliged to thread through.
  `context.WithoutCancel` is how graceful shutdown keeps in-flight work alive.
- **Errors are values.** `fmt.Errorf("...: %w", err)` wraps (like `{ cause }`),
  `errors.Is/As` unwraps. Sentinels (`queue.ErrNotFound`) replace custom error classes.
- **Channels + `sync.WaitGroup` instead of a Promise pool.** `for job := range ch`
  ends when the channel is closed — that is the entire worker shutdown mechanism.
- **`defer`** is `finally`, evaluated LIFO at function exit. Note `defer rows.Close()`
  and `defer ticker.Stop()` — forgetting either leaks.
- **Interfaces are implicit and small.** `Handler` is one method; anything with that
  method satisfies it, no `implements` keyword.
- **Struct tags** (`json:"..."`, `binding:"required"`) drive encoding and validation —
  the closest thing to decorators.
- **`panic` only for programmer errors at startup** (duplicate handler registration);
  request- and job-level panics are recovered and turned into 500s / failed jobs.
