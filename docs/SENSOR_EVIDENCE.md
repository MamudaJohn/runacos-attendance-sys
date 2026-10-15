# Optional sensor evidence and classroom experiments

These observations support lecturer review and experiments. They do not classify a
student as inside the room and never mark attendance present. Browser sensor data
can be fabricated, shared or relayed. The server still checks the enrolled account,
current session, expiring challenge and single-use submission nonce.

## QR acquisition

The lecturer-only challenge issuance response includes a PNG QR image and a compact
JSON payload: `{"v":1,"session_id":"…","token":"…","expires_at":"…"}`.
The student scans within the attendance page. Native `BarcodeDetector` is used where
available; the same-origin bundled jsQR 1.4.0 decoder supplies the fallback. The
camera is stopped after one result, 60 seconds, leaving the page or hiding the tab.
QR screenshots can be forwarded; QR alone establishes possession of a challenge.
The PNG is never served through a public or cacheable challenge URL.

## GPS

Location collection requires an explicit user action and a trusted HTTPS context.
The browser requests one fresh fix with a ten-second timeout. Only latitude,
longitude and reported accuracy are accepted. Altitude, movement and device IDs
are not stored. The optional reference location/radius belongs to the session room
and can only be configured by an administrator or a currently assigned lecturer.
The server computes spherical distance and returns both distance and accuracy.
It does not conclude inside/outside: indoor uncertainty and spoofing prevent that.

## Room spectral comparison

Use the same audible calibration chirp and capture duration for the reference and
student samples. Capture on a separate microphone device while the transmitter
plays the probe, then store eight normalized spectral bins. No raw audio is sent
or retained. Only the latest calibration per room is retained. Cosine distance
compares a student vector to that reference in shadow mode. Speaker, microphone,
position, occupants, background noise and gain controls all affect the measurement.
One reference sample does not establish a reliable fingerprint; representative
inside/outside trials and device variation are necessary before any policy change.
Do not compare the two-frequency challenge decoder vector to a chirp reference.

## BLE hardware protocol

The browser implements a central/GATT client for a real, already advertising CABV
peripheral. It cannot advertise a beacon from the lecturer's web page. A compatible
peripheral is additional hardware/firmware, which this web application does not
provide. Unsupported browsers show an explicit capability message.

* Advertised primary service UUID: `cbaf0001-5c28-4d5f-89f7-19b6c1560001`.
* Readable and optionally notifying challenge characteristic:
  `cbaf0002-5c28-4d5f-89f7-19b6c1560001`.
* UTF-8 payload: the same compact QR JSON, capped at 512 bytes. Long characteristic
  reads must return the complete frame; notifications must also contain a complete
  frame. The application does not assemble arbitrary packet fragments.
* The selected peripheral must publish a current lecturer challenge; no secrets,
  account credentials or student identity are written to it.

The client connects only after the student selects a peripheral, validates the
session ID, reads/receives the frame and disconnects. BLE is browser-reported shadow
evidence, without RSSI ranging or proof that a beacon is in the configured room.
Relaying or cloning the peripheral remains possible.

## API and retention

* `POST /api/v1/attendance/sessions/{id}/evidence`: current enrolled student only.
  Requires consent and a capture method (`manual`, `acoustic`, `qr`, `ble`); optional
  spectral/GPS/BLE fields are strictly bounded. Twenty observations per student per
  session limit accidental storage growth.
* `POST /api/v1/attendance/sessions/{id}/calibration`: assigned lecturer/admin only,
  bound to the session room. No arbitrary room identifier is accepted.
* `GET /api/v1/attendance/sessions/{id}/evidence`: a student sees their own summaries;
  assigned lecturers/admins see up to 200 recent session summaries. Precise GPS
  coordinates and sensor payloads are excluded from both responses.
* Evidence linked to a verification attempt must match that account/session/method,
  be less than two minutes old and have no earlier attempt. It is immutable after
  recording. Server database time is authoritative.
* Optional raw observations expire after seven days. `Store.PruneAttendanceEvidence`
  sanitizes them to an empty object, preserving the opaque ID and derived summary
  needed for the attendance audit. Expired observations are hidden immediately by
  the read API. Room reference coordinates remain until the reference is replaced.

Keep GPS optional and obtain any participant consent needed for classroom research.
Phone microphone, camera, location and passkeys need trusted HTTPS; a plain HTTP
hotspot URL is sufficient for basic login/manual attendance only.

The QR camera decoder is vendored from the pinned npm package `jsqr@1.4.0` under
Apache-2.0. Its full license is shipped at `/static/jsQR.LICENSE.txt`. The Go encoder
is `github.com/skip2/go-qrcode`, pinned in `go.mod`, under MIT.
