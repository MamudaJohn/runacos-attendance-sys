# CABV Attendance System

Phase 2A experimental: Go/PostgreSQL classroom workflows now include browser acoustic
transmission/reception, QR scanning, optional GPS, room reference comparisons, a BLE
GATT client, and WebAuthn phone confirmation. Physical classroom presence and resistance
to relaying remain unvalidated; accepted check-ins still await lecturer review.

For the lecturer laptop / Android Chrome test, follow [the HTTPS test guide](docs/PHONE_HTTPS_TEST.md).

## Docker package

With Docker Desktop running Linux containers, start the complete local stack:

```powershell
docker compose up -d --build --wait
```

Open http://127.0.0.1:8080. Compose waits for PostgreSQL, runs the versioned
migration command, then starts the non-root server container and checks readiness.
The `cabv:phase2a` image contains server, migration, and explicit demo-seed binaries plus SQL files;
the templates, CSS and JavaScript are embedded. Build tools and local files are excluded.
PostgreSQL is exposed on loopback port 55433 to avoid conflicting with an existing
local database. Set `DB_PORT` to override the host port. The current image is linux/amd64.

```powershell
docker compose ps -a
docker compose logs app migrate
docker compose run --rm migrate cabv-migrate status
docker compose down
```

Stopping the stack preserves PostgreSQL data and private resource files in separate
named volumes (`postgres_data` and `resource_data`). Back up and restore both together.
The image export contains application code; it does not contain uploaded files or
database records. These are local
development settings; public hosting, HTTPS, backups and production credentials
remain future work. Local trusted HTTPS tooling is available. Load the exported image with `docker load -i bin/cabv-phase2a.tar`;
use `docker compose up -d --wait` afterward without `--build`.

## Lecturer laptop and student phone test

Put both devices on the same Wi-Fi, or connect the laptop to the phone's hotspot.
The default Docker binding is localhost. For phone access, set APP_BIND_ADDRESS in
the ignored `.env` file to the laptop's Wi-Fi IPv4 address (find it with `ipconfig`):

```text
APP_BIND_ADDRESS=<laptop-Wi-Fi-IPv4>
```

Run `docker compose up -d --no-build --wait`. Open
`http://<laptop-Wi-Fi-IPv4>:8080/login` on both devices. The laptop signs in as
`lecturer@cabv.test`; the phone uses `student@cabv.test`. Use the existing password
in `.local/demo-credentials.txt`. PostgreSQL stays bound to localhost.

Create/open a new CSC101 session on the laptop. On the phone, refresh CSC101, open
that session, and select **Start check-in**. Then issue a fresh challenge on the
laptop, enter its exact case-sensitive value on the phone, and submit before the
90-second challenge/two-minute form deadlines. The lecturer refreshes the results,
reviews with a reason, and the student refreshes to see the manual decision.

The laptop must stay awake. If the phone cannot connect, check the shared network,
guest/client isolation, and firewall allowance for Docker's app port. This workflow
uses manual token entry and does not request microphone permission. An actual
browser/audio laboratory will need an HTTPS secure context.

When the laptop changes networks, update APP_BIND_ADDRESS to its new Wi-Fi IP.
To end network access, change it to `127.0.0.1` and rerun the Compose command.
Docker Compose reads `.env` for host-binding interpolation; the Go application
itself still reads only process environment configuration.

## Accounts and local demonstration

Account provisioning is administrator-controlled, as selected by the project owner.
There is no public signup. An authenticated administrator can use `/admin/users`
to create a student, lecturer, or administrator account. Passwords must contain
12–128 bytes. Course assignments/enrollments are enforced from the database.

Administrators use `/admin/catalog` to create/edit courses and rooms. Each course
management page assigns/removes lecturers and enrolls/withdraws students by email.
Only active accounts with the appropriate role can be assigned. Changes take effect
on the next request even for existing login cookies. Rosters are administrator-only.
Successful catalog changes and resource uploads record attributed audit events in the
same transaction; failed changes roll back both data and audit. Audit viewing/export
UI remains planned. Course, room, roster and resource lists cap at 100 items; pagination
is still planned.

## Course resources

An assigned lecturer or administrator can upload a file from the course page.
An actively enrolled student, assigned lecturer, or administrator can download it.
Resource access is independent of attendance. Removing an enrollment or lecturer
assignment revokes access on subsequent download requests.

- Allowed files: PDF and UTF-8 plain text (`.pdf`, `.txt`) matching their content.
- Per-file maximum: 5 MiB. HTML, binary text, extension mismatches and empty files are rejected.
- Total private storage: 100 MiB by default and at most 1,000 files, including crash orphans.
- Two concurrent uploads, streamed form-token validation before file content, and
  bounded two-minute upload/download deadlines.
- Downloads use attachment headers, `private, no-store`, and support HEAD/byte ranges.

Files use random storage keys in a private directory outside static assets; original
names never form storage paths. Uploads are staged and renamed atomically. If metadata
or audit insertion fails, the new file is removed. A crash between the filesystem
write and database commit can leave a quota-counted orphan; reconciliation tooling is
planned. Quota enforcement is designed for one app replica, as provided by Compose.

For native execution, RESOURCE_DIR defaults to `.local/resources`; Compose uses its
persistent `/app/resources` volume. RESOURCE_QUOTA_BYTES defaults to 104857600 and
can range from 5 MiB to 1 TiB. The 1,000-file cap stays fixed. PDFs receive signature
and end-marker checks; malware scanning/sanitization, resource deletion, and coordinated
backup/recovery tests remain future work.

## Local demonstration

The seed command is an explicit development-only bootstrap. It creates
`admin@cabv.test`, `lecturer@cabv.test`, and `student@cabv.test`, two courses and one
room. Only CSC101 is assigned to the lecturer and student; administrators see both
courses. It requires a supplied password, never runs at startup, and preserves
existing account passwords/details when repeated.

```powershell
# Enter a development-only password without echoing it to the console.
$demoSecret = Read-Host 'Demo password (12–128 bytes)' -AsSecureString
$env:CABV_DEMO_PASSWORD = [System.Net.NetworkCredential]::new('', $demoSecret).Password
docker compose exec -e CABV_DEMO_PASSWORD app cabv-seed
Remove-Item Env:CABV_DEMO_PASSWORD
```

For this local workspace, demonstration accounts have been seeded. Their generated
password is in the ignored `.local/demo-credentials.txt`; no credentials are included
in the image or source. Use the admin account to try account creation. Password
change/reset, institutional identity verification, account deactivation UI, and
production administrator bootstrap remain planned.

## Attendance prototype

The reviewed threat model and API contract are in [docs/attendance-contract.md](docs/attendance-contract.md).
This milestone decodes acoustic challenges and supports QR, optional sensor measurements
and phone authenticator signatures. A copied or relayed challenge can still be submitted;
successful token validation creates **pending** attendance, never automatic approval.

To try the browser workflow at http://127.0.0.1:8080:

1. Sign in as `lecturer@cabv.test`, open CSC101, and create a draft session with room,
   title and duration. Open it to start its 1–180-minute database-clock window.
2. Issue a challenge. The 256-bit value is returned only to authorized staff once;
   issuing another challenge revokes the previous value. It lasts 90 seconds or until
   the session ends. Reloading the page does not recover the plaintext token.
3. In a separate browser/profile, sign in as `student@cabv.test`, open that session,
   receive the challenge by sound or QR on trusted HTTPS, or use manual entry for
   HTTP development. Choose whether to include GPS/room measurements and submit.
   When identity is configured, register a phone authenticator with your password
   and confirm each submission using the device's fingerprint, face or PIN prompt.
4. The student gets **Pending lecturer review**. As the lecturer, review the student,
   choose present/rejected, and give a reason. The outcome is explicitly attributed
   to manual review; existing attempts and prior reviews remain intact.
5. Close attendance. New submissions/challenges stop; review remains available for
   currently enrolled students after closing or expiry. Closed sessions cannot reopen.

Nonces are 256-bit values bound to the student/session, expire after two minutes or
session end, and are used once. Fresh nonce issuance invalidates the student's older
unused nonce. Tokens and nonces are stored as digests, not plaintext. Verification,
nonce consumption, duplicate prevention and audit writes are transactional. Rejected
attempts do not overwrite prior accepted/reviewed records. At most 50 nonces and 20
verification attempts per student/session, and 1,000 challenges per session, are allowed.

Only active enrolled students can submit or see their own result. Only assigned
lecturers/administrators can manage challenges and class results. Staff history
retains withdrawn/disabled students' recorded outcomes, with further review disabled
until the account/enrollment is eligible again. Lists cap at 100; pagination is planned.
Times display in Africa/Lagos (WAT); API timestamps are UTC. The prototype serializes
session operations for correctness; high-volume load testing remains a pilot gate.

The versioned JSON API uses session cookies and a session-bound `X-CSRF-Token` header.
Request bodies are capped at 4 KiB and enforce per-endpoint fields; HTML forms retain
the 8 KiB cap. Student responses never return issued challenges or another student's
result. All attendance responses use no-store caching. See the contract for endpoints,
rejection semantics, manual-review policy, and transaction limits.

Offline approval, acoustic/device compatibility, boundary accuracy, and relay resistance
are unvalidated. Real classroom/device experiments remain separate from these software
tests. Manual review, appeals, consent and retention need institution policy before a pilot.

Sessions expire after 12 hours and logout revokes them in PostgreSQL. Only token
digests are stored in the database. Cookies are HttpOnly and SameSite=Lax. Secure
cookies default to enabled and use the `__Host-` prefix; local Compose explicitly
sets COOKIE_SECURE=false for loopback HTTP. Enable secure cookies behind HTTPS.
Login/logout/account forms enforce CSRF and browser origin checks. PBKDF2-HMAC-SHA256
uses 600,000 iterations and an independent random salt, following
[OWASP's PBKDF2 guidance](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html).
Hashing concurrency is capped at two. Login limits are ten attempts per normalized
account and 1,000 per remote IP per 15 minutes; each limiter holds at most 4,096 keys.
These process-local limits reset on restart. Forwarded IP headers are ignored;
trusted proxy and pilot traffic policies need review before public hosting.

## Implementation checklist

- [x] Go server and validated configuration.
- [x] Rendered home page and lightweight CSS.
- [x] Process liveness and PostgreSQL readiness endpoints.
- [x] Bounded connection pool, HTTP timeouts and shutdown handling.
- [x] Reversible PostgreSQL bootstrap migration and migration CLI.
- [x] Unit tests and disposable-database migration integration tests.
- [x] CI test/vet/build definition.
- [x] Docker image and full Compose stack with ordered migrations.
- [x] Student, lecturer and administrator login/logout and permissions.
- [x] Administrator account-creation form.
- [x] Course/enrollment/room schema and role-specific course dashboards.
- [x] Administrator course, enrollment, assignment and room management UI.
- [x] Authorized course resource upload/download with limits and persistent storage.
- [x] Transactional audit events for catalog changes and resource uploads.
- [x] Attendance session lifecycle, challenge/nonce checks, duplicate prevention and audited lecturer decisions.
- [x] Attendance permission, expiry, replay, concurrency and rollback tests.
- [x] Audible and experimental near-ultrasonic transmitter/streaming decoder with synthetic audio tests.
- [x] Lecturer attendance QR and camera scanning with a locally bundled fallback decoder.
- [x] Optional GPS and audible room-probe reference comparisons for review.
- [x] Browser BLE GATT receiver and documented peripheral protocol (real hardware not tested).
- [x] Signed WebAuthn phone confirmation, required user verification, revocation and audited recovery.
- [x] Local HTTPS certificate tool and Compose HTTPS configuration.
- [ ] Real-phone audio/biometric tests, BLE peripheral integration and classroom boundary/relay trials.
- [ ] PWA caching, HTTPS hosting, backups and pilot deployment.

The portal supports classroom administration, resources, pending check-ins and manual
attendance decisions. It does not establish verified acoustic presence.

## Run locally

Install a supported Go toolchain (Go 1.26 or newer) and Docker with Compose.
The checked dependency versions are recorded in go.mod and go.sum.
From the repository root, in PowerShell:

This workspace also has a checksum-verified Go 1.27.2 toolchain under the ignored
`.tools` directory. To use it without installing Go globally, first run:

```powershell
$env:PATH = (Join-Path $PWD '.tools/go/bin') + ';' + $env:PATH
$env:GOPATH = Join-Path $PWD '.tools/gopath'
$env:GOCACHE = Join-Path $PWD '.tools/gocache'
```

```powershell
docker compose up -d --wait db
$env:DATABASE_URL = 'postgres://cabv:cabv_dev_only@localhost:55433/cabv?sslmode=disable'
$env:HTTP_ADDR = '127.0.0.1:8080'
$env:COOKIE_SECURE = 'false'
go run ./cmd/cabv-migrate up
go run ./cmd/cabv-server
```

Open http://127.0.0.1:8080. `GET /healthz` reports process liveness;
`GET /readyz` returns 200 when PostgreSQL responds and 503 otherwise. Readiness
does not check migration version. Unknown routes return 404; unsupported methods
return 405. Ctrl+C performs graceful shutdown.

Configuration comes from the environment; `.env.example` is documentation and is
not loaded automatically. DATABASE_URL is required. HTTP_ADDR defaults to loopback
port 8080. The supplied password and disabled TLS are for local development only;
hosting needs credentials, PostgreSQL TLS as appropriate, and an HTTPS proxy.

Run migrations explicitly before serving. From the repository root:

```powershell
go run ./cmd/cabv-migrate status
go run ./cmd/cabv-migrate up
# Destructive: removes the most recently applied migration and its data.
go run ./cmd/cabv-migrate down
```

Migrations are versioned by Goose and applied transactionally. The initial migration
creates app_metadata; migration 00002 adds accounts, sessions, courses, assignments,
enrollments, and rooms; migration 00003 adds resources and audit events.
Migration 00004 adds attendance sessions, hashed challenges/nonces, immutable attempt
and review history, and unique per-student attendance records.
Do not run competing migration commands. `docker compose down` preserves the volume.

## Verify

```powershell
go test ./...
go vet ./...
go build ./...
```

The database integration test is skipped unless TEST_DATABASE_URL is provided.
Tests create isolated temporary schemas, apply, repeat, roll back, and reapply
migrations, and verify account/session, catalog, resource, quota, failure-cleanup,
attendance lifecycle/replay/expiry/concurrency, and HTTP authorization boundaries. Use a
**disposable** database; never point this variable at production.
CI provides a disposable PostgreSQL 17 service and runs all checks.

```powershell
$env:TEST_DATABASE_URL = 'postgres://cabv:cabv_dev_only@localhost:55433/cabv_test?sslmode=disable'
go test ./... -count=1
```

Create `cabv_test` separately before this command. Normal development uses `cabv`.

## Architecture and next gate

Go remains the handoff default because no previous implementation exists.
Use a modular monolith: net/http handles the few current routes without a router
dependency; pgx provides a bounded pool (five connections); Goose supplies established
SQL migration tracking. Embedded templates and static assets avoid runtime working
directory dependencies for the server. The migration command uses the root migrations
directory. No frontend framework or JavaScript is needed for this milestone.

HTTP timeouts, bounded headers, database connection timeouts and graceful shutdown
provide a small operational foundation. Database connection errors are not returned
to clients. The database holds account/enrollment data, token digests and attributed
attendance decisions; private course files are separate. Optional spectral measurements
and GPS observations are untrusted supporting evidence. Raw audio and biometric samples
never reach the server. Observation data expires after seven days and is sanitized at
startup and hourly; audit references are retained. Room reference data lasts until replaced.

Administrator-created accounts and enrollment management are implemented. Roster
import, password recovery and institutional identity policies remain planned.
The first requested physical test device is Android with Chrome; other devices remain
uncharacterized. The acoustic challenge threat model and API contracts have been reviewed.
Physical device compatibility, presence detection and relay resistance need field trials.
