"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");
const source = fs.readFileSync(path.join(__dirname, "../static/sensors.js"), "utf8");
const id = "11111111-1111-1111-1111-111111111111";
const payload = JSON.stringify({v: 1, session_id: id, token: "a".repeat(43), expires_at: "2026-10-14T20:00:00Z"});
function load(extra = {}) {
  const context = {isSecureContext: true, navigator: {}, TextDecoder, setTimeout, clearTimeout, ...extra};
  vm.createContext(context); vm.runInContext(source, context);
  return context;
}
test("QR parser binds the frame to the selected session and bounds untrusted data", () => {
  const sensor = load().CABVSensors;
  assert.equal(sensor.parseQR(payload, id).token, "a".repeat(43));
  assert.throws(() => sensor.parseQR(payload, "other-session"), /another session/);
  assert.throws(() => sensor.parseQR("https://example.org", id), /not a CABV/);
  assert.throws(() => sensor.parseQR("x".repeat(513), id), /Invalid attendance/);
  assert.throws(() => sensor.parseQR(payload.replace('"v":1', '"v":2'), id), /invalid challenge/);
});
test("HTTP on a phone fails explicitly before requesting sensor permissions", async () => {
  let requested = false;
  const sensor = load({isSecureContext: false, navigator: {geolocation: {getCurrentPosition() {requested = true;}}}}).CABVSensors;
  assert.throws(() => sensor.captureGPS(), /trusted HTTPS/);
  await assert.rejects(sensor.connectBLE(id, () => {}), /trusted HTTPS/);
  assert.equal(requested, false);
});
test("GPS collects only one consented bounded sample with a finite timeout", async () => {
  let calls = 0, options;
  const sensor = load({navigator: {geolocation: {getCurrentPosition(ok, fail, opts) {
    calls++; options = opts;
    ok({coords: {latitude: 6.5, longitude: 3.4, accuracy: 42, altitude: 99, speed: 7}, timestamp: 0});
  }}}}).CABVSensors;
  assert.equal(calls, 0);
  const sample = await sensor.captureGPS();
  assert.deepEqual(Object.keys(sample).sort(), ["accuracy_meters", "latitude", "longitude"]);
  assert.equal(sample.accuracy_meters, 42);
  assert.equal(options.timeout, 10000);
  assert.equal(options.maximumAge, 0);
});
test("GPS invalid readings are never returned", async () => {
  const sensor = load({navigator: {geolocation: {getCurrentPosition(ok) {ok({coords: {latitude: NaN, longitude: 3, accuracy: 5}});}}}}).CABVSensors;
  await assert.rejects(sensor.captureGPS(), /invalid/);
});
test("BLE missing hardware API never fabricates a received challenge", async () => {
  const sensor = load().CABVSensors;
  assert.equal(sensor.capabilities().bluetooth, false);
  await assert.rejects(sensor.connectBLE(id, () => {}), /real CABV GATT peripheral/);
});
test("BLE reads the documented real peripheral service, validates and disconnects", async () => {
  let requested, disconnected = false, selectedService, selectedCharacteristic, frame;
  const value = Buffer.from(payload);
  const characteristic = {properties: {read: true}, removeEventListener() {}, async readValue() {return new DataView(value.buffer, value.byteOffset, value.byteLength);}};
  const server = {async getPrimaryService(uuid) {selectedService = uuid; return {async getCharacteristic(uuid) {selectedCharacteristic = uuid; return characteristic;}};}};
  const device = {gatt: {connected: true, async connect() {return server;}, disconnect() {disconnected = true; this.connected = false;}}};
  const sensor = load({navigator: {bluetooth: {async requestDevice(options) {requested = options; return device;}}}}).CABVSensors;
  const operation = await sensor.connectBLE(id, f => {frame = f;});
  assert.equal(requested.filters[0].services[0], sensor.SERVICE_UUID);
  assert.equal(selectedService, sensor.SERVICE_UUID);
  assert.equal(selectedCharacteristic, sensor.CHALLENGE_UUID);
  assert.equal(frame.captureMethod, "ble");
  assert.equal(frame.token, "a".repeat(43));
  assert.equal(frame.ble.challenge_received, true);
  assert.equal(disconnected, true);
  operation.stop();
});

test("QR camera permission resolving after page hiding closes the stream", async () => {
  let closed = false;
  const document = {hidden: true, addEventListener() {}};
  const context = load({document, addEventListener() {}, jsQR() {}, navigator: {mediaDevices: {async getUserMedia() {
    return {getTracks() {return [{stop() {closed = true;}}];}};
  }}}});
  await assert.rejects(context.CABVSensors.scanQR({}, id, () => {}), /page is hidden/);
  assert.equal(closed, true);
});
