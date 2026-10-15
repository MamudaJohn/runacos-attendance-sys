import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

// Exercise the shipped page controller's event handlers and API sequence. Sensor
// and authenticator implementations are mocked here; their own tests exercise
// decoding/GATT/WebAuthn. This does not replace testing a real phone/browser.
const filename = new URL('../internal/web/static/attendance-client.js', import.meta.url);
const original = fs.readFileSync(filename, 'utf8');
const source = original.replace(/^import .*;\r?$/gm, '');
const sessionID = '11111111-1111-1111-1111-111111111111';
const challengeToken = 'a'.repeat(43);
const nonceToken = 'b'.repeat(43);
const evidenceID = '22222222-2222-2222-2222-222222222222';
const proofID = '33333333-3333-3333-3333-333333333333';
const spectral = [.05, .1, .1, .15, .2, .2, .15, .05];

class Element {
  constructor(id = '') {
    this.id = id; this.dataset = {}; this.value = ''; this.textContent = '';
    this.hidden = false; this.disabled = false; this.checked = false;
    this.listeners = new Map(); this.children = []; this.button = null;
  }
  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(listener);
  }
  append(...items) { this.children.push(...items); }
  replaceChildren(...items) { this.children = items; }
  querySelector(selector) { return selector === 'button' ? this.button : null; }
}
async function settle() {
  for (let i = 0; i < 8; i++) await Promise.resolve();
  await new Promise(resolve => setImmediate(resolve));
}
function page({manager = false, identity = true, confirmError, serverRequireIdentity = true, response = {}} = {}) {
  const ids = ['attendance-app', 'capture-status', 'device-support', 'stop-devices'];
  if (manager) ids.push('challenge-display', 'challenge-token', 'ble-payload', 'challenge-qr', 'challenge-expiry',
    'challenge-form', 'transmit', 'play-probe', 'sound-profile', 'signal-gain', 'listen', 'capture-room',
    'capture-gps', 'room-radius', 'room-sample-status', 'save-room', 'load-evidence', 'evidence-summary');
  else ids.push('student-token', 'listen', 'scan-qr', 'connect-ble', 'qr-video', 'checkin-form', 'submission-nonce',
    'capture-room', 'capture-gps', 'clear-gps', 'room-sample-status', 'gps-status', 'evidence-consent', 'submit-attendance');
  if (!manager && identity) ids.push('identity-credentials', 'identity-password', 'register-phone');
  const elements = new Map(ids.map(id => [id, new Element(id)]));
  const root = elements.get('attendance-app');
  root.dataset = {sessionId: sessionID, csrf: 'csrf-from-server', manager: String(manager), identity: String(identity)};
  if (manager) {
    elements.get('sound-profile').value = 'near'; elements.get('signal-gain').value = '.04';
    elements.get('room-radius').value = '50'; elements.get('challenge-display').hidden = true;
  } else {
    elements.get('evidence-consent').checked = true;
    if (identity) elements.get('register-phone').button = new Element();
  }
  const log = [], requests = [], renders = [], transmitterPlays = [], listeners = new Map();
  let qrFrame, acousticFrame, bleFrame, reloaded = 0, gesture = false, scannerStops = 0, microphoneStops = 0;
  const document = {
    hidden: false,
    querySelector(selector) { return selector === '#attendance-app' ? root : null; },
    getElementById(id) { return elements.get(id) || null; },
    createElement() { return new Element(); },
    addEventListener(type, callback) { listeners.set(type, callback); }
  };
  const challenge = {token: challengeToken, expires_at: new Date(Date.now() + 90000).toISOString(),
    qr_image: 'data:image/png;base64,AA==', qr_payload: JSON.stringify({v: 1, session_id: sessionID, token: challengeToken, expires_at: new Date(Date.now() + 90000).toISOString()})};
  const context = {
    document, Date, Promise, console,
    location: {reload() { reloaded++; log.push('reload'); }},
    window: {addEventListener(type, callback) { listeners.set(type, callback); }},
    setInterval() { return 1; }, clearInterval() {},
    CABVSensors: {
      capabilities() { return {secureContext: true, bluetooth: true}; },
      renderQR(data, image) { renders.push({data, image: image.id}); image.hidden = false; },
      async scanQR(video, id, onFrame) { assert.equal(id, sessionID); assert.equal(video.id, 'qr-video'); qrFrame = onFrame; return {stop() { scannerStops++; }}; },
      async captureGPS() { return {latitude: 6.5, longitude: 3.4, accuracy_meters: 50}; },
      async connectBLE(id, onFrame) { assert.equal(id, sessionID); bleFrame = onFrame; return {stop() {}}; }
    },
    createTransmitter() { return {
      async stop() { log.push('transmitter:stop'); },
      async play(token, options) { transmitterPlays.push({token, options: {...options}, gesture}); log.push('transmitter:play'); },
      async playProbe(options) { log.push('probe'); assert.equal(options.profile, 'audible'); }
    }; },
    async startReceiver(options) { acousticFrame = options.onFrame; return {
      async stop() { microphoneStops++; },
      async captureFeatures(seconds, profile) { assert.equal(seconds, 3); assert.equal(profile, 'audible'); return {rms: .3, spectrum: spectral}; }
    }; },
    async identityStatus() { return {credentials: []}; },
    async registerAuthenticator() {}, async revokeAuthenticator() {},
    async confirmAttendance(id, nonce, csrf) {
      log.push('identity:confirm'); assert.equal(id, sessionID); assert.equal(nonce, nonceToken); assert.equal(csrf, 'csrf-from-server');
      if (confirmError) throw new Error(confirmError);
      return {proof_id: proofID};
    },
    async fetch(path, options) {
      const suffix = path.slice(path.lastIndexOf('/') + 1), input = options.body ? JSON.parse(options.body) : undefined;
      requests.push({suffix, method: options.method, input, options}); log.push(`api:${suffix}`);
      let result;
      if (response[suffix]) result = await response[suffix](input, options);
      else if (suffix === 'challenge') result = challenge;
      else if (suffix === 'evidence') result = options.method === 'GET' ? {mode: 'shadow', evidence: []} : {id: evidenceID};
      else if (suffix === 'nonce') result = {nonce: nonceToken};
      else if (suffix === 'verify') result = serverRequireIdentity && !input.identity_proof_id
        ? {status: 'rejected', reason_code: 'identity_required'} : {status: 'pending', reason_code: 'pending_review'};
      else if (suffix === 'calibration') result = {saved: true, mode: 'shadow'};
      else throw new Error(`Unexpected endpoint ${suffix}`);
      return {ok: result.httpError === undefined, async text() {return result.httpError || '';}, async json() {return result;}};
    }
  };
  vm.createContext(context); vm.runInContext(source, context, {filename: filename.pathname});
  return {
    elements, requests, log, renders, transmitterPlays,
    get reloaded() { return reloaded; }, get scannerStops() { return scannerStops; }, get microphoneStops() { return microphoneStops; },
    async fire(id, type = 'click') {
      const element = elements.get(id); assert.ok(element, `element ${id} exists`);
      const event = {currentTarget: element, preventDefault() {this.defaultPrevented = true;}};
      for (const listener of element.listeners.get(type) || []) {
        gesture = true;
        let pending;
        try {pending = listener(event);} finally {gesture = false;}
        await pending;
      }
      await settle();
    },
    qr(token = challengeToken) {assert.ok(qrFrame); qrFrame({token});},
    acoustic(token = challengeToken) {assert.ok(acousticFrame); acousticFrame({token, snrDB: 12});},
    ble(token = challengeToken) {assert.ok(bleFrame); bleFrame({token, ble: {service_uuid: 'cbaf0001-5c28-4d5f-89f7-19b6c1560001', challenge_received: true}});},
    event(type) {listeners.get(type)?.();},
    challenge
  };
}

test('lecturer issues a challenge and renders QR, then plays only from an explicit gesture', async () => {
  const ui = page({manager: true});
  await ui.fire('transmit');
  assert.equal(ui.transmitterPlays.length, 0);
  assert.match(ui.elements.get('capture-status').textContent, /fresh challenge/);
  await ui.fire('challenge-form', 'submit');
  assert.equal(ui.requests.length, 1);
  assert.equal(ui.requests[0].suffix, 'challenge');
  assert.equal(ui.requests[0].options.headers['X-CSRF-Token'], 'csrf-from-server');
  assert.equal(ui.requests[0].options.credentials, 'same-origin');
  assert.equal(ui.requests[0].options.redirect, 'error');
  assert.equal(ui.renders[0].data, ui.challenge.qr_image);
  assert.equal(ui.elements.get('ble-payload').value, ui.challenge.qr_payload);
  assert.equal(ui.transmitterPlays.length, 0);
  await ui.fire('transmit');
  assert.deepEqual(ui.transmitterPlays, [{token: challengeToken, options: {profile: 'near', gain: .04}, gesture: true}]);
});

test('student scans QR, posts evidence and nonce, confirms WebAuthn, then submits and reloads pending result', async () => {
  const ui = page();
  await ui.fire('scan-qr'); ui.qr(); await settle();
  assert.equal(ui.elements.get('student-token').value, challengeToken);
  assert.equal(ui.scannerStops, 1);
  await ui.fire('checkin-form', 'submit');
  assert.deepEqual(ui.log.filter(item => item.startsWith('api:') || item.startsWith('identity:') || item === 'reload'),
    ['api:evidence', 'api:nonce', 'identity:confirm', 'api:verify', 'reload']);
  assert.deepEqual(ui.requests[0].input, {consent: true, capture_method: 'qr'});
  const verify = ui.requests.find(request => request.suffix === 'verify');
  assert.deepEqual(verify.input, {token: challengeToken, nonce: nonceToken, capture_method: 'qr', evidence_id: evidenceID, identity_proof_id: proofID});
  assert.equal(ui.reloaded, 1);
  assert.equal(ui.elements.get('submit-attendance').disabled, false);
});

test('authenticator denial does not call verify and restores the submit button', async () => {
  const ui = page({confirmError: 'Phone identity confirmation was cancelled.'});
  ui.elements.get('student-token').value = challengeToken;
  await ui.fire('checkin-form', 'submit');
  assert.deepEqual(ui.requests.map(request => request.suffix), ['evidence', 'nonce']);
  assert.equal(ui.reloaded, 0);
  assert.equal(ui.elements.get('submit-attendance').disabled, false);
  assert.match(ui.elements.get('capture-status').textContent, /cancelled/);
});

test('server-required identity rejection is respected even if the page dataset disables confirmation', async () => {
  // The real Go tests separately check mandatory identity. This UI test demonstrates
  // that a client flag cannot make a server rejection appear successful.
  const ui = page({identity: false, serverRequireIdentity: true});
  ui.elements.get('student-token').value = challengeToken;
  await ui.fire('checkin-form', 'submit');
  assert.equal(ui.requests.find(request => request.suffix === 'verify').input.identity_proof_id, undefined);
  assert.equal(ui.reloaded, 0);
  assert.match(ui.elements.get('capture-status').textContent, /identity_required/);
});

test('no consent and evidence API failure each stop before a nonce or authenticator call', async () => {
  const noConsent = page(); noConsent.elements.get('student-token').value = challengeToken;
  noConsent.elements.get('evidence-consent').checked = false;
  await noConsent.fire('checkin-form', 'submit');
  assert.equal(noConsent.requests.length, 0);
  const denied = page({response: {evidence() {return {httpError: 'Enrollment withdrawn'};}}});
  denied.elements.get('student-token').value = challengeToken;
  await denied.fire('checkin-form', 'submit');
  assert.deepEqual(denied.requests.map(request => request.suffix), ['evidence']);
  assert.equal(denied.log.includes('identity:confirm'), false);
  assert.equal(denied.reloaded, 0);
});

test('editing a scanned token resets acquisition to manual and removed GPS is not sent', async () => {
  const ui = page(); await ui.fire('scan-qr'); ui.qr(); await settle();
  await ui.fire('capture-gps'); await ui.fire('clear-gps');
  await ui.fire('student-token', 'input');
  await ui.fire('checkin-form', 'submit');
  const input = ui.requests.find(request => request.suffix === 'evidence').input;
  assert.deepEqual(input, {consent: true, capture_method: 'manual'});
});

test('lecturer can save an audible room reference without choosing GPS', async () => {
  const ui = page({manager: true});
  await ui.fire('listen'); await ui.fire('capture-room'); await ui.fire('save-room');
  const input = ui.requests.find(request => request.suffix === 'calibration').input;
  assert.deepEqual(input.spectral_features, spectral);
  assert.equal(input.gps, undefined);
  assert.ok(input.radius_meters === undefined || input.radius_meters === 0, 'room radius must be omitted without a GPS reference');
});

test('a second submit while the evidence request is pending cannot mint another nonce or proof', async () => {
  let finishEvidence;
  const waiting = new Promise(resolve => { finishEvidence = resolve; });
  const ui = page({response: {evidence: () => waiting}});
  ui.elements.get('student-token').value = challengeToken;
  const first = ui.fire('checkin-form', 'submit');
  await settle();
  assert.equal(ui.elements.get('submit-attendance').disabled, true);
  try {
    await ui.fire('checkin-form', 'submit');
    assert.deepEqual(ui.requests.map(request => request.suffix), ['evidence']);
  } finally {finishEvidence({id: evidenceID});}
  await first;
  assert.deepEqual(ui.requests.map(request => request.suffix), ['evidence', 'nonce', 'verify']);
  assert.equal(ui.log.filter(item => item === 'identity:confirm').length, 1);
  assert.equal(ui.reloaded, 1);
});

test('page hiding stops active microphone and scanner; leaving clears capture state', async () => {
  const ui = page(); await ui.fire('listen'); await ui.fire('scan-qr');
  assert.equal(ui.microphoneStops, 1, 'starting QR closes the microphone');
  await ui.fire('capture-gps');
  ui.event('pagehide'); await settle();
  assert.equal(ui.scannerStops, 1);
  ui.elements.get('student-token').value = challengeToken;
  await ui.fire('checkin-form', 'submit');
  assert.equal(ui.requests.find(request => request.suffix === 'evidence').input.gps, undefined);
});
