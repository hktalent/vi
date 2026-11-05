// 车牌链路：YOLOv5-face 车牌检测 (640) + CRNN CTC 字符识别 (48x160)
const DET_SIZE = 640;
const REC_H = 48;
const REC_W = 160;

function nmsBoxes(boxes, thresh) {
  const sorted = boxes.slice().sort((a, b) => b.score - a.score);
  const keep = [];
  for (const b of sorted) {
    let ok = true;
    for (const k of keep) {
      if (iouRect(b, k) > thresh) { ok = false; break; }
    }
    if (ok) keep.push(b);
  }
  return keep;
}

function iouRect(a, b) {
  const x1 = Math.max(a.x, b.x), y1 = Math.max(a.y, b.y);
  const x2 = Math.min(a.x + a.w, b.x + b.w), y2 = Math.min(a.y + a.h, b.y + b.h);
  const inter = Math.max(0, x2 - x1) * Math.max(0, y2 - y1);
  const union = a.w * a.h + b.w * b.h - inter;
  return union <= 0 ? 0 : inter / union;
}

class PlateEngine {
  constructor() {
    this.det = null;
    this.rec = null;
    this.chars = [];
    this.detInput = 'input';
    this.recInput = 'data';
    this.canvas = new OffscreenCanvas(DET_SIZE, DET_SIZE);
    this.ctx = this.canvas.getContext('2d', { willReadFrequently: true });
    this.ready = false;
  }

  async init(onStep) {
    ort.env.wasm.wasmPaths = '/vendor/ort/';
    ort.env.wasm.numThreads = self.crossOriginIsolated
      ? Math.min(4, navigator.hardwareConcurrency || 1) : 1;
    ort.env.logLevel = 'error';
    // proxy 模式下只支持 wasm EP（推理在 Worker 中执行，不阻塞画面）
    onStep && onStep('载入车牌模型...');
    // 和人脸模型共用同一个 Cache API 持久化缓存
    const [detAB, recAB, chars] = await Promise.all([
      _modelAB('/models/plate_det.onnx'),
      _modelAB('/models/plate_rec.onnx'),
      (async () => (await (await fetch('/models/chars.json')).json()))(),
    ]);
    const [det, rec] = await Promise.all([
      ort.InferenceSession.create(detAB, { executionProviders: ['wasm'], graphOptimizationLevel: 'all' }),
      ort.InferenceSession.create(recAB, { executionProviders: ['wasm'], graphOptimizationLevel: 'all' }),
    ]);
    this.det = det;
    this.rec = rec;
    this.detInput = det.inputNames[0];
    this.recInput = rec.inputNames[0];
    this.chars = chars;
    this.ready = true;
    onStep && onStep('车牌模型就绪');
  }

  // 推理异常（偶发 kernel/内存问题）时重建 WASM session 后重试一次
  async _runWithFallback(which, feeds) {
    try {
      return await this[which].run(feeds);
    } catch (e) {
      const url = which === 'det' ? '/models/plate_det.onnx' : '/models/plate_rec.onnx';
      console.warn('plate ' + which + ' 推理失败，重建 WASM session 重试:', e.message);
      const ab = await _modelAB(url);
      this[which] = await ort.InferenceSession.create(ab, {
        executionProviders: ['wasm'], graphOptimizationLevel: 'all'
      });
      if (which === 'det') this.detInput = this.det.inputNames[0];
      else this.recInput = this.rec.inputNames[0];
      return await this[which].run(feeds);
    }
  }

  preprocess(video) {
    const vw = video.videoWidth, vh = video.videoHeight;
    const r = Math.min(DET_SIZE / vw, DET_SIZE / vh);
    const nw = Math.round(vw * r), nh = Math.round(vh * r);
    const dw = (DET_SIZE - nw) / 2, dh = (DET_SIZE - nh) / 2;
    const ctx = this.ctx;
    ctx.fillStyle = '#000';
    ctx.fillRect(0, 0, DET_SIZE, DET_SIZE);
    ctx.drawImage(video, dw, dh, nw, nh);
    const img = ctx.getImageData(0, 0, DET_SIZE, DET_SIZE).data;
    const data = new Float32Array(1 * 3 * DET_SIZE * DET_SIZE);
    const plane = DET_SIZE * DET_SIZE;
    for (let i = 0, p = 0; i < img.length; i += 4, p++) {
      data[p] = img[i] / 255;
      data[p + plane] = img[i + 1] / 255;
      data[p + 2 * plane] = img[i + 2] / 255;
    }
    return { tensor: data, ratio: r, dw, dh, vw, vh };
  }

  async detect(video) {
    if (!this.ready) return [];
    const { tensor, ratio, dw, dh, vw, vh } = this.preprocess(video);
    const out = await this._runWithFallback('det', {
      [this.detInput]: new ort.Tensor('float32', tensor, [1, 3, DET_SIZE, DET_SIZE])
    });
    const data = out[this.det.outputNames[0]].data;
    const rows = data.length / 15;
    const cands = [];
    for (let i = 0; i < rows; i++) {
      const o = i * 15;
      const conf = data[o + 4];
      if (conf < 0.3) continue;
      const cx = (data[o] - dw) / ratio;
      const cy = (data[o + 1] - dh) / ratio;
      const w = data[o + 2] / ratio;
      const h = data[o + 3] / ratio;
      if (w < 12 || h < 4 || w > vw * 1.1) continue;
      const box = { x: cx - w / 2, y: cy - h / 2, w, h, score: conf };
      const pts = [];
      for (let k = 0; k < 5; k++) {
        pts.push([
          (data[o + 5 + k * 2] - dw) / ratio,
          (data[o + 6 + k * 2] - dh) / ratio
        ]);
      }
      let minX = vw, minY = vh, maxX = 0, maxY = 0;
      for (const [x, y] of pts) {
        minX = Math.min(minX, x); maxX = Math.max(maxX, x);
        minY = Math.min(minY, y); maxY = Math.max(maxY, y);
      }
      if (maxX - minX > 4 && maxY - minY > 2) {
        box.x = Math.max(0, minX);
        box.y = Math.max(0, minY);
        box.w = Math.min(vw - box.x, maxX - minX);
        box.h = Math.min(vh - box.y, maxY - minY);
      }
      box.x = Math.max(0, box.x);
      box.y = Math.max(0, box.y);
      cands.push(box);
    }
    return nmsBoxes(cands, 0.55).slice(0, 5);
  }

  // 把检测框裁剪成识别所需尺寸，返回归一化后的张量与上传用画布
  makeRecInput(video, box) {
    const pad = Math.max(2, box.h * 0.08);
    const sx = Math.max(0, box.x - pad), sy = Math.max(0, box.y - pad);
    const sw = Math.min(video.videoWidth - sx, box.w + pad * 2);
    const sh = Math.min(video.videoHeight - sy, box.h + pad * 2);

    const ratio = sw / sh;
    let resizedW = Math.round(REC_H * ratio);
    resizedW = Math.max(20, Math.min(REC_W, resizedW));

    const small = new OffscreenCanvas(resizedW, REC_H);
    const sctx = small.getContext('2d', { willReadFrequently: true });
    sctx.drawImage(video, sx, sy, sw, sh, 0, 0, resizedW, REC_H);
    const img = sctx.getImageData(0, 0, resizedW, REC_H).data;

    const plane = REC_H * REC_W;
    const data = new Float32Array(3 * plane);
    for (let y = 0; y < REC_H; y++) {
      for (let x = 0; x < resizedW; x++) {
        const i = (y * resizedW + x) * 4;
        const j = y * REC_W + x;
        data[j] = (img[i] - 127.5) / 127.5;
        data[j + plane] = (img[i + 1] - 127.5) / 127.5;
        data[j + 2 * plane] = (img[i + 2] - 127.5) / 127.5;
      }
    }

    const crop = document.createElement('canvas');
    crop.width = Math.max(48, Math.round(sw));
    crop.height = Math.max(16, Math.round(sh));
    crop.getContext('2d').drawImage(video, sx, sy, sw, sh, 0, 0, crop.width, crop.height);

    return { tensor: data, crop };
  }

  async recognize(video, box) {
    const { tensor, crop } = this.makeRecInput(video, box);
    const out = await this._runWithFallback('rec', {
      [this.recInput]: new ort.Tensor('float32', tensor, [1, 3, REC_H, REC_W])
    });
    const t = out[this.rec.outputNames[0]];
    const dims = t.dims;
    const T = dims[1], C = dims[2];
    const data = t.data;
    let text = '';
    let confSum = 0, confN = 0;
    let prev = -1;
    for (let i = 0; i < T; i++) {
      let best = 0, bestIdx = 0, sum = 0;
      for (let c = 0; c < C; c++) {
        const v = data[i * C + c];
        sum += v;
        if (v > best) { best = v; bestIdx = c; }
      }
      if (bestIdx === 0) { prev = bestIdx; continue; }
      if (bestIdx === prev) continue;
      prev = bestIdx;
      const ch = this.chars[bestIdx] ?? '';
      if (!ch) continue;
      text += ch;
      confSum += best / (sum || 1);
      confN++;
    }
    return { text, conf: confN ? confSum / confN : 0, crop, box };
  }

  async detectAndRecognize(video) {
    const dets = await this.detect(video);
    const out = [];
    for (const d of dets.slice(0, 5)) {
      if (d.w < 24 || d.h < 8) continue;
      const r = await this.recognize(video, d);
      if (r.text.length >= 5) out.push(r);
    }
    return out;
  }
}

window.PlateEngine = PlateEngine;
