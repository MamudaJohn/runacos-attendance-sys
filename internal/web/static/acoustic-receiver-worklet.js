// The microphone is never played back. Only bounded PCM batches leave this
// worklet for ephemeral local decoding; browser code never uploads these arrays.
class CABVMicrophone extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = new Float32Array(4096);
    this.used = 0;
    this.active = true;
    this.port.onmessage = event => { if (event.data === 'stop') { this.active = false; this.buffer.fill(0); } };
  }
  process(inputs, outputs) {
    for (const output of outputs) for (const channel of output) channel.fill(0);
    if (!this.active) return false;
    const channels = inputs[0];
    if (!channels?.length) return true;
    const length = channels[0].length;
    for (let i = 0; i < length; i++) {
      let value = 0;
      for (const channel of channels) value += channel[i] || 0;
      this.buffer[this.used++] = value / channels.length;
      if (this.used === this.buffer.length) {
        const completed = this.buffer;
        this.port.postMessage(completed, [completed.buffer]);
        this.buffer = new Float32Array(4096); this.used = 0;
      }
    }
    return true;
  }
}
registerProcessor('cabv-microphone', CABVMicrophone);
