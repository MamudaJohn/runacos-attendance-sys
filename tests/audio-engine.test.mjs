import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFile } from 'node:fs/promises';
import { createTransmitter, startReceiver } from '../internal/web/static/acoustic-engine.js';
import { synthesizeFrame, generateCalibrationProbe } from '../internal/web/static/acoustic-codec.js';

const token = Buffer.from(Array.from({ length: 32 }, (_, i) => i)).toString('base64url');
function setup({ secure = true, denied = false } = {}) {
  const previous = new Map();
  const contexts = [], worklets = [], sources = [], gains = [], constraints = [];
  const track = { stopped: false, stop() { this.stopped = true; }, getSettings() { return { sampleRate: 48000, echoCancellation: true, noiseSuppression: false }; } };
  function set(name, value) { previous.set(name, Object.getOwnPropertyDescriptor(globalThis, name)); Object.defineProperty(globalThis, name, { value, writable: true, configurable: true }); }
  function node(kind) { return { kind, connected: [], disconnected: false, connect(to) { this.connected.push(to); return to; }, disconnect() { this.disconnected = true; } }; }
  class Context {
    constructor() { this.sampleRate = 48000; this.destination = node('destination'); this.closed = false; this.resumed = false; this.audioWorklet = { addModule: async url => { this.module = String(url); } }; contexts.push(this); }
    async resume() { this.resumed = true; }
    async close() { this.closed = true; }
    createBuffer(channels, count) { const samples = new Float32Array(count); return { getChannelData: () => samples }; }
    createBufferSource() { const source = { ...node('sound'), start() { this.started = true; }, stop() { this.stopped = true; } }; sources.push(source); return source; }
    createMediaStreamSource() { const source = node('microphone'); sources.push(source); return source; }
    createGain() { const gain = { ...node('gain'), gain: { value: 1 } }; gains.push(gain); return gain; }
  }
  class Worklet { constructor() { Object.assign(this, node('worklet')); this.port = { onmessage: null, sent: [], postMessage(value) { this.sent.push(value); } }; worklets.push(this); } }
  set('AudioContext', Context); set('AudioWorkletNode', Worklet); set('isSecureContext', secure);
  set('navigator', { mediaDevices: { getUserMedia: async opts => { constraints.push(opts); if (denied) throw new Error('Permission denied'); return { getAudioTracks: () => [track], getTracks: () => [track] }; } } });
  return { contexts, sources, worklets, gains, track, constraints, restore() { for (const [name, descriptor] of previous) { if (descriptor) Object.defineProperty(globalThis, name, descriptor); else delete globalThis[name]; } } };
}

test('transmitter uses conservative PCM, no autoplay, and closes its audio context', async () => {
  const mock = setup();
  try {
    const states = [], tx = createTransmitter({ onState: s => states.push(s.state) });
    assert.equal(mock.contexts.length, 0);
    const played = await tx.play(token);
    assert.equal(played.duration, 7.24);
    assert.ok(mock.sources[0].buffer.getChannelData(0).every(v => Math.abs(v) <= 0.040001));
    assert.equal(mock.sources[0].started, true);
    await assert.rejects(tx.play(token), /Stop/);
    await tx.stop(); await played.completion;
    assert.equal(mock.contexts[0].closed, true);
    assert.equal(mock.sources[0].stopped, true);
    assert.deepEqual(states, ['playing', 'stopped']);
    await assert.rejects(tx.play(token, { gain: 0.5 }), /gain/);
    assert.equal(mock.contexts[1].closed, true);
  } finally { mock.restore(); }
});

test('receiver refuses insecure phone HTTP and frees context on microphone denial', async () => {
  let mock = setup({ secure: false });
  try { await assert.rejects(startReceiver(), /HTTPS/); assert.equal(mock.contexts.length, 0); } finally { mock.restore(); }
  mock = setup({ denied: true });
  try { await assert.rejects(startReceiver(), /Permission denied/); assert.equal(mock.contexts[0].closed, true); } finally { mock.restore(); }
});

test('receiver reports actual processing settings, mutes microphone graph, decodes and cleans up', async () => {
  const mock = setup();
  try {
    const frames = [], receiver = await startReceiver({ onFrame: v => frames.push(v), profile: 'audible' });
    assert.equal(mock.constraints[0].audio.echoCancellation, false);
    assert.equal(mock.constraints[0].audio.noiseSuppression, false);
    assert.equal(mock.constraints[0].audio.autoGainControl, false);
    assert.equal(receiver.settings.echoCancellation, true); // Device did not honor requested processing.
    assert.equal(mock.gains[0].gain.value, 0);
    assert.equal(mock.sources[0].connected[0], mock.worklets[0]);
    assert.equal(mock.worklets[0].connected[0], mock.gains[0]);
    const input = synthesizeFrame(token).pcm;
    for (let i = 0; i < input.length; i += 4096) mock.worklets[0].port.onmessage({ data: input.slice(i, i + 4096) });
    assert.equal(frames.length, 1);
    assert.equal(frames[0].token, token);
    await receiver.stop(); await receiver.stop();
    assert.equal(mock.track.stopped, true);
    assert.equal(mock.contexts[0].closed, true);
    assert.equal(mock.worklets[0].port.onmessage, null);
    assert.deepEqual(mock.worklets[0].port.sent, ['stop']);
  } finally { mock.restore(); }
});

test('room probe capture returns eight normalized features and discards received PCM', async () => {
  const mock = setup();
  try {
    const receiver = await startReceiver({ profile: 'audible' });
    const captured = receiver.captureFeatures(2);
    const input = generateCalibrationProbe().pcm;
    for (let i = 0; i < input.length; i += 4096) {
      const chunk = input.slice(i, i + 4096); mock.worklets[0].port.onmessage({ data: chunk });
      assert.ok(chunk.every(v => v === 0));
    }
    const features = await captured;
    assert.equal(features.spectrum.length, 8);
    assert.ok(Math.abs(features.spectrum.reduce((a, b) => a + b, 0) - 1) < 0.0001);
    assert.equal(features.pcm, undefined);
    const incomplete = receiver.captureFeatures(3); const rejection = assert.rejects(incomplete, /stopped/);
    await receiver.stop(); await rejection;
  } finally { mock.restore(); }
});

test('AudioWorklet always emits silence and stops processing after stop message', async () => {
  const source = await readFile(new URL('../internal/web/static/acoustic-receiver-worklet.js', import.meta.url), 'utf8');
  let Processor;
  class Base { constructor() { this.port = { sent: [], postMessage(samples) { this.sent.push(samples); } }; } }
  vm.runInNewContext(source, { Float32Array, AudioWorkletProcessor: Base, registerProcessor: (name, Constructor) => { assert.equal(name, 'cabv-microphone'); Processor = Constructor; } });
  const worklet = new Processor(), microphone = new Float32Array(128).fill(0.5), output = new Float32Array(128).fill(1);
  for (let i = 0; i < 32; i++) assert.equal(worklet.process([[microphone]], [[output]]), true);
  assert.ok(output.every(value => value === 0));
  assert.equal(worklet.port.sent.length, 1);
  assert.equal(worklet.port.sent[0].length, 4096);
  worklet.port.onmessage({ data: 'stop' });
  assert.equal(worklet.process([[microphone]], [[output]]), false);
});
