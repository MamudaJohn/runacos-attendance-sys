# Attendance contract — experimental audio and phone identity

This contract describes the implemented attendance workflow, extended by the experimental
acoustic, QR, sensor and WebAuthn milestone. A valid token demonstrates possession of
a challenge; it does not establish that a student is inside a classroom. Real-time sharing
and audio relay remain possible. No automatic outcome is called verified physical presence.

## Identity, permissions and lifecycle

- Cookie authentication and current database roles apply to every request.
- An active assigned lecturer or administrator manages a course's attendance sessions,
  challenge issuance, results and manual review. Another lecturer cannot manage that course.
- Only an active, enrolled student can obtain a nonce, submit, or see their own outcome.
  A student never receives a course roster, someone else's result, or an issued challenge.
- A session is created as `draft`, with title, room and duration (1–180 minutes).
  Opening starts the database-clock window. A session opens once and cannot reopen.
- Closing permanently disables submissions and revokes challenges/unused nonces.
  An open session past its end time is effectively expired even if its stored status is open.
  Manual review is permitted after closing/expiry, but not for an unopened draft.
- Current enrollment is required for manual review. Withdrawal prevents new student
  access/submissions; staff results retain recorded attendance/attempt/review history,
  including withdrawn or disabled accounts, and mark those rows unavailable for review.

## Challenges, nonces and outcomes

Challenge issuance is an explicit manager-only POST. It generates a cryptographically
random 256-bit token, revokes earlier challenges, and returns plaintext once to that
manager. The database stores only its SHA-256 digest. Tokens expire after 90 seconds or
session end, whichever comes first. There is no GET that retrieves an existing token.
Browser audio and QR can acquire this token. BLE can receive the same session-scoped
payload from a compatible external GATT peripheral; browsers cannot advertise it.

A student obtains a separate 256-bit nonce bound to their identity and session. It expires
after two minutes or session end. Only a digest is stored. Issuing a new nonce invalidates
the student's older unused nonce. At most 50 nonces and 20 verification attempts per
student/session are allowed; challenge generations cap at 1,000.

Verification uses current authorization, session window, current challenge freshness,
and nonce ownership/freshness/one-use state. An owned fresh nonce is consumed on an attempt,
including a wrong challenge or closed/expired window. Another student's nonce cannot be consumed. A replay,
expired/revoked challenge, closed/expired session, or invalid nonce is rejected with a
reason code. Authorized attempts and audit events are transactional and contain no
plaintext token or nonce. Once the attempt cap is reached, further calls cannot grow
attempt/audit history indefinitely.

When `PUBLIC_ORIGIN` and `WEBAUTHN_RP_ID` are configured, every submission requires
a signed WebAuthn assertion with required user verification. Creating, opening or
issuing a challenge through that configuration permanently sets the session's
`identity_required` flag; later configuration changes or older clients cannot disable
it. The exact expected origin, RP, challenge, credential owner, signature, presence,
UV flag and counter are verified with go-webauthn. Registration requires password
re-entry. One-use ceremonies expire after five minutes and bind the browser login
session; attendance assertions additionally bind the student/session/nonce. Two-minute
one-use proof IDs are consumed in the same transaction as attendance and must match
the current RP. Revocation invalidates outstanding proofs. Students can revoke their
credentials; administrator reset requires a password and an audited reason.

User verification may use fingerprint, face or a device PIN. No biometric samples
or authenticator private keys are collected. Credential enrollment after password
confirmation establishes an enrolled key holder, not institutionally verified identity
or ownership of a particular handset; passkeys may be synced. Supervised enrollment
and recovery are still institutional policy decisions.

Optional evidence IDs must belong to the student, session and capture method, be
less than two minutes old and have never been linked to an attempt. A capture method
is a browser claim, not a server-attested sensor. Spectral/GPS comparisons are shadow
measurements without an inside/outside decision and never mark attendance present.
Optional observation data expires after seven days and is sanitized at startup/hourly;
comparison/audit references remain. Calibration references persist until replaced.

An accepted challenge creates a single `pending` record with reason `pending_review`.
Initial rejected attempts remain in attempt history and do not create approved attendance.
Further fresh valid submissions return the existing record as a duplicate. Rejected/replayed
submissions do not overwrite a prior accepted or manually reviewed record. A lecturer
can mark a current enrolled student `present` or `rejected`, with a required 1–500-byte
reason. That decision has `manual` evidence source and attributed immutable review/audit
history. It never changes or erases prior verification attempts.

Session, account, course permission and nonce/challenge locks plus database uniqueness
prevent two concurrent submissions from producing duplicate attendance records. Time is
checked using the database clock after waiting for transaction locks. Closing, rotation,
review and verification serialize consistently. Revocation does not cancel an already
completed request; normal HTTP authentication is checked at request start.

## HTTP endpoints

HTML forms use the paths below without `/api/v1`, except the course-scoped list/form.
JSON API mutations require cookie authentication, same-origin browser metadata,
`Content-Type: application/json`, `X-CSRF-Token` bound to the login session, a body under
4 KiB, and only the documented fields. Form mutations use the existing 8 KiB form cap
and hidden CSRF token. Responses are private/no-store. Tokens never appear in URLs.

Verify accepts `token`, `nonce`, optional `capture_method`, `evidence_id`, and
`identity_proof_id`; identity enforcement is server/database policy. No client policy
flag is accepted. Missing/invalid identity gives `identity_required`/`identity_rejected`;
foreign, stale or reused evidence gives `evidence_invalid`, preserving prior records.
Identity ceremony bodies have a 20 KiB envelope cap and a 16 KiB credential cap.
The sensor endpoints and GATT protocol are described in [SENSOR_EVIDENCE.md](SENSOR_EVIDENCE.md).

| Method/path | Fields | Access / response |
|---|---|---|
| POST /api/v1/attendance/sessions | course_id, room_id, title, duration_minutes | Manager; draft session metadata |
| GET /api/v1/attendance/sessions/{id} | — | Authorized course member; safe metadata only |
| POST /api/v1/attendance/sessions/{id}/open | — | Manager; opened metadata |
| POST /api/v1/attendance/sessions/{id}/close | — | Manager; closed metadata |
| POST /api/v1/attendance/sessions/{id}/challenge | — | Manager; token, generation, expires_at |
| POST /api/v1/attendance/sessions/{id}/nonce | — | Student; nonce, expires_at |
| POST /api/v1/attendance/sessions/{id}/verify | token, nonce | Student; status, reason_code, duplicate |
| GET /api/v1/attendance/sessions/{id}/results | — | Manager-only results |
| GET /api/v1/attendance/sessions/{id}/mine | — | Student's own result |
| POST /api/v1/attendance/sessions/{id}/reviews | email, decision, reason | Manager; manual present/rejected decision |

Missing or unauthorized objects return 404; invalid fields 400; lifecycle conflicts 409;
bounded attempt/nonce/generation exhaustion 429; database outages 503. Valid-shaped
verification requests return a decision (`pending`, `rejected`, or an existing manually
reviewed status with `duplicate=true`) rather than hiding a
rejection behind a success redirect. HTML shows the same decision and its reason.

## Required verification

Test permission boundaries, hidden token/roster leakage, inactive/withdrawn accounts,
malformed input and CSRF, expired/revoked/wrong challenges, nonce ownership/expiry/replay,
duplicates and concurrent submissions, close/rotation races, manual-review attribution,
and transaction rollback on audit failure. Smoke-test the deployed Docker stack with
demonstration accounts. Real-phone acoustic compatibility, classroom boundary behavior,
FAR/FRR, and relay attack measurements remain later experimental gates.
