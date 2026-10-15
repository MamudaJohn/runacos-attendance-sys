import {createTransmitter, startReceiver} from './acoustic-engine.js';
import {registerAuthenticator, confirmAttendance, identityStatus, revokeAuthenticator} from './identity-client.js';

const root = document.querySelector('#attendance-app');
if (root) initialize(root);
function initialize(root) {
  const $ = id => document.getElementById(id);
  const sessionID = root.dataset.sessionId, csrf = root.dataset.csrf;
  const manager = root.dataset.manager === 'true', identity = root.dataset.identity === 'true';
  const sensors = globalThis.CABVSensors;
  let receiver, scanner, peripheral, gps, roomFeatures, challenge, frameMethod = 'manual', ble, submitting = false;
  const say = value => { $('capture-status').textContent = value; };
  const error = e => say(e.message || String(e));
  const tx = createTransmitter({onState: s => { if (s.state === 'playing') say(`Playing ${s.profile} sound for ${s.duration.toFixed(1)} seconds.`); }});
  const profile = () => $('sound-profile')?.value || 'audible';
  const gain = () => Number($('signal-gain')?.value || .04);
  const api = async (suffix, body, method = 'POST') => {
    const response = await fetch(`/api/v1/attendance/sessions/${encodeURIComponent(sessionID)}/${suffix}`, {
      method, credentials: 'same-origin', redirect: 'error',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf}, ...(method === 'POST' ? {body: JSON.stringify(body)} : {})
    });
    if (!response.ok) throw new Error((await response.text()).trim() || 'Request failed.');
    return response.json();
  };
  function on(id, callback) { $(id)?.addEventListener('click', e => { e.preventDefault(); Promise.resolve().then(callback).catch(error); }); }
  async function stop() {
    scanner?.stop(); scanner = null; peripheral?.stop(); peripheral = null;
    const old = receiver; receiver = null;
    await Promise.all([old?.stop(), tx.stop()]);
  }
  function received(frame, method) {
    if (!$('student-token') || submitting) return;
    $('student-token').value = frame.token; frameMethod = method; ble = frame.ble;
    say(`Challenge received via ${method}${frame.snrDB !== undefined ? ` (${frame.snrDB.toFixed(1)} dB estimated signal ratio)` : ''}. Review the optional measurements, then submit.`);
    stop().catch(error);
  }
  async function listen() {
    if (receiver) { say('Microphone is already listening.'); return; }
    receiver = await startReceiver({onFrame: f => received(f, 'acoustic'), onError: error,
      onState: s => {
        if (s.state === 'stopped') receiver = null;
        if (s.state === 'listening') say(`Microphone enabled at ${s.settings.contextSampleRate} Hz (echo cancellation ${s.settings.echoCancellation ?? 'unknown'}, noise suppression ${s.settings.noiseSuppression ?? 'unknown'}). Listening stops after two minutes.`);
      }});
  }
  function setChallenge(value) {
    challenge = value; $('challenge-display').hidden = false;
    $('challenge-token').textContent = value.token;
    $('ble-payload').value = value.qr_payload;
    sensors.renderQR(value.qr_image, $('challenge-qr'));
    say('Fresh challenge ready. Display its QR or press Play challenge sound.');
  }
  const support = sensors.capabilities();
  $('device-support').textContent = support.secureContext
    ? `Secure connection. Bluetooth ${support.bluetooth ? 'available with a compatible peripheral' : 'unavailable in this browser'}. ${identity ? 'Phone confirmation is required for student submissions.' : 'Phone confirmation is disabled in this configuration.'}`
    : 'This address uses HTTP. Phone microphone, camera, location and identity confirmation require trusted HTTPS. Lecturer sound playback and manual review remain available.';
  const saved = $('challenge-display');
  if (saved?.dataset.token) setChallenge({token: saved.dataset.token, expires_at: saved.dataset.expires,
    qr_image: $('challenge-qr').dataset.qr,
    qr_payload: JSON.stringify({v:1, session_id:sessionID, token:saved.dataset.token, expires_at:saved.dataset.expires})});
  $('challenge-form')?.addEventListener('submit', async e => {
    e.preventDefault();
    try { await tx.stop(); setChallenge(await api('challenge', {})); } catch (e) { error(e); }
  });
  $('transmit')?.addEventListener('click', () => {
    if (!challenge || Date.now() >= Date.parse(challenge.expires_at)) { error(new Error('Issue a fresh challenge first.')); return; }
    tx.play(challenge.token, {profile: profile(), gain: gain()}).catch(error);
  });
  $('play-probe')?.addEventListener('click', () => tx.playProbe({profile:'audible', gain:gain(), duration:2}).catch(error));
  on('stop-devices', async () => { await stop(); say('Sound, microphone, camera and Bluetooth stopped.'); });
  on('listen', listen);
  on('scan-qr', async () => {
    await stop(); scanner = await sensors.scanQR($('qr-video'), sessionID, f => received(f, 'qr'), error);
    say('Point the camera at the lecturer’s attendance QR.');
  });
  $('connect-ble')?.addEventListener('click', () => {
    sensors.connectBLE(sessionID, f => received(f, 'ble'), error).then(p => { peripheral = p; }).catch(error);
  });
  on('capture-gps', async () => {
    gps = await sensors.captureGPS();
    const message = `Location selected, reported accuracy ±${Math.round(gps.accuracy_meters)} m. It is sent only when you save or submit.`;
    if ($('gps-status')) $('gps-status').textContent = message; else say(message);
  });
  on('clear-gps', () => { gps = undefined; $('gps-status').textContent = 'GPS removed.'; });
  on('capture-room', async () => {
    if (!receiver) throw new Error('Enable the microphone first. Capture while the lecturer plays the audible room probe.');
    say('Capturing three seconds. Play the separate audible room probe now.');
    const features = await receiver.captureFeatures(3, 'audible');
    if (!features.rms || !features.spectrum?.some(x => x > 0)) throw new Error('No usable sound received. Repeat the room probe.');
    roomFeatures = features.spectrum;
    $('room-sample-status').textContent = 'Eight room measurements captured. No audio will be uploaded.';
  });
  on('save-room', async () => {
    if (!roomFeatures) throw new Error('Capture a room probe before saving the reference.');
    await api('calibration', {consent:true, spectral_features:roomFeatures, ...(gps ? {gps, radius_meters:Number($('room-radius').value)} : {})});
    say('Experimental room reference saved. Student comparisons will be shown to the lecturer.');
  });
  on('load-evidence', async () => {
    const data = await api('evidence', undefined, 'GET'), target = $('evidence-summary'); target.replaceChildren();
    for (const e of data.evidence) {
      const p = document.createElement('p'), c = e.comparison;
      p.textContent = `${e.student_id} · ${e.capture_method} · ${new Date(e.created_at).toLocaleString()}` +
        (c.spectral_cosine_distance !== undefined ? ` · spectral distance ${c.spectral_cosine_distance.toFixed(3)}` : '') +
        (c.gps_distance_meters !== undefined ? ` · GPS distance ${Math.round(c.gps_distance_meters)} m (accuracy ±${Math.round(c.gps_accuracy_meters)} m)` : '') +
        ' · supporting evidence, pending review'; target.append(p);
    }
    if (!data.evidence.length) target.textContent = 'No supporting evidence has been submitted.';
  });
  $('student-token')?.addEventListener('input', () => { frameMethod = 'manual'; ble = undefined; });
  $('checkin-form')?.addEventListener('submit', async e => {
    e.preventDefault(); if (submitting) return;
    if (!$('evidence-consent').checked) { error(new Error('Choose whether to save the captured evidence before submitting.')); return; }
    submitting = true; $('submit-attendance').disabled = true;
    try {
      const token = $('student-token').value.trim();
      if (!/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error('Receive a valid challenge first.');
      const evidence = await api('evidence', {consent:true, capture_method:frameMethod,
        ...(roomFeatures ? {spectral_features:roomFeatures} : {}), ...(gps ? {gps} : {}), ...(ble ? {ble} : {})});
      const n = await api('nonce', {}); $('submission-nonce').value = n.nonce;
      say(identity ? 'Confirm on your phone, then your check-in will be submitted.' : 'Submitting your check-in for lecturer review.');
      const proof = identity ? await confirmAttendance(sessionID, n.nonce, csrf) : null;
      const result = await api('verify', {token, nonce:n.nonce, capture_method:frameMethod, evidence_id:evidence.id,
        ...(proof ? {identity_proof_id:proof.proof_id} : {})});
      await stop();
      if (result.status === 'rejected' && !result.duplicate) throw new Error(`Check-in rejected (${result.reason_code}). Receive a fresh challenge and try again.`);
      location.reload();
    } catch (e) { error(e); } finally { submitting = false; $('submit-attendance').disabled = false; }
  });
  async function showCredentials() {
    if (manager || !identity) return;
    const status = await identityStatus(), target = $('identity-credentials'); target.replaceChildren();
    for (const credential of status.credentials) {
      const p = document.createElement('p'); p.textContent = `Authenticator registered ${new Date(credential.created_at).toLocaleDateString()} `;
      const button = document.createElement('button'); button.type = 'button'; button.textContent = 'Revoke';
      button.addEventListener('click', async () => {
        try { await revokeAuthenticator(credential.id, csrf, $('identity-password').value); $('identity-password').value = ''; await showCredentials(); say('Authenticator revoked.'); } catch (e) { error(e); }
      }); p.append(button); target.append(p);
    }
    if (!status.credentials.length) target.textContent = 'Register an authenticator before checking in.';
  }
  $('register-phone')?.addEventListener('submit', async e => {
    e.preventDefault(); const button = e.currentTarget.querySelector('button'); button.disabled = true;
    try { await registerAuthenticator(csrf, $('identity-password').value); say('Phone authenticator registered.'); await showCredentials(); }
    catch (e) { error(e); } finally { $('identity-password').value = ''; button.disabled = false; }
  });
  showCredentials().catch(error);
  const timer = setInterval(() => {
    if (!challenge) return;
    const seconds = Math.max(0, Math.ceil((Date.parse(challenge.expires_at)-Date.now())/1000));
    $('challenge-expiry').textContent = seconds ? `Challenge expires in ${seconds} seconds.` : 'Challenge expired. Issue a fresh challenge.';
    if (!seconds) { $('challenge-qr').hidden = true; tx.stop().catch(error); challenge = null; }
  }, 1000);
  document.addEventListener('visibilitychange', () => { if (document.hidden) stop().catch(() => {}); });
  window.addEventListener('pagehide', () => { clearInterval(timer); gps = undefined; roomFeatures = undefined; challenge = null; stop().catch(() => {}); });
}
