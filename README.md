# Staging Self-Service Platform

Lightweight web UI to make staging self-service for engineering (dev/QA).
Built for an already-busy Ubuntu 18.04 GCP host, so the stack is intentionally
tiny: **one Go binary + SQLite + server-rendered HTML** (no Node/React build,
no separate DB server).

This repository is built in 5 phases (see `../staging_self_service_platform_planning.txt`, section 37).
**All 5 phases are implemented.**

## Phase 1 — read-only dashboard

- Discovers staging **slots** by reading `/data/app/agent-*`.
- Shows, per slot: container status/health (`docker ps`), current **branch +
  commit + version** (from the slot's git repo / `.env` `VITE_GIT_*`), and the
  public **domains** (parsed from nginx `sites-enabled`).
- 100% **read-only** — it never changes anything on staging.

## Phase 2 — auth, RBAC, audit

- **Login required** for the dashboard and API. Accounts use a **username and
  password** — there is no Google/SSO integration.
- **Accounts are admin-provisioned**: signing in never creates an account.
  - On first-ever run (zero users in the DB), the platform creates a
    bootstrap `admin` account with a random password, printed once to the
    server log.
  - After that, an admin creates every other account on `/admin/users`
    (username + optional name + role). A random temporary password is
    generated and shown **once** on screen for the admin to hand out.
- **Mandatory password reset**: every new/reset account is flagged
  `must_reset_password`. The user is forced to `/reset-password` before they
  can use anything else (min 8 characters).
- Admins can **reset any user's password** at any time (`Reset password`
  button on `/admin/users`), which generates a fresh temp password and forces
  the reset flow again.
- Passwords are hashed with **bcrypt** (`golang.org/x/crypto/bcrypt`).
- **RBAC**: roles (`engineering`/`admin`) are set when the account is created
  and managed afterwards in `/admin/users`.
- **Audit log** of privileged actions at `/audit` (admin only): `LOGIN`,
  `LOGOUT`, `CREATE_USER`, `SET_ROLE`, `SET_STATUS`, `RESET_PASSWORD`,
  `SET_OWN_PASSWORD`.
- State is stored in **SQLite** (pure-Go driver, no CGO) at `dbPath`.

## Run with Docker (recommended — this is how it's meant to be deployed)

The host's own Node/Python are too old, so the platform always runs inside a
container. Everything it needs (Go binary, SQLite, bcrypt) is self-contained
in the image; no host dependencies besides Docker itself.

### Deploy to the staging server

```sh
# On the staging server, inside this platform/ directory:
cp config.example.json config.json     # edit paths/deploy/jenkins as needed
docker compose up -d --build
docker compose logs -f staging-platform   # watch for the bootstrap admin password
```

On first run with an empty database the log prints a one-time bootstrap
`admin` username/password — copy it immediately, log in, and set your own
password (you'll be forced to). See "First run / access" in the Runbook below.

To ship a pre-built image instead of building on the server:

```sh
# On a machine with Docker (e.g. this laptop):
docker build -t staging-platform:latest .
docker save staging-platform:latest | gzip > staging-platform.tar.gz
scp staging-platform.tar.gz user@staging-host:/path/

# On the staging server:
gunzip -c staging-platform.tar.gz | docker load
docker compose up -d   # uses the loaded image (comment out `build: .` in
                        # docker-compose.yml, or run: docker compose up -d --no-build)
```

### What gets mounted (read-only where possible)

- `./config.json` → `/app/config.json` (ro) — your configuration.
- `./data` → `/app/data` — **persistent** SQLite DB (users, bookings,
  deployments, audit). Back this directory up.
- `/var/run/docker.sock` (ro) — read `docker ps`/`inspect` for slot status.
- `/data/app` (ro) — read slot git repos/`.env` for version info.
- `/etc/nginx/sites-enabled` (ro) — read domain mappings per slot.

Resource limits are kept low (192 MB / 0.5 CPU) since the host is busy.
A `healthcheck` hits `/healthz` so `docker compose ps` shows real status.

### Updating

```sh
git pull   # or copy the new source
docker compose up -d --build   # rebuilds the image, restarts the container
```
The SQLite DB in `./data` persists across rebuilds/restarts.

## Run locally (for development)

Requires Go 1.21+, plus `docker` and `git` on PATH for discovery.

```sh
cp config.example.json config.json
go run . -config config.json
```

## Configuration (`config.json`)

| key | meaning |
|-----|---------|
| `listen` | HTTP listen address (default `:8088`) |
| `discovery.appRoot` | dir holding `agent-<slot>` folders |
| `discovery.slotDirPrefix` | slot folder prefix (`agent-`) |
| `discovery.repoSubdir` | git repo subfolder used for version info |
| `discovery.nginxSitesDir` | nginx sites-enabled dir for domains |
| `discovery.services` | deployable service names per slot |
| `discovery.refreshSeconds` | dashboard auto-refresh interval |
| `hiddenSlots` | slot names to hide (legacy/dead) |

## Endpoints

- `GET /` — HTML dashboard
- `GET /api/slots` — JSON of the same data
- `GET /healthz` — liveness

## Phase 3 — booking per slot

- `/bookings`: book a slot for a time window, see who holds each slot until when.
- Overlap is rejected transactionally; owner can release, admin can override.
- Bookings auto-expire; the dashboard shows `booked`/`available` per slot.

## Phase 4 — deploy via Jenkins

- `/deploy`: pick service (ws/fs/wbo) + slot + branch **or** tag, one-click deploy.
- Triggers the matching Jenkins job (`deploy-ws/fs/wbo`) via
  `buildWithParameters`, then tracks queue → build → result.
- One active deploy per slot (transactional lock); optional `requireBooking`
  blocks deploying without/over someone else's booking.
- `/deployments` history + `/deployments/<id>` detail with **live console log**
  streamed over SSE.
- Configure under `deploy` in `config.json`. `jenkinsToken` is a **secret** —
  request a platform-specific token and enable with `deploy.enabled: true`.

## Phase 5 — admin, rollback, hardening

- **Manual user management in-app** (`/admin/users`): admins **create every
  account** here (email + name + role) — signing in never creates or changes
  an account. Only the very first login ever (bootstrap, no admin exists yet)
  is allowed without a pre-created account.
  Guards: cannot demote/disable the last admin, cannot disable yourself,
  duplicate emails and disallowed domains are rejected.
- **Slot management** (`/admin/slots`): hide/show slots (legacy/dead) — hidden
  slots disappear from dashboard, booking, and deploy.
- **Controlled rollback** (admin only): from a finished deployment, redeploy the
  previous successful ref for that slot+service via Jenkins.
- **Searchable audit log** (`/audit?q=`).

## Runbook (operations & troubleshooting)

### First run / access
- On first start with an empty database, the platform prints a bootstrap
  **admin** account (`username: admin`, random password) to the server log.
  Log in with it, then set your own password (mandatory).
- From then on, create every other teammate's account on `/admin/users`
  (username + role). A temp password is shown once — hand it out securely.
  They cannot log in before their account is created.
- Locked out (no admin, e.g. everyone disabled)? Stop the app and, in the
  SQLite DB, re-enable an admin directly:
  `UPDATE users SET role='admin', status='active' WHERE username='admin';`
  (Passwords are bcrypt-hashed; to reset one without the UI, use `ResetPassword`
  from a small Go script, or delete the row and restart with 0 users to
  re-trigger bootstrap.)

### Common issues
- **Login denied: invalid username or password** — either wrong credentials,
  the account doesn't exist yet (ask an admin to create it on `/admin/users`),
  or the account is disabled.
- **Stuck on /reset-password** — expected after account creation or an admin
  password reset; set a new password (min 8 chars) to proceed.
- **Dashboard shows no slots** — check `discovery.appRoot` and that the container
  mounts `/data/app` (read-only). Verify `docker ps` works for the app user.
- **Deploy fails immediately** — `deploy.enabled` false, or missing
  `jenkinsURL`/`jenkinsToken`. Check the deployment detail page for the error and
  `/audit` for the DEPLOY entry.
- **"a deployment is already in progress for this slot"** — a slot lock is held.
  It clears when the build finishes; check `/deployments` for the active one.
- **Deploy blocked by booking** — the slot is booked by someone else (or you have
  no booking and `requireBooking` is on). See `/bookings`.
- **Stuck deployment** — the watcher times out after 60 min and marks it
  `error`, releasing the lock.

### Rotating the Jenkins token
Update `deploy.jenkinsToken` in `config.json` and restart. The token is a secret;
never commit it. The token in `deploy-to-staging.md` is leaked — rotate it.

### Backup
Back up the SQLite file at `dbPath` (users, bookings, deployments, audit). The
app only appends; copying the file while stopped is a safe backup.

### Logs
The platform logs to stdout (`docker compose logs staging-platform`). Business
history lives in the DB and is visible under `/deployments` and `/audit`.
# parkee-agent-staging-deployment
