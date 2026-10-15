import { synthesizeFrame, createStreamingDecoder, generateCalibrationProbe, extractRoomFeatures } from './acoustic-codec.js';

function audioConstructor() {
  const Constructor = globalThis.AudioContext || globalThis.webkitAudioContext;
  if (!Constructor) throw new Error('Web Audio is unavailable in this browser.');
  return Constructor;
}
function receiverRequirements() {
  if (!globalThis.isSecureContext) throw new Error('Microphone reception needs HTTPS (or localhost on this device). An HTTP laptop IP address cannot grant phone microphone access.');
  if (!navigator.mediaDevices?.getUserMedia || !globalThis.AudioWorkletNode) throw new Error('This browser does not support microphone AudioWorklet reception. Use a supported browser or lecturer review.');
}
export function createTransmitter({ onState = () => {} } = {}) {
  let context = null, source = null, ended = null;
  const stop = async () => {
    if (source) { source.onended = null; try { source.stop(); } catch {} source.disconnect(); source = null; }
    if (context) { const closing = context; context = null; await closing.close(); }
    if (ended) { ended(); ended = null; }
    onState({ state: 'stopped' });
  };
  async function playGenerated(generator) {
    // Called directly from a click/gesture. Create/resume before awaiting anything
    // so browsers can authorize playback without an autoplay workaround.
    if (context || source) throw new Error('Stop the current sound before starting another.');
    const Constructor = audioConstructor();
    context = new Constructor();
    try {
      const resuming = context.resume();
      const generated = generator(context.sampleRate);
      const buffer = context.createBuffer(1, generated.pcm.length, context.sampleRate);
      buffer.getChannelData(0).set(generated.pcm); generated.pcm.fill(0);
      source = context.createBufferSource(); source.buffer = buffer; source.connect(context.destination);
      await resuming;
      const completion = new Promise(resolve => { ended = resolve; });
      source.onended = () => { stop().catch(() => {}); };
      source.start(); onState({ state: 'playing', profile: generated.profile, duration: generated.duration, sampleRate: context.sampleRate });
      return { completion, profile: generated.profile, duration: generated.duration, sampleRate: context.sampleRate };
    } catch (error) { await stop(); throw error; }
  }
  return {
    play(token, { profile = 'audible', gain = 0.04 } = {}) { return playGenerated(sampleRate => synthesizeFrame(token, { profile, gain, sampleRate })); },
    playProbe({ profile = 'audible', gain = 0.04, duration = 2 } = {}) { return playGenerated(sampleRate => generateCalibrationProbe({ profile, gain, duration, sampleRate })); },
    stop,
  };
}

export async function startReceiver({ onFrame = () => {}, onState = () => {}, onError = () => {}, profile, autoStopSeconds = 120 } = {}) {
  receiverRequirements();
  if (!Number.isFinite(autoStopSeconds) || autoStopSeconds < 5 || autoStopSeconds > 300) throw new Error('Microphone duration must be 5–300 seconds.');
  let context = null, stream = null, source = null, worklet = null, mute = null, decoder = null;
  let stopped = false, timer = null, capture = null, captureTimer = null;
  const stop = async () => {
    if (stopped) return;
    stopped = true; clearTimeout(timer); clearTimeout(captureTimer);
    if (capture) { const reject = capture.reject; capture.chunks.forEach(c => c.fill(0)); capture = null; reject(new Error('Room capture stopped.')); }
    if (worklet) { worklet.port.onmessage = null; worklet.port.postMessage('stop'); worklet.disconnect(); }
    source?.disconnect(); mute?.disconnect(); decoder?.stop();
    stream?.getTracks().forEach(track => track.stop());
    if (context) await context.close();
    onState({ state: 'stopped' });
  };
  try {
    // This initial resume is deliberately started before the permission await.
    const Constructor = audioConstructor(); context = new Constructor();
    await context.resume();
    stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 1 }, video: false });
    if (stopped) { stream.getTracks().forEach(track => track.stop()); throw new Error('Microphone start cancelled.'); }
    await context.audioWorklet.addModule(new URL('./acoustic-receiver-worklet.js', import.meta.url));
    const track = stream.getAudioTracks()[0];
    const settings = { ...track.getSettings(), contextSampleRate: context.sampleRate };
    decoder = createStreamingDecoder(context.sampleRate, { profiles: profile ? [profile] : undefined, onFrame });
    source = context.createMediaStreamSource(stream);
    worklet = new AudioWorkletNode(context, 'cabv-microphone', { numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1] });
    mute = context.createGain(); mute.gain.value = 0;
    source.connect(worklet); worklet.connect(mute); mute.connect(context.destination);
    worklet.port.onmessage = event => {
      if (stopped || !(event.data instanceof Float32Array)) return;
      const samples = event.data;
      try {
        decoder.push(samples);
        if (capture) { const remaining = capture.target - capture.count; const chunk = samples.slice(0, Math.max(0, remaining)); capture.chunks.push(chunk); capture.count += chunk.length; if (capture.count >= capture.target) finishCapture(); }
      } catch (error) { onError(error); stop().catch(() => {}); }
      finally { samples.fill(0); }
    };
    track.onended = () => { onError(new Error('Microphone permission or device became unavailable.')); stop().catch(() => {}); };
    timer = setTimeout(() => stop().catch(onError), autoStopSeconds * 1000);
    onState({ state: 'listening', settings });
    function finishCapture() {
      if (!capture) return;
      clearTimeout(captureTimer);
      const current = capture; capture = null;
      const pcm = new Float32Array(current.count); let offset = 0;
      for (const chunk of current.chunks) { pcm.set(chunk, offset); offset += chunk.length; chunk.fill(0); }
      const features = extractRoomFeatures(pcm, context.sampleRate, { profile: current.profile }); pcm.fill(0);
      current.resolve(features);
    }
    return {
      stop, settings, sampleRate: context.sampleRate,
      captureFeatures(seconds = 3, captureProfile = profile || 'audible') {
        if (stopped) return Promise.reject(new Error('Start the microphone before recording a room probe.'));
        if (capture) return Promise.reject(new Error('A room capture is already running.'));
        if (!Number.isFinite(seconds) || seconds < 1 || seconds > 5) return Promise.reject(new Error('Room capture must be 1–5 seconds.'));
        return new Promise((resolve, reject) => {
          capture = { target: Math.round(seconds * context.sampleRate), count: 0, chunks: [], resolve, reject, profile: captureProfile };
          captureTimer = setTimeout(() => {
            if (!capture) return;
            capture.chunks.forEach(c => c.fill(0)); capture = null; reject(new Error('No microphone samples received for room capture.'));
          }, (seconds + 2) * 1000);
          onState({ state: 'capturing-room', seconds });
        });
      },
    };
  } catch (error) { await stop(); throw error; }
}
