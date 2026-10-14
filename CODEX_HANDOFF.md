# CODEX HANDOFF — CABV Attendance System

**Project:** CABV (Classroom Acoustic Boundary Verification)  
**Track:** A — Web 1.0 prototype  
**Handoff date:** 2026-10-14  
**Audience:** OpenAI Codex in VS Code / project developers  
**Status:** Phase 0 and Phase 1A/1B/1C classroom workflows implemented and locally verified on 2026-10-14. Attendance validates prototype tokens and records lecturer review; acoustic decoding and physical-presence validation remain planned/experimental.

> **Truth convention:** **[CONFIRMED]** = explicitly established in the project conversation. **[RECOMMENDED]** = proposed engineering decision, not yet approved or implemented. **[EXPERIMENTAL]** = hypothesis requiring measurements. **[UNKNOWN]** = not evidenced. **[FUTURE]** = later-stage scope.
>
> **Do not assume a codebase exists.** No repository, source files, database, deployed service, passing tests, or implemented feature was supplied or inspected in this conversation. If a repository is open in VS Code, inspect it before changing code and update this document with verified findings.

## 1. Project summary and non-negotiable constraints

**[CONFIRMED]** Build a lightweight internet-hosted attendance and classroom resource website for lecturers and students. Attendance should require evidence of physical presence in the assigned lecture room or lecture theatre; merely receiving a URL via WhatsApp/social media must not suffice. The aspirational policy is **“Not Within the Walls, No Attendance.”**

**[CONFIRMED]** Available hardware: lecturer phone, student phones, optionally an already available speaker. No classroom Wi-Fi/router, new dedicated devices, or reliable fast internet. Student access must work on slow mobile connections. Lecture rooms are smaller rectangular spaces; lecture theatres are larger and acoustically different. Attendance and course-resource access are separate functions.

**[CONFIRMED]** Develop in four generations:
1. **Web 1.0:** Browser attendance/resource portal; acoustic challenge prototype; experimental measurements.
2. **Native 2.0:** Mobile apps with broader access to BLE, audio, sensors, and supported ranging/security APIs.
3. **Spatial 3.0:** Room-specific measured geometry, acoustics, doorway tests, and calibrated classifiers.
4. **Adaptive 4.0:** Guided onboarding/calibration for additional rooms and theatres.

**[CONFIRMED]** The acoustic signal should ideally be inaudible or minimally distracting. Near-ultrasonic 17–19 kHz signals were discussed as a candidate, **not** validated as compatible or inaudible for all users.

**Security reality:** No commodity-phone browser solution guarantees exact wall-boundary enforcement or immunity to real-time relaying. Acoustic, QR, BLE, GPS, and room fingerprint signals are supporting evidence, not unforgeable proof. Explicitly report false acceptance, false rejection, and relay success rates. Avoid claiming verified physical presence based solely on a client-reported measurement.

## 2. Implementation status — verified vs planned

### Workspace inspection and Phase 0 evidence (2026-10-14)

**[CONFIRMED: inspected]** Before bootstrap, the workspace contained only this handoff.
There was no Git repository, source code, README, tests, migrations, or deployment setup,
and no applicable AGENTS.md was found. No existing implementation warranted changing
the recommended Go/PostgreSQL approach. The feature rows below describe later scope.

**[CONFIRMED: implemented and locally verified]** Phase 0 now contains:
- Go modular-monolith skeleton using net/http and pgx; validated environment configuration,
  HTTP timeouts, bounded connection pool, and graceful shutdown handling.
- Embedded server-rendered home page and CSS; GET /healthz for process liveness and
  GET /readyz for database connectivity (not schema-version readiness).
- Explicit Goose migration command with up/down/status and a reversible app_metadata
  bootstrap migration. The conceptual domain schema in §6 remains unimplemented.
- PostgreSQL 17 Docker Compose development definition, .env.example, README commands,
  pinned Go dependencies, and a GitHub Actions test/vet/build workflow.

**[CONFIRMED: verification]** Go 1.27.2 windows/amd64 was downloaded from go.dev and
verified against the published SHA-256. Dependencies require Go 1.26 or newer.
An isolated workspace PostgreSQL 15.4 instance on 127.0.0.1:55432 was used for testing;
no existing database was modified. `go test ./... -count=1` passed with
TEST_DATABASE_URL enabled, including apply, repeated apply, rollback, and reapply.
`go vet ./...` and `go build ./...` passed. Live HTTP checks returned 200 for /,
/static/site.css, /healthz, and /readyz; HTML plus CSS measured 723 bytes uncompressed.
After stopping PostgreSQL, /readyz returned 503 while /healthz remained 200. The test
server and database processes were stopped afterward. Unit tests cover configuration,
routes, methods, and readiness failure without leaking database details.

**[CONFIRMED: Docker packaging follow-up, 2026-10-14]** The installed Go 1.27.2 and
running Docker Desktop Linux engine were accessed successfully outside the sandbox.
A multi-stage Dockerfile builds the server and migration CLI into a non-root
linux/amd64 image (`cabv:phase0`). Compose now starts PostgreSQL, runs migrations,
and starts the server with a database readiness healthcheck. The initial host port
5432 could not be bound; local PostgreSQL access now defaults to 127.0.0.1:55433
(override with DB_PORT). The application is served at http://127.0.0.1:8080.

Compose configuration validation, image build, migration application and container
health checks passed. PostgreSQL 17.11 was confirmed by SQL query. All Go tests passed
with TEST_DATABASE_URL pointing to a separate disposable database in that container;
vet/build passed with the installed toolchain. Live checks for /, /static/site.css,
/healthz and /readyz returned 200. The image runs as user cabv (UID 100). An exported
image is available at bin/cabv-phase0.tar (13,675,008 bytes); it contains application
binaries and migrations, not database records. The local Docker stack was left running
for review. README contains startup, stop, image loading and implementation checklist.

### Phase 1A verified checkpoint (2026-10-14)

**[CONFIRMED: user decision]** Real accounts will be administrator-created.
Public signup, roster import, and institution single sign-on are not implemented.

**[CONFIRMED: implemented]** Migration 00002 adds users, hashed database sessions,
courses, lecturer assignments, active/withdrawn enrollments, and rooms. Login/logout,
administrator/lecturer/student dashboards, authorized course details, a course-list
JSON endpoint, and an administrator-only account creation form are implemented.
Course listings currently cap at 100; pagination and administration of course/room/
enrollment records remain planned. Permissions are derived from current database
account roles, active status, assignments and enrollments, rather than cookie claims.

Passwords use independently salted PBKDF2-HMAC-SHA256 (600,000 iterations); passwords
are bounded to 12–128 bytes when created. Sessions use random 256-bit tokens,
store only token hashes, expire after 12 hours, and are revoked by logout. Login
rotates/revokes an existing cookie session. Secure HttpOnly host-only cookies are the
default; the local HTTP Compose stack explicitly disables Secure. Secure mode uses
__Host- cookie prefixes. Login/logout/account creation validate CSRF and browser
origin metadata. Login has process-local per-account (10) and shared-IP (1,000)
15-minute quotas with bounded maps; expensive hashing concurrency is capped at two.
Production proxy trust, rate policy, password recovery/change and identity checks
remain future work.

An explicit cabv-seed command creates demonstration accounts/course relationships
using a supplied password; it never runs at server startup and preserves existing
account passwords. Local demonstration accounts were seeded with a generated
password stored only in ignored .local files. The image does not contain credentials
or database records. Current Docker tag is cabv:phase1a; exported image is
bin/cabv-phase1a.tar (17,359,360 bytes) with a SHA-256 sidecar. Phase 0 archive is retained
as an older artifact. Current stack remains running at http://127.0.0.1:8080.

**[CONFIRMED: verification]** `go test ./... -count=1`, `go vet ./...`, and
`go build ./...` passed using installed Go 1.27.2 and Docker PostgreSQL 17.11.
Integration tests use isolated disposable schemas: migration full rollback/reapply,
unique/check/foreign-key constraints, active accounts, current roles, session expiry/
revocation, enrollment removal, admin account creation and duplicate rejection,
course authorization, CSRF rejection, origin checks, login rotation, bounded forms,
and account throttling. Live HTTP login/dashboard/course access/admin-boundary/CSRF/
logout checks passed for all three demo roles, plus /healthz and /readyz. Docker image
build, automatic migration and container health checks passed while retaining the
existing database volume. Connected browser tools exposed no available browser;
visual browser review has not been completed.

### Phase 1B verified checkpoint (2026-10-14)

**[CONFIRMED: implemented]** Administrators can create/edit courses and rooms from
/admin/catalog, assign/remove active lecturers, and enroll/withdraw active students
by email on /admin/courses/{id}. Course rosters remain administrator-only. Current
database roles and active status are rechecked; membership changes revoke access on
subsequent requests with existing cookies. Listings cap at 100; bulk import and
pagination remain planned. Disabled/demoted account relationship cleanup requires
future account-administration policy.

Assigned lecturers and administrators can upload PDF/UTF-8 text resources from the
course page. Authorized course members can download attachments, including HEAD and
byte ranges. Resource access is independent of attendance. Uploads validate origin
and streamed CSRF before reading file data, limit one file to 5 MiB, and cap concurrent
uploads at two. HTML/binary text, mismatched extensions, and empty files are rejected.
PDF checks inspect signatures/end markers, not malware or complete PDF structure.

Private files use random 256-bit storage keys, rooted filesystem access, atomic
staging/rename, and 0600 files in a 0700 directory. Compose persists files in the
resource_data volume separately from postgres_data. RESOURCE_DIR defaults to
.local/resources for native runs and /app/resources in Compose. Default quota is
100 MiB and at most 1,000 stored entries including crash orphans; enforcement assumes
one app replica. Resource transfers have bounded two-minute deadlines. Download
responses retain private/no-store headers, including range-error responses.

Migration 00003 creates resources and audit_events, advancing schema_phase to 1b.
Catalog changes and resource uploads write attributed audit events in the same
transaction. Upload metadata rechecks and locks current authority; failed database
or audit commits remove new files. Filesystem and database commits are not a single
transaction: crash orphan reconciliation and coordinated backup/restore are future
work. Resource deletion, scanning/sanitization, and audit viewing/export UI are planned.

**[CONFIRMED: verification]** Installed Go 1.27.2 and Docker PostgreSQL 17.11:
`go test ./... -count=1`, `go vet ./...`, and `go build ./...` passed with disposable
isolated database schemas. Tests cover admin boundaries, duplicate/invalid catalog
inputs, correct-role membership, immediate revocation, atomic audit rollback,
migration 1B rollback/reapply/full-chain rollback, path/symlink isolation, file type,
size/quota/entry caps, concurrent exhaustion, multipart/CSRF/origin errors, failed
upload cleanup, GET/HEAD/206/416 authorization and private-cache headers. Focused
catalog/routes tests passed after final template changes. Read-only security review
found no remaining confirmed blocker in this milestone; this is not a production audit.

The Docker stack built and passed readiness checks, applied migration 00003, and
retained the existing database volume. Live HTTP checks created a DEMO1B practice
course and demo room, assigned/enrolled the demo users, uploaded/downloaded a text
resource, checked byte ranges and student upload denial, and verified withdrawal/
assignment removal with existing sessions. The demo users' CSC101 access was preserved;
DEMO1B remains accessible to the administrator for review. Resource download persisted
after container recreation. All three login/logout/role-flow checks plus health and
readiness passed afterward. Storage ownership confirmed cabv with mode 0700; app UID100.

Current image is cabv:phase1b, exported as bin/cabv-phase1b.tar (17,410,560 bytes) with
SHA-256 sidecar. Earlier Phase 0/1A archives are retained as historical artifacts.
Image exports contain application code and migrations, not database records,
credentials, or uploaded files. The local stack remains running at http://127.0.0.1:8080.
README and implementation checklist are updated; existing ignored demo credentials
are unchanged. Visual browser review remains pending because connected browser tools
exposed no available browser.

### Phase 1C attendance verified checkpoint (2026-10-14)

**[CONFIRMED: contract reviewed]** docs/attendance-contract.md defines authorization,
session lifecycle, nonce consumption, challenge expiry, duplicates, manual decisions,
HTTP endpoints and software tests. A separate read-only security review inspected the
contract, schema, SQL transactions and HTTP/template code. The chosen policy is
token-review-v1: valid token possession creates pending lecturer review. This milestone
does not claim acoustic sensing, anti-relay protection, or verified classroom presence.

**[CONFIRMED: implemented]** Assigned active lecturers/administrators can create draft
sessions for a course and room, open a 1–180-minute database-clock window, issue/rotate
short-lived challenges, close attendance permanently, see results, and record manual
present/rejected decisions with mandatory reasons. Current account role, assignment
and enrollment are enforced. Students can obtain a student/session-bound nonce,
submit a manually entered prototype challenge, and see only their own outcome.
Student pages/API responses never return an issued challenge or class roster.

Challenges and nonces use 256-bit randomness and SHA-256 digests at rest. Challenge
TTL is 90 seconds, nonce TTL two minutes; both clip at session end. Rotation revokes
earlier challenges; issuing a nonce retires the student's older unused nonce. Session
closing revokes challenges/unused nonces. Rejected/replayed submissions do not replace
existing accepted/reviewed attendance. A unique(session,student) record plus consistent
transaction locks prevents concurrent duplicates. Authority and expiry are checked
after waiting for locks using the PostgreSQL wall clock. Attempts, nonce use, records
and audit writes commit together or roll back. Limits persist in PostgreSQL: 20 attempts
and 50 nonces per student/session, 1,000 challenge generations per session.

Manual decisions append review/audit history and retain original attempts/source
attempt references. Review is permitted after closure/expiry for currently active
enrolled students, and forbidden for drafts that never opened. Staff results retain
withdrawn/disabled students with recorded history; ineligible rows cannot be reviewed.
API JSON input has a 4 KiB limit, endpoint-specific fields, cookie authentication,
same-origin checks and session-bound CSRF headers. HTML forms use existing 8 KiB
limits and hidden CSRF. UI times use Africa/Lagos (WAT) with embedded timezone data.
Listings cap at 100; pagination and high-volume performance testing remain future work.

Migration 00004 creates attendance_sessions, acoustic_challenges, submission_nonces,
verification_attempts, attendance_records and attendance_reviews; schema_phase=1c.
Acoustic table naming prepares for later integration; current token entry is manual.
The schema rejects partial session times and cross-session/student source-attempt
references. Attendance timestamps use post-lock clock_timestamp() to retain review
order under concurrent transactions.

**[CONFIRMED: verification]** Installed Go 1.27.2 and Docker PostgreSQL 17.11:
`go test ./... -count=1`, `go vet ./...`, and `go build ./...` passed with isolated
disposable schemas. Focused attendance HTTP/HTML tests passed after final UI changes.
Coverage includes lifecycle, manager/student boundaries, nonce ownership/expiry/use,
challenge expiry/revocation, wrong-token rejection, prior-record preservation, concurrent
submissions producing one record, audit rollback, delayed-lock expiry, manual-review
chronology, withdrawal/disabled-history retention, schema constraints, full migration
rollback/reapply, CSRF/origin checks, malformed/oversized JSON, endpoint field restrictions,
student secret/roster leaks, and invalid manual verification status rejection.

The Docker stack was upgraded to cabv:phase1c, migration 00004 applied, and containers
became healthy while preserving existing accounts/courses/resource volumes. Live HTTP
tests created a CSC101 demonstration session, opened it, issued a challenge and nonce,
and submitted four simultaneous requests with that nonce: one pending result and three
nonce_used rejections. Fresh-nonce duplicate handling, challenge rotation, private
student views, closure/reopen denial, and audited manual review after closing passed.
The final demonstration record is present with source=manual and an explicit simulated
workflow-test reason, not a classroom observation. SQL confirmed one record, seven
attempts and zero duplicate groups. A closed demonstration session remains in CSC101
for user inspection. Login/logout/role/health checks and earlier resource persistence
checks passed afterward. Demo credentials are unchanged.

Current export: bin/cabv-phase1c.tar (17,558,016 bytes) with SHA-256 sidecar. Prior archives
are retained. Image archives contain code/migrations, not credentials, database records
or uploaded files. Stack remains running at http://127.0.0.1:8080. README includes a
manual test walkthrough; .local/verify_attendance.py is an ignored live-test helper.

### Local phone-test access (2026-10-14)

**[CONFIRMED: configured]** The project owner requested lecturer=laptop and student=phone.
Compose app host binding is now configurable through APP_BIND_ADDRESS (default loopback).
The ignored local .env currently selects the laptop Wi-Fi address 10.218.25.223;
the test URL is http://10.218.25.223:8080/login on both devices. PostgreSQL remains
127.0.0.1:55433. The laptop and phone must share a Wi-Fi/hotspot network.

The app container is healthy and host-side Wi-Fi URL checks returned 200 for /, /login,
/healthz and /readyz. Admin/lecturer/student login, role boundaries, CSRF and logout
checks passed over this Wi-Fi URL. Docker already has enabled inbound firewall rules
for the active network profile; no Windows firewall rules or network profiles were
changed. Actual phone browser loading and device layout require the owner's test.
README includes the two-device workflow and rollback: set APP_BIND_ADDRESS back to
127.0.0.1 and recreate with Compose. Update the saved address after network/IP changes.
Application code and the Phase 1C image are unchanged; current testing uses manual
challenge entry. No microphone access or acoustic reception is implemented.

**[UNVERIFIED: remaining scope]** Acoustic/device decoding, real classroom boundary
accuracy, relay resistance/FAR/FRR, visual browser/mobile review, hosted CI, public
HTTPS deployment, backup/restore, consent/appeals/retention policy, and pilot load
testing remain unverified. Software validation does not establish physical presence.

| Feature | Status | Evidence / note |
|---|---|---|
| Requirements and staged roadmap | **[CONFIRMED: documented]** | Discussed in conversation |
| Architecture and proposed schema | **[PARTIAL: Phase 1C implemented]** | Platform/resources/attendance/audit tables migrated; experimental device/research schema remains conceptual |
| Student / lecturer / administrator login | **[CONFIRMED: locally verified]** | Database sessions, role dashboards, logout, CSRF and live HTTP checks |
| Administrator account creation | **[CONFIRMED: locally verified]** | Admin-only form, database authority check, password hashing, duplicate prevention |
| Course enrollment and role permissions | **[CONFIRMED: locally verified]** | Admin management UI and database-backed authorization; enrollment/assignment revocation tested |
| Course and room administration | **[CONFIRMED: locally verified]** | Create/edit UI, validation, duplicate prevention and transactional audit events |
| Attendance sessions and challenge validation | **[CONFIRMED: software locally verified]** | Session lifecycle, short-lived tokens, one-use nonces, transactions, duplicate/replay checks; acceptance=pending review |
| Lecturer attendance review | **[CONFIRMED: locally verified]** | Present/rejected manual decisions, required reason, audit attribution and retained history |
| Acoustic transmitter / decoder | **[UNKNOWN: not verified]** | Prototype not supplied |
| Silent / near-ultrasonic mode | **[EXPERIMENTAL: not verified]** | Requires device testing |
| QR challenge | **[UNKNOWN: not verified]** | Planned secondary mechanism |
| GPS, room fingerprinting, BLE | **[EXPERIMENTAL / FUTURE]** | Not verified |
| Resource upload/download portal | **[CONFIRMED: locally verified]** | Authorized PDF/text uploads and private attachment downloads, limits, persistent volume and cleanup tests |
| Offline/PWA asset caching | **[UNKNOWN: not verified]** | Planned |
| Database migrations and tests | **[CONFIRMED: Phase 1C locally verified]** | Four Goose migrations; isolated unit/integration tests, vet/build and deployed HTTP workflow checks passed |
| Local Docker package | **[CONFIRMED: verified]** | Non-root image built/exported; PostgreSQL 17.11, migrations, container health and HTTP smoke checks passed |
| Hosted deployment | **[PLANNED: not verified]** | No public hosted service or hosted CI run |

**Known bugs:** **None currently confirmed by the recorded tests.** No issue tracker exists. Limitations and pending validation are recorded above; anticipated risks in §11 are not evidence of reproduced defects.

## 3. Backend choice: Go vs Rust vs Python

**[RECOMMENDED] Choose Go for Web 1.0.** Use a modular monolith with a small JSON API and server-rendered HTML/vanilla JS. Go's standard library, simple concurrency model, single-binary deployment, and modest typical runtime overhead suit a small VPS and a fast prototype. Backend language alone does not determine cost: database hosting, file storage, network egress, traffic, and operational complexity also matter.

| Criterion | Go | Rust | Python/Django |
|---|---|---|---|
| Speed to first working prototype | Strong | Slower learning/compile cycle | Very strong with built-in admin/auth |
| Runtime memory/CPU | Generally efficient | Generally very efficient | Often higher overhead, workload-dependent |
| Deployment | Single compiled binary | Compiled binary | Interpreter + app server |
| Authentication/admin scaffolding | More custom integration | More custom integration | Mature batteries-included ecosystem |
| Recommendation | **Web 1.0 default** | Later specialized modules if needed | Fallback if team already knows Django best |

**Decision gate:** If the developer is significantly more experienced with Django than Go and has a strict near-term deadline, Django may deliver a correct prototype sooner. Do not rewrite a functioning backend merely for speculative hosting savings. Rust is not required for browser audio processing; that processing runs on the client.

### Proposed stack — not implemented

- **Backend:** Go (current stable toolchain at build time), `net/http` + `chi` router or standard library only; `pgx` PostgreSQL driver; SQL migrations via `golang-migrate` or Goose; `slog` structured logging.
- **Database:** PostgreSQL; use a local Docker Compose environment for development if Docker is available. SQLite may be a temporary local-only convenience but should not silently replace the planned production database.
- **Frontend:** Server-rendered HTML templates, CSS, vanilla JavaScript; no heavy SPA dependency.
- **Audio:** Web Audio API, AudioWorklet where supported, `getUserMedia()` with user consent; signal analysis in JS; optional WebAssembly only after profiling.
- **PWA:** Service worker caches static shell and opted-in course resources, **not** attendance approvals or private API responses.
- **Hosting:** Small Linux VPS, HTTPS reverse proxy (Caddy or Nginx), Go binary, PostgreSQL, file storage and backups.
- **Testing:** Go unit/integration tests, browser/device tests, scripted signal-processing tests, security tests.
- **Research analysis:** Python/NumPy/SciPy/scikit-learn for offline experiments only, independent of Go production backend.

## 4. Suggested repository structure

**[RECOMMENDED — create only after checking existing repository]**

```text
cabv/
├── CODEX_HANDOFF.md
├── README.md
├── .env.example
├── .gitignore
├── go.mod
├── cmd/
│   └── cabv-server/main.go
├── internal/
│   ├── config/
│   ├── auth/
│   ├── users/
│   ├── courses/
│   ├── rooms/
│   ├── attendance/
│   ├── challenges/
│   ├── resources/
│   ├── experiments/
│   ├── audit/
│   └── store/
├── migrations/
├── web/
│   ├── templates/
│   │   ├── lecturer/
│   │   ├── student/
│   │   └── shared/
│   └── static/
│       ├── css/
│       ├── js/
│       │   ├── audio-transmitter.js
│       │   ├── audio-receiver.js
│       │   ├── signal-codec.js
│       │   ├── attendance.js
│       │   └── sw.js
│       └── icons/
├── research/
│   ├── protocols/
│   ├── fixtures/
│   ├── analysis/
│   └── results/            # anonymized outputs only
├── tests/
├── deploy/
│   ├── Dockerfile
│   └── compose.yaml
└── docs/
    ├── architecture.md
    ├── threat-model.md
    ├── api.md
    └── privacy.md
```

Do not commit secrets, real student records, raw audio, or identifying experimental datasets.

## 5. Roles and key user journeys

**Roles:** administrator, lecturer, student. A lecturer may manage only authorized courses; a student may access only enrolled courses and their own attendance.

**Attendance:** lecturer creates/opens a session → server issues unpredictable, short-lived challenge → teacher phone emits acoustic frame and may display QR → logged-in student grants microphone permission and decodes challenge → student sends challenge response and server-issued one-time nonce → server checks identity, enrollment, session window, token validity, replay, duplicates → stores verified/pending/rejected result with reason and audit event. **Do not expose active acoustic token in any student-accessible API or HTML.**

**Resources:** lecturer uploads a file to a course → server validates authorization, file size/type and storage path → enrolled student views list/downloads. Resource access does not require acoustic presence verification.

**Fallback:** If sensing is unsupported or internet fails, mark request pending or offer logged lecturer review; do not silently award verified attendance. Manual overrides must be attributed and audited.

## 6. Proposed database schema

**[PARTIAL: platform/attendance schema implemented]** Users, courses, course_lecturers,
enrollments, rooms, resources, audit_events and attendance workflow tables are migrated,
plus sessions and attendance_reviews. device_profiles, experiments and measurements
remain recommended/unimplemented. The table below is the original conceptual design;
actual constraints and fields are defined by migrations, and current attendance uses
pending/present/rejected with explicit challenge/manual source. Use UUID primary keys,
UTC timestamps, foreign keys, explicit indexes, and transactional verification.

| Table | Fields / constraints |
|---|---|
| `users` | `id`, `email`/institution ID unique, `password_hash` or auth-provider ID, `role`, `active`, timestamps |
| `courses` | `id`, `code`, `title`, timestamps |
| `course_lecturers` | `course_id`, `lecturer_id`; composite unique |
| `enrollments` | `course_id`, `student_id`, `status`; unique(course, student) |
| `rooms` | `id`, `name`, `kind` (`room`/`theatre`), `capacity`, `profile_version` nullable |
| `attendance_sessions` | `id`, `course_id`, `room_id`, `opened_by`, `starts_at`, `ends_at`, `status`, `policy_version` |
| `acoustic_challenges` | `id`, `session_id`, `token_hash`, `valid_from`, `expires_at`, `generation`, `revoked_at`; no plaintext challenge in student API |
| `submission_nonces` | `id`, `session_id`, `student_id`, `nonce_hash`, `expires_at`, `used_at`; one-use |
| `verification_attempts` | `id`, `session_id`, `student_id`, `created_at`, `decision`, `reason_code`, `policy_version`, `challenge_id`, `device_id` nullable |
| `attendance_records` | `id`, `session_id`, `student_id`, `status`, `verified_at`, `source_attempt_id`, `reviewed_by`; unique(session, student) |
| `device_profiles` | `id`, `user_id`, pseudonymous client ID, platform/browser, audio capability summary; optional |
| `experiments` | `id`, `room_id`, `protocol_version`, `operator_id`, timestamps, consent/protocol metadata |
| `measurements` | `id`, `experiment_id`, `device_profile_id`, `position_label`, `inside_label`, `door_state`, `speaker_mode`, `frequency_profile`, `snr_db`, `decoder_success`, `features_json`, timestamps |
| `resources` | `id`, `course_id`, `uploaded_by`, `title`, `storage_key`, `mime_type`, `size_bytes`, timestamps |
| `audit_events` | `id`, `actor_id`, `action`, `target_type`, `target_id`, `created_at`, minimal metadata |

Consider separate lecturer-approved override records instead of editing verification attempts. Ensure all attendance mutations use transactions and database uniqueness guarantees.

## 7. Suggested API contract

**[RECOMMENDED — version under `/api/v1`]**

| Method/path | Authorization | Function |
|---|---|---|
| `POST /auth/login` | Public + rate limit | Login; secure HttpOnly session cookie |
| `POST /auth/logout` | Authenticated | End session |
| `GET /courses` | Authenticated | List authorized courses |
| `POST /attendance/sessions` | Lecturer | Create session |
| `POST /attendance/sessions/{id}/open` | Authorized lecturer | Activate session |
| `POST /attendance/sessions/{id}/close` | Authorized lecturer | Close session |
| `GET /attendance/sessions/{id}/transmit` | Authorized lecturer only | Fetch active challenge for audio transmission |
| `POST /attendance/sessions/{id}/nonce` | Enrolled student | Create one-use submission nonce |
| `POST /attendance/sessions/{id}/verify` | Enrolled student | Submit decoded challenge + nonce |
| `GET /attendance/sessions/{id}/results` | Authorized lecturer | View outcomes |
| `POST /attendance/sessions/{id}/overrides` | Authorized lecturer | Audited manual decision |
| `POST /experiments/{id}/measurements` | Consent + authorization | Record experimental features |
| `GET /courses/{id}/resources` | Enrolled/authorized | List materials |
| `POST /courses/{id}/resources` | Authorized lecturer | Upload materials |
| `GET /resources/{id}/download` | Authorized | Download material |

Protect state-changing endpoints against CSRF if using cookie sessions. Validate server-side permissions on every request. Use request-size limits and throttling.

## 8. Acoustic prototype specification

**[EXPERIMENTAL — not validated]**

1. **Audible control profile:** e.g., 2.5/3.5 kHz BFSK for initial decoder debugging.
2. **Near-ultrasonic profile:** test approximately 17–19 kHz at conservative volume; do not assume inaudibility or hardware support.
3. **Protocol:** random server challenge (at least 64 bits initially), preamble, version, payload, error-detection checksum, optional error correction. Measure actual frame duration and symbol error rates.
4. **Lecturer:** playback initiated by a user gesture; short controlled bursts; no continuous high-volume output.
5. **Student:** permission-gated microphone; request disabling echo cancellation/noise suppression/AGC where supported; inspect actual settings; decode with Goertzel/FFT-based detection; record confidence and errors.
6. **Server:** validate challenge/nonce freshness, authorization, replay, duplicates. The server cannot trust a browser's claim that audio was heard; client-generated features can be forged.
7. **Research track:** separate room-impulse-response / echo fingerprint prototype; test repeatability before using it in production decisions.
8. **Safety/accessibility:** no forced loud sound; provide a documented alternative for incompatible phones and hearing-sensitive users. Never equate inaudible with harmless.

**Important:** A student may forward a decoded challenge or relay audio in real time. The prototype must document this attack and measure it, not promise cryptographic room membership.

## 9. Room and theatre experiment protocol

**[EXPERIMENTAL]** Collect ground-truth labeled observations at front/middle/back seats, corners, immediately inside doorway, immediately outside doorway, corridors, and adjacent rooms. Repeat with doors open/closed, quiet/noisy rooms, different occupancy, teacher phone locations, speaker/no-speaker, phone orientations and device models.

For theatres, stratify by seating zone and height; a single global acoustic template may be inadequate. For smaller rooms, sample all walls and especially door/window leakage.

Track detection rate, bit/frame error rate, latency, signal-to-noise ratio, false acceptance rate (FAR), false rejection rate (FRR), doorway-specific FAR/FRR, device compatibility, data usage, battery impact, and **live relay attack success**. Split train/test by session and ideally by device to avoid misleading leakage. Report confidence intervals and sample sizes.

Experimental data should be pseudonymized, consent-based, minimized, access-controlled, and deleted under a defined retention policy. Prefer feature vectors over raw audio uploads.

## 10. Slow-internet requirements

**[CONFIRMED]** Low bandwidth is essential. **[RECOMMENDED targets, not measured]:** compressed initial application shell under ~100 KB, normal verification payload under ~5 KB, no routine raw audio upload, no heavyweight JS framework, and responsive behavior under simulated 2G/3G and packet loss. Targets must be measured and adjusted.

Use static asset compression/caching, paginated course lists, compact JSON, conditional requests for resources, and downloadable PDFs. Cache static PWA shell only; never display offline-pending attendance as verified. If a request arrives after challenge expiration, reject or send for manual review according to an explicit policy.

## 11. Known bugs vs anticipated risks

**Confirmed bugs: none currently open in the recorded software tests** — Phase 0/1A/1B/1C code has been inspected and tested; anticipated audio/boundary/relay risks remain unvalidated.

**Anticipated risks to test:**

- Near-ultrasonic audio attenuated by phone speaker/microphone, audio filters, or PA systems.
- 17–19 kHz signal audible to some students; discomfort at inappropriate volume.
- Echo cancellation/noise suppression distorts signals; browser APIs vary by platform.
- Lecturer phone audio playback blocked until user gesture; screen sleep/background throttling.
- Doorway and corridor leakage; room echoes may blur inside/outside distinction.
- Real-time relay, replay, screenshots/QR sharing, fabricated browser measurements.
- GPS indoor inaccuracy/spoofing; BLE browser/device limitations.
- Slow internet causes challenge expiration, repeated submissions, or duplicate requests.
- Permission refusal and low-end phone compatibility.
- Unauthorized resource downloads, CSRF, IDOR, upload abuse, storage exhaustion.
- Incorrect timezone handling and duplicate attendance under concurrency.
- Model overfitting to one phone or one classroom; privacy issues in experimental audio.

Treat each as a hypothesis until reproduced and logged with steps, environment, severity, and test evidence.

## 12. Prioritized implementation plan

### Phase 0 — Inspect and bootstrap
- Check existing repository, git status, language/runtime, README, tests, migrations, and deployment files. **Do not overwrite existing work.**
- Record actual implementation status in this handoff.
- Confirm Go backend decision, supported browsers, and expected student phone mix.
- Create Go server skeleton, config validation, health endpoint, templates/static assets, PostgreSQL migrations, local dev setup, and CI test command.

### Phase 1 — Working classroom platform
- Implement authentication, secure sessions, lecturer/student/admin roles, course enrollment, rooms, lecturer dashboard, student dashboard.
- Add resource upload/list/download with authorization and size/type limits.
- Add attendance session lifecycle, random challenge creation, nonce issuance, transactional verification, audit logs, duplicate prevention, manual review.
- Write unit and integration tests for role boundaries, replay, expiry, duplicates, and concurrency.

### Phase 2 — Acoustic laboratory (early parallel spike)
- Build a standalone HTTPS transmitter/receiver page using an **audible control** signal first.
- Add near-ultrasonic mode with adjustable frequency, conservative volume, and diagnostics.
- Test two real phones across Android/iOS browsers where available; record compatibility and error rates.
- Integrate decoded challenge with `/verify` only after reliable frame decoding.

### Phase 3 — Research instrumentation
- Create experiments and measurements endpoints/schema, consent workflow, device capability logging, and position labels.
- Run inside/doorway/outside experiments in small rooms and theatres.
- Analyze FAR/FRR and relay vulnerability; keep unvalidated classifier outputs in **shadow mode**.

### Phase 4 — PWA, deployment, and pilot
- Cache static assets and permitted resources; implement retry UX without offline attendance approval.
- Deploy HTTPS, logging, backups, migration process, rate limits, secure headers, monitoring.
- Pilot with volunteers; publish measured performance and limitations.

### Later generations (not Web 1.0 commitments)
- **Native 2.0:** BLE GATT, platform-specific ranging where available, native acoustic pipelines, device integrity signals.
- **Spatial 3.0:** measured geometry, material/acoustic profiles, per-room calibrated models.
- **Adaptive 4.0:** guided measurement/enrollment and automatic quality validation for new rooms.

## 13. Immediate Codex task (recommended first prompt)

> Inspect the current workspace before making changes. Treat `CODEX_HANDOFF.md` as a requirements handoff, **not proof that features are implemented**. Summarize the repository's actual state, then propose the smallest safe implementation plan. Unless an existing codebase suggests otherwise, bootstrap a Go modular-monolith backend with PostgreSQL migrations, health endpoint, configuration, server-rendered home page, and tests. Do not implement acoustic attendance verification until the challenge threat model and API contracts are reviewed. After each milestone, run tests and update the handoff's status section with verified evidence.

## 14. Open decisions to resolve

1. Are most student devices Android, iPhone, or mixed? Which browser versions are common?
2. Is installing a lecturer-only companion app permitted in Web 1.0, or strictly browser-only?
3. What hosting budget and expected concurrent attendance volume apply?
4. **Resolved:** administrator-created accounts and email-based enrollment administration are implemented. Institutional identity verification and roster import remain open.
5. What are the policies for failed verification, appeals, privacy consent, and data retention?
6. Are there existing lecture theatre speakers and do they reproduce 17–19 kHz?
7. Will manual lecturer confirmation be allowed for accessibility/device incompatibility?

**Bottom line:** Start with a correct, inexpensive Go-based web platform and a separate acoustic proof of concept. Validate the actual classroom boundary and anti-relay properties experimentally before enforcing them as hard attendance policy.
