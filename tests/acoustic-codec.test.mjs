import test from 'node:test';
import assert from 'node:assert/strict';
import { encodeFrame, synthesizeFrame, decodePCM, createStreamingDecoder, generateCalibrationProbe, extractRoomFeatures, crc32 } from '../internal/web/static/acoustic-codec.js';

const token = Buffer.from(Array.from({ length: 32 }, (_, i) => (i * 37 + 11) % 256)).toString('base64url');
function randomGenerator(seed = 42) { return () => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed / 0x100000000; }; }
function noisyOffset(pcm, sampleRate, offsetSeconds = 0.337, noise = 0.01) {
  const out = new Float32Array(Math.round(offsetSeconds * sampleRate) + pcm.length + sampleRate / 4);
  const random = randomGenerator();
  for (let i = 0; i < out.length; i++) out[i] = (random() + random() + random() - 1.5) * noise;
  const offset = Math.round(offsetSeconds * sampleRate);
  for (let i = 0; i < pcm.length; i++) out[offset + i] += pcm[i];
  return out;
}
function timeSkew(pcm, ratio) {
  const out = new Float32Array(Math.ceil(pcm.length * ratio));
  for (let i = 0; i < out.length; i++) { const pos = i / ratio, low = Math.floor(pos), part = pos - low; out[i] = (pcm[low] || 0) * (1 - part) + (pcm[low + 1] || 0) * part; }
  return out;
}
function renderBits(bits, profile, sampleRate = 48000) {
  const pcm = new Float32Array(Math.ceil((bits.length * profile.symbolSeconds + 0.3) * sampleRate));
  for (let b = 0; b < bits.length; b++) {
    const start = Math.round((0.1 + b * profile.symbolSeconds) * sampleRate);
    const end = Math.round((0.1 + (b + 1) * profile.symbolSeconds) * sampleRate);
    const frequency = bits[b] ? profile.oneHz : profile.zeroHz;
    for (let i = start; i < end; i++) pcm[i] = 0.04 * Math.sin(2 * Math.PI * frequency * i / sampleRate);
  }
  return pcm;
}

test('CRC32 known vector and canonical 256-bit challenge encoding', () => {
  assert.equal(crc32(new TextEncoder().encode('123456789')), 0xcbf43926);
  const frame = encodeFrame(token);
  assert.equal(frame.bytes.length, 38);
  assert.equal(frame.bits.length, 352);
  assert.ok(frame.duration < 8);
  assert.throws(() => encodeFrame('not-a-token'));
  assert.throws(() => encodeFrame(token.slice(0, -1) + 'B'));
  assert.throws(() => synthesizeFrame(token, { gain: 0.151 }));
  assert.throws(() => synthesizeFrame(token, { gain: 0 }));
  assert.throws(() => synthesizeFrame(token, { profile: 'near', sampleRate: 32000 }));
});

for (const profile of ['audible', 'near']) {
  for (const sampleRate of [44100, 48000]) {
    test(`${profile} decodes generated PCM at ${sampleRate} Hz after noise and arbitrary offset`, () => {
      const { pcm } = synthesizeFrame(token, { profile, sampleRate });
      const frames = decodePCM(noisyOffset(pcm, sampleRate), sampleRate);
      assert.equal(frames.length, 1);
      assert.equal(frames[0].token, token);
      assert.equal(frames[0].profile, profile);
      assert.equal(frames[0].crcValid, true);
      assert.ok(Number.isFinite(frames[0].snrDB));
      assert.equal(frames[0].features.spectrum.length, 8);
      assert.ok(Math.abs(frames[0].features.spectrum.reduce((a, b) => a + b, 0) - 1) < 0.0001);
    });
  }
  for (const skew of (profile === 'audible' ? [0.99, 0.997, 1.003, 1.01] : [0.997, 1.003])) {
    test(`${profile} decodes a ${((skew - 1) * 100).toFixed(1)}% clock mismatch`, () => {
      const { pcm } = synthesizeFrame(token, { profile });
      const frames = decodePCM(noisyOffset(timeSkew(pcm, skew), 48000, 0.173, 0.003), 48000);
      assert.equal(frames.length, 1);
      assert.equal(frames[0].token, token);
    });
  }
  test(`${profile} tolerates clipped PCM and a delayed echo`, () => {
    const { pcm } = synthesizeFrame(token, { profile });
    const echoDelay = 173;
    const channel = new Float32Array(pcm.length + 4800);
    for (let i = 0; i < pcm.length; i++) channel[i] = Math.max(-1, Math.min(1, 40 * (pcm[i] + 0.25 * (pcm[i - echoDelay] || 0))));
    const frames = decodePCM(channel, 48000);
    assert.equal(frames.length, 1);
    assert.equal(frames[0].token, token);
  });
}

test('near mode fails closed when a 1% resampling shift exceeds its carrier bins', () => {
  const { pcm } = synthesizeFrame(token, { profile: 'near' });
  for (const skew of [0.99, 1.01]) assert.deepEqual(decodePCM(noisyOffset(timeSkew(pcm, skew), 48000, 0.173, 0.003), 48000), []);
});

test('streaming decoder crosses arbitrary chunk boundaries and emits a frame once', () => {
  const { pcm } = synthesizeFrame(token);
  const input = noisyOffset(pcm, 48000, 0.057, 0.002);
  const frames = [];
  const decoder = createStreamingDecoder(48000, { onFrame: frame => frames.push(frame) });
  let index = 0;
  const random = randomGenerator(123);
  while (index < input.length) { const size = 32 + Math.round(random() * 8192); decoder.push(input.subarray(index, index + size)); index += size; }
  decoder.push(new Float32Array(48000));
  assert.equal(frames.length, 1);
  assert.equal(frames[0].token, token);
  decoder.stop(); decoder.push(input);
  assert.equal(frames.length, 1);
});

test('truncated, corrupted CRC, silent, random and wrong-profile inputs do not decode', () => {
  const { pcm } = synthesizeFrame(token);
  assert.deepEqual(decodePCM(pcm.subarray(0, Math.round(pcm.length * 0.65)), 48000), []);
  const frame = encodeFrame(token); frame.bits[frame.bits.length - 7] ^= 1;
  assert.deepEqual(decodePCM(renderBits(frame.bits, frame.profile), 48000), []);
  const random = randomGenerator(666), noise = new Float32Array(48000 * 9);
  for (let i = 0; i < noise.length; i++) noise[i] = (random() - 0.5) * 0.2;
  assert.deepEqual(decodePCM(noise, 48000), []);
  assert.deepEqual(decodePCM(new Float32Array(48000 * 9), 48000), []);
  assert.deepEqual(decodePCM(pcm, 48000, { profiles: ['near'] }), []);
});

test('two successive tokens decode without replaying the first PCM frame', () => {
  const second = Buffer.from(Array.from({ length: 32 }, (_, i) => 255 - i)).toString('base64url');
  const a = synthesizeFrame(token).pcm, b = synthesizeFrame(second).pcm;
  const stream = new Float32Array(a.length + b.length + 24000); stream.set(a); stream.set(b, a.length + 24000);
  assert.deepEqual(decodePCM(stream, 48000).map(v => v.token), [token, second]);
  const frames = [], decoder = createStreamingDecoder(48000, { onFrame: frame => frames.push(frame) });
  decoder.push(stream); decoder.stop();
  assert.deepEqual(frames.map(v => v.token), [token, second]);
  assert.ok(frames.every(frame => frame.features.duration > 6.9), 'large pushes retain each complete frame for local feature extraction');
});

test('room chirp features are normalized bounded vectors without raw PCM', () => {
  for (const profile of ['audible', 'near']) {
    const { pcm, sampleRate, duration } = generateCalibrationProbe({ profile });
    assert.equal(duration, 2);
    assert.ok(pcm.every(value => Math.abs(value) <= 0.040001));
    const features = extractRoomFeatures(pcm, sampleRate, { profile });
    assert.equal(features.spectrum.length, 8);
    assert.ok(features.spectrum.every(v => Number.isFinite(v) && v >= 0 && v <= 1));
    assert.ok(Math.abs(features.spectrum.reduce((a, b) => a + b, 0) - 1) < 0.0001);
    assert.equal(features.decayRatio, 0);
    assert.equal(features.pcm, undefined);
  }
  assert.deepEqual(extractRoomFeatures(new Float32Array(0), 48000).spectrum, Array(8).fill(0));
  assert.throws(() => generateCalibrationProbe({ duration: 30 }));
});
