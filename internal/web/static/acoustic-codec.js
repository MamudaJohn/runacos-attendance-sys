// Experimental CABV BFSK transport. A decoded token is possession evidence, not
// proof of location: audio and decoded tokens can be relayed or fabricated.
export const PROFILES = Object.freeze({
  audible: Object.freeze({ id: 1, zeroHz: 2500, oneHz: 3500, symbolSeconds: 0.020 }),
  near: Object.freeze({ id: 2, zeroHz: 17500, oneHz: 18500, symbolSeconds: 0.020 }),
});
const VERSION = 1;
const PREAMBLE = '101010101010101011100101101001100101110010010011'.split('').map(Number);
const FRAME_BYTES = 38; // version, profile, random 32-byte token, CRC32
const FRAME_BITS = PREAMBLE.length + FRAME_BYTES * 8;
const GUARD_SECONDS = 0.10;
const MAX_GAIN = 0.15;

function resolveProfile(value = 'audible') {
  const p = typeof value === 'string' ? PROFILES[value] : Object.values(PROFILES).find(v => v.id === value);
  if (!p) throw new Error('Unknown acoustic profile.');
  return p;
}
function tokenBytes(token) {
  if (typeof token !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(token)) throw new Error('Challenge must be a 32-byte base64url token.');
  const binary = atob(token.replace(/-/g, '+').replace(/_/g, '/') + '=');
  const bytes = Uint8Array.from(binary, c => c.charCodeAt(0));
  // A canonical encoding avoids accepting unused nonzero padding bits.
  if (bytes.length !== 32 || encodeToken(bytes) !== token) throw new Error('Noncanonical acoustic challenge.');
  return bytes;
}
function encodeToken(bytes) {
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
export function crc32(bytes) {
  let crc = 0xffffffff;
  for (const byte of bytes) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0);
  }
  return (crc ^ 0xffffffff) >>> 0;
}
export function encodeFrame(token, profileId = 'audible') {
  const profile = resolveProfile(profileId);
  const bytes = new Uint8Array(FRAME_BYTES);
  bytes[0] = VERSION;
  bytes[1] = profile.id;
  bytes.set(tokenBytes(token), 2);
  const crc = crc32(bytes.subarray(0, 34));
  new DataView(bytes.buffer).setUint32(34, crc);
  const bits = PREAMBLE.slice();
  for (const byte of bytes) for (let shift = 7; shift >= 0; shift--) bits.push((byte >>> shift) & 1);
  return { bits, profile, bytes, duration: bits.length * profile.symbolSeconds + 2 * GUARD_SECONDS };
}

export function synthesizeFrame(token, { profile = 'audible', sampleRate = 48000, gain = 0.04 } = {}) {
  validateAudio(sampleRate, gain);
  const frame = encodeFrame(token, profile);
  if (frame.profile.oneHz >= sampleRate / 2) throw new Error('Sample rate cannot support this profile.');
  const pcm = new Float32Array(Math.ceil(frame.duration * sampleRate));
  const start = Math.round(GUARD_SECONDS * sampleRate);
  const symbolSamples = frame.profile.symbolSeconds * sampleRate;
  let phase = 0;
  for (let b = 0; b < frame.bits.length; b++) {
    const begin = start + Math.round(b * symbolSamples);
    const end = start + Math.round((b + 1) * symbolSamples);
    const omega = 2 * Math.PI * (frame.bits[b] ? frame.profile.oneHz : frame.profile.zeroHz) / sampleRate;
    const ramp = Math.max(1, Math.round(0.001 * sampleRate));
    for (let i = begin; i < end; i++) {
      const envelope = Math.min(1, (i - begin) / ramp, (end - 1 - i) / ramp);
      pcm[i] = Math.sin(phase) * gain * Math.max(0, envelope);
      phase = (phase + omega) % (2 * Math.PI);
    }
  }
  return { pcm, sampleRate, duration: pcm.length / sampleRate, profile: profileName(frame.profile.id) };
}
function validateAudio(sampleRate, gain) {
  if (!Number.isFinite(sampleRate) || sampleRate < 40000 || sampleRate > 192000) throw new Error('Unsupported audio sample rate.');
  if (!Number.isFinite(gain) || gain <= 0 || gain > MAX_GAIN) throw new Error('Audio gain must be above zero and at most 0.15.');
}
function profileName(id) { return Object.entries(PROFILES).find(([, p]) => p.id === id)?.[0]; }

// Sliding quadrature correlation is equivalent to measuring the Goertzel bins,
// but requires constant work per PCM sample rather than rescanning every window.
class ToneDetector {
  constructor(sampleRate, profile) {
    this.profile = profile;
    this.n = Math.round(sampleRate * 0.014);
    this.hop = Math.max(1, Math.round(sampleRate * 0.001));
    this.sampleRate = sampleRate;
    this.ring = Array.from({ length: 5 }, () => new Float64Array(this.n));
    this.sums = [0, 0, 0, 0, 0];
    this.cos = [1, 1]; this.sin = [0, 0];
    this.rotCos = [profile.zeroHz, profile.oneHz].map(f => Math.cos(2 * Math.PI * f / sampleRate));
    this.rotSin = [profile.zeroHz, profile.oneHz].map(f => Math.sin(2 * Math.PI * f / sampleRate));
    this.seen = 0; this.energies = []; this.base = 0;
  }
  push(samples) {
    for (let k = 0; k < samples.length; k++) {
      const x = Number.isFinite(samples[k]) ? samples[k] : 0;
      const pos = this.seen % this.n;
      for (let tone = 0; tone < 2; tone++) {
        const a = tone * 2, c = this.cos[tone], s = this.sin[tone];
        const re = x * c, im = x * s;
        this.sums[a] += re - this.ring[a][pos]; this.ring[a][pos] = re;
        this.sums[a + 1] += im - this.ring[a + 1][pos]; this.ring[a + 1][pos] = im;
        this.cos[tone] = c * this.rotCos[tone] - s * this.rotSin[tone];
        this.sin[tone] = s * this.rotCos[tone] + c * this.rotSin[tone];
      }
      this.sums[4] += x * x - this.ring[4][pos]; this.ring[4][pos] = x * x;
      this.seen++;
      if (this.seen % 4096 === 0) {
        for (let tone = 0; tone < 2; tone++) { const norm = Math.hypot(this.cos[tone], this.sin[tone]); this.cos[tone] /= norm; this.sin[tone] /= norm; }
      }
      if (this.seen >= this.n && this.seen % this.hop === 0) {
        const p0 = (this.sums[0] ** 2 + this.sums[1] ** 2) / this.n ** 2;
        const p1 = (this.sums[2] ** 2 + this.sums[3] ** 2) / this.n ** 2;
        const dominant = Math.max(p0, p1), total = p0 + p1;
        const ratio = total > 0 ? dominant / total : 0;
        this.energies.push({ time: (this.seen - this.n / 2) / this.sampleRate,
          bit: p1 > p0 ? 1 : 0, valid: dominant > 1e-8 && ratio > 0.70,
          snr: 10 * Math.log10(Math.max(1e-14, 2 * dominant) / Math.max(1e-14, this.sums[4] / this.n - 2 * dominant)), ratio });
      }
    }
  }
  at(time) {
    if (!this.energies.length) return undefined;
    const index = Math.round((time - this.energies[0].time) * this.sampleRate / this.hop);
    return this.energies[index];
  }
  trim(before) {
    const amount = Math.max(0, Math.floor((before - (this.energies[0]?.time || 0)) * this.sampleRate / this.hop));
    if (amount > 1000) this.energies.splice(0, amount);
  }
}

export function createStreamingDecoder(sampleRate, { onFrame = () => {}, profiles = ['audible', 'near'] } = {}) {
  if (!Number.isFinite(sampleRate) || sampleRate < 40000 || sampleRate > 192000) throw new Error('Unsupported decoder sample rate.');
  const detectors = profiles.map(name => new ToneDetector(sampleRate, resolveProfile(name)));
  const states = detectors.map(() => ({ searchedUntil: 0, candidates: [], lastFrameEnd: 0 }));
  let buffered = new Float32Array(0), bufferedStart = 0, totalSamples = 0, stopped = false;
  function scan(detector, state) {
    const latest = detector.energies.at(-1)?.time || 0;
    const symbol = detector.profile.symbolSeconds;
    // Search all sample phases. Preamble tests reject early and candidates are
    // capped; candidates are never treated as frames without a valid CRC/header.
    const limit = latest - (PREAMBLE.length - 1) * symbol * 1.011;
    const begin = Math.max(state.searchedUntil, state.lastFrameEnd, detector.energies[0]?.time || 0);
    for (let time = begin; time <= limit; time += 0.001) {
      let good = 0, bad = 0;
      for (let bit = 0; bit < PREAMBLE.length; bit++) {
        const e = detector.at(time + bit * symbol);
        if (e?.valid && e.bit === PREAMBLE[bit]) good++; else bad++;
        if (bad > 3) break;
      }
      if (good >= PREAMBLE.length - 3) {
        if (!state.candidates.some(c => Math.abs(c.time - time) < 0.0015) && state.candidates.length < 32) state.candidates.push({ time });
      }
    }
    state.searchedUntil = Math.max(begin, limit + 0.001);
    for (const candidate of state.candidates) {
      if (candidate.time < state.lastFrameEnd || latest < candidate.time + (FRAME_BITS - 1) * symbol * 1.011) continue;
      // Symbol timing is searched at 0.05% increments up to +/-1%. Large
      // frequency shifts still exceed narrow tone bins: near mode is tested
      // at +/-0.3%, not claimed to tolerate +/-1% audio resampling.
      for (let drift = 0; drift <= 40; drift++) {
        const offset = drift === 0 ? 0 : (drift % 2 ? 1 : -1) * Math.ceil(drift / 2) * 0.0005;
        const clock = symbol * (1 + offset);
        const bytes = new Uint8Array(FRAME_BYTES); let snr = 0, valid = true;
        for (let bit = 0; bit < FRAME_BYTES * 8; bit++) {
          const e = detector.at(candidate.time + (PREAMBLE.length + bit) * clock);
          if (!e?.valid) { valid = false; break; }
          bytes[bit >>> 3] = (bytes[bit >>> 3] << 1) | e.bit;
          snr += Math.max(-20, Math.min(60, e.snr));
        }
        if (!valid || bytes[0] !== VERSION || bytes[1] !== detector.profile.id || crc32(bytes.subarray(0, 34)) !== new DataView(bytes.buffer).getUint32(34)) continue;
        const frameEnd = candidate.time + FRAME_BITS * clock;
        state.lastFrameEnd = frameEnd;
        const start = Math.max(0, Math.round((candidate.time - symbol / 2 - bufferedStart) * sampleRate));
        const end = Math.min(buffered.length, Math.round((frameEnd - bufferedStart) * sampleRate));
        const result = { token: encodeToken(bytes.subarray(2, 34)), profile: profileName(detector.profile.id),
          snrDB: Math.round(snr / (FRAME_BYTES * 8) * 10) / 10, crcValid: true,
          clockDrift: offset, features: extractRoomFeatures(buffered.subarray(start, end), sampleRate, { profile: profileName(detector.profile.id) }),
          receivedAt: new Date().toISOString() };
        onFrame(result);
        break;
      }
      candidate.done = true;
    }
    state.candidates = state.candidates.filter(c => !c.done && c.time >= state.lastFrameEnd && c.time >= latest - 10);
    detector.trim(latest - 12);
  }
  return {
    push(samples) {
      if (stopped || !samples?.length) return;
      // Bound individual chunks so preamble detection cannot outrun retained PCM.
      for (let start = 0; start < samples.length; start += Math.round(sampleRate * 0.10)) {
        const chunk = samples.subarray(start, start + Math.round(sampleRate * 0.10));
        // Update the bounded local window before each scan, including callers
        // that provide more than twelve seconds in a single push.
        const keep = Math.round(sampleRate * 12);
        const joined = new Float32Array(Math.min(keep, buffered.length + chunk.length));
        const oldKeep = Math.min(buffered.length, Math.max(0, joined.length - chunk.length));
        joined.set(buffered.subarray(buffered.length - oldKeep));
        joined.set(chunk.subarray(Math.max(0, chunk.length - joined.length)), oldKeep);
        totalSamples += chunk.length; buffered = joined; bufferedStart = (totalSamples - buffered.length) / sampleRate;
        detectors.forEach((d, i) => { d.push(chunk); scan(d, states[i]); });
      }
    },
    stop() { stopped = true; buffered = new Float32Array(0); detectors.forEach(d => { d.energies.length = 0; d.ring.forEach(r => r.fill(0)); }); },
  };
}
export function decodePCM(pcm, sampleRate, options = {}) {
  const frames = [];
  const decoder = createStreamingDecoder(sampleRate, { ...options, onFrame: frame => frames.push(frame) });
  for (let i = 0; i < pcm.length; i += 4096) decoder.push(pcm.subarray(i, i + 4096));
  decoder.stop();
  return frames;
}

// A finite, user-initiated chirp for shadow room measurements. No calibrated
// classifier is implied. Device/speaker gain and orientation affect the vector.
export function generateCalibrationProbe({ profile = 'audible', sampleRate = 48000, duration = 2, gain = 0.04 } = {}) {
  validateAudio(sampleRate, gain); const p = resolveProfile(profile);
  if (!Number.isFinite(duration) || duration < 1 || duration > 3) throw new Error('Probe duration must be 1–3 seconds.');
  const pcm = new Float32Array(Math.round(duration * sampleRate));
  const beginHz = p.id === 1 ? 1000 : 17000, endHz = p.id === 1 ? 5000 : 19000;
  if (endHz >= sampleRate / 2) throw new Error('Sample rate cannot support this probe.');
  const activeDuration = duration - 0.4;
  for (let i = 0; i < Math.round(activeDuration * sampleRate); i++) {
    const t = i / sampleRate, envelope = Math.min(1, t / 0.025, (activeDuration - t) / 0.025);
    const phase = 2 * Math.PI * (beginHz * t + (endHz - beginHz) * t * t / (2 * activeDuration));
    pcm[i] = Math.sin(phase) * gain * Math.max(0, envelope);
  }
  return { pcm, sampleRate, duration, profile: profileName(p.id) };
}
export function extractRoomFeatures(pcm, sampleRate, { profile = 'audible' } = {}) {
  const p = resolveProfile(profile);
  const binsHz = p.id === 1 ? [1000, 1500, 2000, 2500, 3000, 3500, 4000, 5000] : [17000, 17250, 17500, 17750, 18000, 18250, 18500, 19000];
  if (!pcm.length) return { version: 1, profile: profileName(p.id), binsHz, spectrum: binsHz.map(() => 0), rms: 0, decayRatio: 0, duration: 0 };
  const windowSize = Math.min(2048, pcm.length), powers = binsHz.map(() => 0);
  let count = 0, square = 0;
  for (const value of pcm) square += value * value;
  for (let start = 0; start + windowSize <= pcm.length; start += windowSize * 4) {
    for (let bin = 0; bin < binsHz.length; bin++) {
      const coeff = 2 * Math.cos(2 * Math.PI * binsHz[bin] / sampleRate); let a = 0, b = 0;
      for (let i = 0; i < windowSize; i++) { const next = pcm[start + i] * (0.5 - 0.5 * Math.cos(2 * Math.PI * i / Math.max(1, windowSize - 1))) + coeff * a - b; b = a; a = next; }
      powers[bin] += Math.max(0, a * a + b * b - coeff * a * b);
    }
    count++;
  }
  const total = powers.reduce((a, b) => a + b, 0);
  const tailSize = Math.min(Math.round(sampleRate * 0.2), Math.floor(pcm.length / 4));
  let tail = 0;
  for (let i = pcm.length - tailSize; i < pcm.length; i++) tail += pcm[i] ** 2;
  return { version: 1, profile: profileName(p.id), binsHz, spectrum: powers.map(v => total > 0 ? Math.round(v / total * 100000) / 100000 : 0),
    rms: Math.round(Math.sqrt(square / pcm.length) * 1000000) / 1000000,
    decayRatio: Math.round((tailSize && square > 0 ? (tail / tailSize) / (square / pcm.length) : 0) * 10000) / 10000,
    duration: Math.round(pcm.length / sampleRate * 1000) / 1000, windows: count };
}
