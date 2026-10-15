# Phone and laptop attendance test

The lecturer uses the laptop and the student uses the phone. This is a local
development test, with both devices on the same Wi-Fi or the laptop connected to
the phone's hotspot. Real phone sensor and classroom trials have not yet been
completed. Automated waveform, server, database and WebAuthn tests do not replace
this checklist.

Phone microphone, camera and passkey access need a trusted secure connection.
Opening the earlier `http://10.218.25.223:8080` address does not provide that.
See [MDN's microphone/camera security requirements](https://developer.mozilla.org/en-US/docs/Web/API/MediaDevices/getUserMedia#security).

## Start local HTTPS

The laptop Wi-Fi address checked for this setup was `10.218.25.223`. The matching
DNS hostname is `cabv.10.218.25.223.sslip.io`. Confirm the address is still current
in Windows Wi-Fi network details; a hotspot reconnection can change it. Substitute
the current address in both variables below if necessary.

From PowerShell in this project, with Go installed and Docker Desktop running:

```powershell
$env:APP_BIND_ADDRESS = '127.0.0.1'
$env:HTTPS_BIND_ADDRESS = '0.0.0.0'
$env:LOCAL_HTTPS_HOST = 'cabv.10.218.25.223.sslip.io'
go run ./cmd/cabv-devcert -host $env:LOCAL_HTTPS_HOST -out .local/tls
docker compose -f compose.yaml -f compose.https.yaml up -d --build --wait
```

The shell variables override Docker Compose's ignored `.env` file. The HTTPS
listener accepts connections from your test network; the internal HTTP endpoint
stays on localhost. For later sessions, these settings can be added to `.env`. Compose
reads this file; the native Go server does not automatically read it.

The [sslip.io DNS service](https://github.com/cunnie/sslip.io) resolves the hostname
to its embedded IP address. It does not host or publish this application. The
phone needs DNS resolution and a network route to the laptop. The test address is:

```text
https://cabv.10.218.25.223.sslip.io:8443/login
```

Use this exact hostname, scheme and port on both devices. The certificate covers
the hostname, and passkeys belong to that hostname. Substituting the bare IP or
`localhost` will not work on the phone. A hostname change requires a new server
certificate and authenticator registration for the new hostname.

The overlay starts `app-https` on port `8443`, sharing the existing PostgreSQL and
resource volumes. It also configures the existing port `8080` app with secure
cookies and the same required identity policy. The HTTP endpoint therefore cannot
provide a challenge-only bypass of the HTTPS policy. Use the HTTPS address for all
browser sign-in and attendance during this test.

Sessions created, opened or given a fresh challenge with identity enabled also
retain that requirement in PostgreSQL. Removing the HTTPS overlay or restarting a
server without identity configuration cannot downgrade those protected sessions.
A lecturer's separately audited manual attendance decision remains available.

## Trust the development certificate manually

Certificate generation creates `.local/tls/root-ca.crt`, `root-ca.key`,
`server.crt` and `server.key`. It does **not** install a certificate or change any
device trust settings. Existing local CA and server keys are preserved on reruns.

1. Transfer only `.local/tls/root-ca.crt` to your test phone, for example over USB.
   Keep both `.key` files on the laptop; never transfer, upload or commit them.
2. On Android, use the device's certificate installation settings to install this
   public certificate as a **CA certificate**. Common settings are Security and
   privacy → More security settings → Encryption and credentials → Install a
   certificate → CA certificate. Manufacturer and Android version labels vary;
   the Wi-Fi certificate option is a different purpose. Confirm the certificate
   identifies `CABV local phone-test CA` and complete the phone's own confirmation.
   [Google's certificate help](https://support.google.com/pixelphone/answer/2844832)
   describes the certificate settings and removal controls.
3. On the laptop, open `root-ca.crt` and manually install it into the current
   user's Trusted Root Certification Authorities store for this development test.
4. Close and reopen Chrome, then open the exact HTTPS address on both devices.
   Continue the sensor test only when the page loads with a valid trusted
   connection and **no certificate warning**. Clicking through a warning does not
   establish the trusted setup required here.

A device or browser that refuses this locally installed CA needs a different
trusted HTTPS setup, such as a real domain and publicly trusted certificate. Do
not disable certificate validation or browser security settings. Remove this
specific development CA from your test devices when it is no longer needed.

## Test the basic phone flow

Use the lecturer and student accounts listed in
[the local demo credentials file](../.local/demo-credentials.txt). The demo password
is also in [the local password file](../.local/demo-password.txt); it is deliberately
not repeated in this guide. These are development accounts.

- [ ] Laptop: sign in as the lecturer at the HTTPS address and open the assigned
  course. Create a new attendance session and open it.
- [ ] Phone: open the exact HTTPS URL in Chrome's address bar, including
  `https://` and `:8443`, then sign in as the student and open the same course and
  session. If Chrome shows a Google search result, the URL was searched instead of
  opened; paste the complete address into the address bar.
- [ ] Phone: under **Your phone authenticator**, enter the current account
  password and register. Complete the phone's fingerprint, face or screen-lock
  prompt. The operating system chooses the verification method; CABV cannot
  insist on fingerprint instead of PIN or receive biometric samples.
- [ ] Laptop: issue a fresh challenge and QR. Start with the **Audible** profile,
  low system speaker volume and the default signal slider value.
- [ ] Phone: tap **Listen for challenge**, allow the microphone, then have the
  laptop play the challenge sound. Check that the received challenge appears.
  The microphone should stop after a successful capture.
- [ ] Phone: choose whether to collect optional measurements, select the evidence
  consent checkbox, and submit. The app requests a fresh one-use nonce, asks for
  phone confirmation, then submits the signed proof and challenge together.
- [ ] Both devices: the check-in is **Pending lecturer review**, not automatically
  present. The lecturer reviews the student and saves a decision with a reason.
  Refresh the phone and confirm the resulting attendance record.
- [ ] In a separate new session, issue a fresh QR and use **Scan lecturer QR** on
  the phone instead of sound. Permit camera access and confirm capture, phone
  confirmation, pending status and lecturer review. A QR screenshot can be shared;
  successful scanning alone does not prove classroom presence.
- [ ] Test **Stop sensors**, hiding the page and leaving it. The camera and
  microphone indicators should turn off. Record device model, Android/Chrome
  versions, selected profile, distance and any errors with each trial.

Required WebAuthn verification proves use of an enrolled credential with user
verification. It does not establish the student's institutional identity or
guarantee that another person does not know the phone PIN. Supervised initial
enrollment and recovery remain institutional decisions. The
[WebAuthn specification](https://www.w3.org/TR/webauthn-3/) describes the signed,
origin-bound credential verification model.

## Try the optional signals separately

- [ ] **Room reference:** use a separate speaker and microphone device. For this
  two-device demo, temporarily sign the phone in as the lecturer, enable its room
  microphone and capture three seconds while the laptop plays the audible room
  calibration probe. Save the reference, then sign the phone out and back in as
  the student. Repeat the same probe for the student's three-second sample,
  keeping speaker position and volume consistent. In classroom trials the
  lecturer should supervise reference capture on their own reference device. Do
  not treat laptop speaker feedback into its own microphone as a classroom baseline.
  Only eight normalized measurements are uploaded. Collect varied devices and
  inside/outside samples before interpreting distances: this is an experimental
  comparison, not a hard room classifier.
- [ ] **GPS:** explicitly include the phone's current location, check the reported
  accuracy, and try removing it before submission. The lecturer may optionally
  save room coordinates and a reference radius. Indoor accuracy and spoofing mean
  distance remains supporting evidence, not an automatic presence decision.
- [ ] **Near-ultrasonic:** after audible capture works, test the candidate
  `17.5/18.5 kHz` profile at low volume. Some speakers and microphones filter it,
  and it may remain audible. Stop if uncomfortable; record failures rather than
  increasing volume aggressively. Do not substitute synthetic decoder results
  for measurements on the actual phone.
- [ ] **BLE:** test only if a real CABV GATT peripheral is available with the
  service, characteristic and current payload specified in
  [the sensor evidence contract](SENSOR_EVIDENCE.md#ble-hardware-protocol).
  The lecturer's browser cannot advertise a beacon. No peripheral has been
  acquired or physically tested, and the handoff's no-hardware-budget constraint
  remains in place. Browser support alone cannot complete this trial.

[Chrome's Web Bluetooth documentation](https://developer.chrome.com/docs/capabilities/bluetooth)
describes its secure-context, user-gesture and central/GATT requirements. Bluetooth
support varies by browser; an unavailable device picker is not an attendance
failure and should use another capture method or an audited lecturer decision.

## Recovery and troubleshooting

Lost-phone recovery is an administrator operation:
`POST /api/v1/admin/users/{student_id}/identity/reset`. It needs the administrator's
logged-in session, exact configured HTTPS Origin, session CSRF header, and JSON
containing the administrator's current `password` and a nonempty `reason`.
Institutionally verify the student before resetting. Reset revokes their
credentials, invalidates outstanding confirmations and records an audit event;
the student must register again. A student can revoke an individual authenticator
from the attendance page after re-entering their password.

For connection failures, first check `docker compose -f compose.yaml -f
compose.https.yaml ps`, the current Wi-Fi IP, DNS resolution of the exact hostname,
and Windows firewall access to TCP `8443` on the test network. Some hotspots or DNS
resolvers block access to local clients or private-address DNS responses. DNS
failure, connection timeout and certificate rejection are separate problems;
changing to plain HTTP will not enable the phone sensors.

For later hosting, a stable real domain with publicly trusted HTTPS will avoid
per-phone development CA installation and hostname changes. Production credentials,
deployment configuration, backup/restore verification and operational monitoring
still need completion before that deployment is ready for real student data.
