/* CABV optional sensor helpers. Captures are consent-based, transient and untrusted.
 * jsQR 1.4.0 is vendored separately under its Apache-2.0 license and loaded on demand.
 */
(function (global) {
  "use strict";
  const SERVICE_UUID = "cbaf0001-5c28-4d5f-89f7-19b6c1560001";
  const CHALLENGE_UUID = "cbaf0002-5c28-4d5f-89f7-19b6c1560001";
  const activeStops = new Set();
  function requireSecure() {
    if (!global.isSecureContext) throw new Error("Phone sensors require a trusted HTTPS address. HTTP hotspot addresses cannot use the camera, microphone, GPS or passkeys.");
  }
  function capabilities() {
    const nav = global.navigator || {};
    return Object.freeze({secureContext: !!global.isSecureContext,
      camera: !!(nav.mediaDevices && nav.mediaDevices.getUserMedia),
      gps: !!nav.geolocation, bluetooth: !!(nav.bluetooth && nav.bluetooth.requestDevice),
      nativeQR: typeof global.BarcodeDetector === "function"});
  }
  function parseQR(raw, sessionID) {
    if (typeof raw !== "string" || raw.length > 512) throw new Error("Invalid attendance QR payload.");
    let frame;
    try { frame = JSON.parse(raw); } catch (_) { throw new Error("This QR is not a CABV attendance challenge."); }
    if (!frame || frame.v !== 1 || frame.session_id !== sessionID || !/^[A-Za-z0-9_-]{43}$/.test(frame.token) ||
        typeof frame.expires_at !== "string" || !Number.isFinite(Date.parse(frame.expires_at))) {
      throw new Error("QR belongs to another session or contains an invalid challenge.");
    }
    // Server database time, token expiry and nonce checks remain authoritative.
    return {token: frame.token, sessionID: frame.session_id, expiresAt: frame.expires_at, captureMethod: "qr"};
  }
  function renderQR(dataURL, image) {
    if (!image || typeof dataURL !== "string" || !dataURL.startsWith("data:image/png;base64,")) throw new Error("Invalid QR image.");
    image.src = dataURL;
    image.alt = "Attendance challenge QR code; scan on your student phone";
    image.hidden = false;
  }
  function captureGPS() {
    requireSecure();
    if (!global.navigator.geolocation) return Promise.reject(new Error("Location is unavailable in this browser."));
    return new Promise((resolve, reject) => {
      global.navigator.geolocation.getCurrentPosition(position => {
        const c = position.coords;
        const data = {latitude: c.latitude, longitude: c.longitude, accuracy_meters: c.accuracy};
        if (!Object.values(data).every(Number.isFinite) || Math.abs(data.latitude) > 90 || Math.abs(data.longitude) > 180 || data.accuracy_meters < 0 || data.accuracy_meters > 100000) {
          reject(new Error("Location readings were invalid; no location was sent.")); return;
        }
        resolve(data);
      }, error => reject(new Error(error.code === 1 ? "Location permission was denied; GPS was skipped." : "Location could not be measured; GPS was skipped.")),
      {enableHighAccuracy: true, maximumAge: 0, timeout: 10000});
    });
  }
  let qrLibraryPromise;
  function loadQRDecoder() {
    if (global.jsQR) return Promise.resolve(global.jsQR);
    if (!qrLibraryPromise) qrLibraryPromise = new Promise((resolve, reject) => {
      const script = global.document.createElement("script");
      script.src = "/static/jsQR.js";
      script.onload = () => global.jsQR ? resolve(global.jsQR) : reject(new Error("QR decoder did not load."));
      script.onerror = () => {qrLibraryPromise = undefined; reject(new Error("QR decoder could not load."));};
      global.document.head.append(script);
    });
    return qrLibraryPromise;
  }
  // onFrame receives only a validated challenge. Caller explicitly requests a
  // server nonce and submits it; scanning never marks attendance present.
  async function scanQR(video, sessionID, onFrame, onError) {
    requireSecure();
    if (!global.navigator.mediaDevices || !global.navigator.mediaDevices.getUserMedia) throw new Error("Camera access is unavailable.");
    if (!video || typeof onFrame !== "function") throw new Error("Missing camera preview or challenge handler.");
    let detector = null;
    if (typeof global.BarcodeDetector === "function") {
      try {
        const supported = await global.BarcodeDetector.getSupportedFormats();
        if (supported.includes("qr_code")) detector = new global.BarcodeDetector({formats: ["qr_code"]});
      } catch (_) { /* Use the locally bundled decoder. */ }
    }
    const decode = detector ? null : await loadQRDecoder();
    const stream = await global.navigator.mediaDevices.getUserMedia({video: {facingMode: {ideal: "environment"}, width: {ideal: 640}, height: {ideal: 480}}, audio: false});
    if (global.document.hidden) {
      stream.getTracks().forEach(track => track.stop());
      throw new Error("Camera stopped because the attendance page is hidden.");
    }
    let stopped = false, timer, deadline;
    const canvas = global.document.createElement("canvas");
    const context = canvas.getContext("2d", {willReadFrequently: true});
    const stop = () => {
      if (stopped) return;
      stopped = true; clearTimeout(timer); clearTimeout(deadline);
      stream.getTracks().forEach(track => track.stop()); video.srcObject = null; video.hidden = true;
      activeStops.delete(stop);
    };
    activeStops.add(stop);
    video.srcObject = stream; video.playsInline = true; video.muted = true; video.hidden = false;
    try { await video.play(); } catch (error) { stop(); throw error; }
    async function tick() {
      if (stopped) return;
      try {
        let raw = null;
        if (video.readyState >= 2 && video.videoWidth && video.videoHeight) {
          if (detector) { const found = await detector.detect(video); if (found.length) raw = found[0].rawValue; }
          else {
            canvas.width = Math.min(video.videoWidth, 640);
            canvas.height = Math.round(video.videoHeight * canvas.width / video.videoWidth);
            context.drawImage(video, 0, 0, canvas.width, canvas.height);
            const pixels = context.getImageData(0, 0, canvas.width, canvas.height);
            const found = decode(pixels.data, pixels.width, pixels.height, {inversionAttempts: "dontInvert"});
            if (found) raw = found.data;
          }
        }
        if (stopped) return;
        if (raw) {
          let frame;
          try { frame = parseQR(raw, sessionID); } catch (error) { if (onError) onError(error); }
          if (frame) { stop(); onFrame(frame); return; }
        }
      } catch (error) { stop(); if (onError) onError(error); return; }
      if (!stopped) timer = setTimeout(tick, 180);
    }
    deadline = setTimeout(() => {stop(); if (onError) onError(new Error("QR scan timed out. Start again when the lecturer displays a fresh challenge."));}, 60000);
    tick();
    return {stop};
  }
  async function connectBLE(sessionID, onFrame, onError) {
    requireSecure();
    if (!global.navigator.bluetooth || !global.navigator.bluetooth.requestDevice) throw new Error("This browser does not support Web Bluetooth. BLE requires a supported browser and a real CABV GATT peripheral.");
    // requestDevice must run directly from the student's button gesture.
    const device = await global.navigator.bluetooth.requestDevice({filters: [{services: [SERVICE_UUID]}]});
    if (global.document && global.document.hidden) throw new Error("Bluetooth stopped because the attendance page is hidden.");
    let characteristic, stopped = false, deadline;
    const stop = () => {
      if (stopped) return;
      stopped = true; clearTimeout(deadline); activeStops.delete(stop);
      if (characteristic) characteristic.removeEventListener("characteristicvaluechanged", changed);
      if (device.gatt && device.gatt.connected) device.gatt.disconnect();
    };
    const accept = value => {
      if (stopped || !value || value.byteLength > 512) return;
      try {
        const raw = new TextDecoder("utf-8", {fatal: true}).decode(value);
        const frame = parseQR(raw, sessionID); frame.captureMethod = "ble";
        frame.ble = {service_uuid: SERVICE_UUID, challenge_received: true};
        stop(); onFrame(frame);
      } catch (error) { if (onError) onError(error); }
    };
    function changed(event) {accept(event.target.value);}
    activeStops.add(stop);
    try {
      const server = await device.gatt.connect();
      const service = await server.getPrimaryService(SERVICE_UUID);
      characteristic = await service.getCharacteristic(CHALLENGE_UUID);
      if (characteristic.properties.notify || characteristic.properties.indicate) {
        characteristic.addEventListener("characteristicvaluechanged", changed);
        await characteristic.startNotifications();
      }
      if (characteristic.properties.read) accept(await characteristic.readValue());
      if (!stopped && !characteristic.properties.notify && !characteristic.properties.indicate) throw new Error("Peripheral does not provide readable CABV challenge data.");
      if (!stopped) deadline = setTimeout(() => {stop(); if (onError) onError(new Error("BLE challenge timed out."));}, 30000);
      return {stop};
    } catch (error) {stop(); throw error;}
  }
  if (global.document) {
    global.document.addEventListener("visibilitychange", () => {if (global.document.hidden) [...activeStops].forEach(stop => stop());});
    global.addEventListener("pagehide", () => [...activeStops].forEach(stop => stop()));
  }
  global.CABVSensors = Object.freeze({SERVICE_UUID, CHALLENGE_UUID, capabilities, parseQR, renderQR, captureGPS, scanQR, connectBLE});
})(typeof window !== "undefined" ? window : globalThis);
