// 人脸链路：MediaPipe FaceLandmarker 检测 + 关键点对齐 + ArcFace 512 维特征
const FACE_REF = [
  [38.2946, 51.6963],
  [73.5318, 51.5014],
  [56.0252, 71.7366],
  [41.5493, 92.3655],
  [70.7299, 92.2041]
];

// 解 4 元一次方程：相似变换 u = a*x - b*y + tx, v = b*x + a*y + ty
function solve4(A, b) {
  for (let i = 0; i < 4; i++) {
    let pivot = i;
    for (let r = i + 1; r < 4; r++) {
      if (Math.abs(A[r][i]) > Math.abs(A[pivot][i])) pivot = r;
    }
    [A[i], A[pivot]] = [A[pivot], A[i]];
    [b[i], b[pivot]] = [b[pivot], b[i]];
    const p = A[i][i] || 1e-9;
    for (let c = i; c < 4; c++) A[i][c] /= p;
    b[i] /= p;
    for (let r = 0; r < 4; r++) {
      if (r === i) continue;
      const f = A[r][i];
      for (let c = i; c < 4; c++) A[r][c] -= f * A[i][c];
      b[r] -= f * b[i];
    }
  }
  return b;
}

function estimateSimilarity(src, dst) {
  const A = [[0, 0, 0, 0], [0, 0, 0, 0], [0, 0, 0, 0], [0, 0, 0, 0]];
  const b = [0, 0, 0, 0];
  const rows = [];
  for (let i = 0; i < src.length; i++) {
    const [x, y] = src[i];
    const [u, v] = dst[i];
    rows.push({ row: [x, -y, 1, 0], rhs: u });
    rows.push({ row: [y, x, 0, 1], rhs: v });
  }
  for (const { row, rhs } of rows) {
    for (let i = 0; i < 4; i++) {
      for (let j = 0; j < 4; j++) A[i][j] += row[i] * row[j];
      b[i] += row[i] * rhs;
    }
  }
  const [a, bb, tx, ty] = solve4(A, b);
  return { a, b: bb, tx, ty };
}

function iou(a, b) {
  const x1 = Math.max(a.x, b.x), y1 = Math.max(a.y, b.y);
  const x2 = Math.min(a.x + a.w, b.x + b.w), y2 = Math.min(a.y + a.h, b.y + b.h);
  const inter = Math.max(0, x2 - x1) * Math.max(0, y2 - y1);
  const union = a.w * a.h + b.w * b.h - inter;
  return union <= 0 ? 0 : inter / union;
}

class FaceEngine {
  constructor() {
    this.session = null;
    this.landmarker = null;
    this.sessGender = null;
    this.sessAge = null;
    this.sessGlasses = null;
    this.inName = 'input_1';
    this.outName = 'output';
    this.alignCanvas = new OffscreenCanvas(112, 112);
    this.alignCtx = this.alignCanvas.getContext('2d', { willReadFrequently: true });
    this.attrCanvas = new OffscreenCanvas(224, 224);
    this.attrCtx = this.attrCanvas.getContext('2d', { willReadFrequently: true });
    this.lastTs = -1;
    this.provider = '';
  }

  async init(onStep) {
    if (typeof ort === 'undefined') throw new Error('ONNX Runtime Web 未加载');
    ort.env.wasm.wasmPaths = '/vendor/ort/';
    // 跨源隔离下才有多线程 WASM；否则自动回退单线程
    ort.env.wasm.numThreads = self.crossOriginIsolated
      ? Math.min(4, navigator.hardwareConcurrency || 1) : 1;
    // proxy=true：推理放到 Worker 里跑，避免 ArcFace/属性模型长时间阻塞主线程（画面卡顿）
    ort.env.wasm.proxy = true;
    ort.env.logLevel = 'error';

    onStep && onStep('并行载入人脸模型 (ArcFace + 属性 ×3 + MediaPipe)...');
    // 全部并行 fetch（Safari/Chrome 都会并发拉），各自缓存后 createSession
    const [arc, gender, age, glasses] = await Promise.all([
      ort.InferenceSession.create('/models/arc.onnx', { executionProviders: ['wasm'], graphOptimizationLevel: 'all' }),
      ort.InferenceSession.create('/models/face_gender.onnx', { executionProviders: ['wasm'], graphOptimizationLevel: 'all' }).catch(() => null),
      ort.InferenceSession.create('/models/face_age.onnx', { executionProviders: ['wasm'], graphOptimizationLevel: 'all' }).catch(() => null),
      ort.InferenceSession.create('/models/face_glasses.onnx', { executionProviders: ['wasm'], graphOptimizationLevel: 'all' }).catch(() => null),
    ]);
    this.session = arc;
    this.inName = arc.inputNames[0];
    this.outName = arc.outputNames[0];
    this.provider = (arc.executionProviders && arc.executionProviders()?.[0]) || '';
    this.sessGender = gender;
    this.sessAge = age;
    this.sessGlasses = glasses;

    const vision = await import('/vendor/mp/vision_bundle.mjs');
    const fileset = await vision.FilesetResolver.forVisionTasks('/vendor/mp/wasm');
    // GPU delegate 在部分机器的 ANGLE/Metal 上会导致 WebGL 上下文丢失（检测直接失效甚至拖崩系统），
    // 默认用 CPU(XNNPACK)；需要 GPU 时用 ?mpgpu=1 显式开启
    const wantGPU = new URLSearchParams(location.search).get('mpgpu') === '1';
    this.vision = vision;
    this.fileset = fileset;
    this.landmarker = await this.createLandmarker(wantGPU ? 'GPU' : 'CPU');
    onStep && onStep('人脸模型就绪');
  }

  async createLandmarker(delegate) {
    const mk = d => this.vision.FaceLandmarker.createFromOptions(this.fileset, {
      baseOptions: { modelAssetPath: '/models/face_landmarker.task', delegate: d },
      runningMode: 'VIDEO',
      numFaces: 10,
      minFaceDetectionConfidence: 0.4,
      minFacePresenceConfidence: 0.4,
      minTrackingConfidence: 0.4,
      outputFaceBlendshapes: false,
      outputFacialTransformationMatrixes: false
    });
    try {
      return await mk(delegate);
    } catch (e) {
      if (delegate !== 'CPU') {
        console.warn('FaceLandmarker ' + delegate + ' 初始化失败，回退 CPU：' + e.message);
        return await mk('CPU');
      }
      throw e;
    }
  }

  // 推理失败（GPU 上下文丢失等）时重建为 CPU 实例后重试一次
  async rawDetect(video, tsMs) {
    try {
      return this.landmarker.detectForVideo(video, tsMs) || {};
    } catch (e) {
      console.warn('FaceLandmarker 推理失败，重建 CPU 实例：' + e.message);
      try { this.landmarker.close && this.landmarker.close(); } catch (_) {}
      this.landmarker = await this.createLandmarker('CPU');
      return this.landmarker.detectForVideo(video, tsMs) || {};
    }
  }

  fivePoints(landmarks, vw, vh) {
    const p = i => [landmarks[i].x * vw, landmarks[i].y * vh];
    const mid = (a, b) => [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
    return [
      mid(p(33), p(133)),
      mid(p(362), p(263)),
      p(1),
      p(61),
      p(291)
    ];
  }

  async detect(video, tsMs) {
    if (!this.landmarker) return [];
    if (tsMs <= this.lastTs) tsMs = this.lastTs + 1;
    this.lastTs = tsMs;

    const res = await this.rawDetect(video, tsMs);
    const all = res.faceLandmarks || [];
    const vw = video.videoWidth, vh = video.videoHeight;
    const out = [];

    for (const landmarks of all) {
      const five = this.fivePoints(landmarks, vw, vh);
      const t = estimateSimilarity(five, FACE_REF);
      this.alignCtx.setTransform(t.a, t.b, -t.b, t.a, t.tx, t.ty);
      this.alignCtx.drawImage(video, 0, 0, vw, vh);
      this.alignCtx.setTransform(1, 0, 0, 1, 0, 0);

      let minX = vw, minY = vh, maxX = 0, maxY = 0;
      for (const l of landmarks) {
        const x = l.x * vw, y = l.y * vh;
        if (x < minX) minX = x;
        if (y < minY) minY = y;
        if (x > maxX) maxX = x;
        if (y > maxY) maxY = y;
      }
      const fw = maxX - minX, fh = maxY - minY;
      const padX = fw * 0.22, padY = fh * 0.28;
      const box = {
        x: Math.max(0, minX - padX),
        y: Math.max(0, minY - padY),
        w: Math.min(vw - Math.max(0, minX - padX), fw + padX * 2),
        h: Math.min(vh - Math.max(0, minY - padY), fh + padY * 2)
      };

      // 对齐结果快照，供低频的 embedding/属性推理使用（detect 本身只做检测+对齐）
      const snap = new OffscreenCanvas(112, 112);
      snap.getContext('2d').drawImage(this.alignCanvas, 0, 0);
      out.push({ box, snap });
    }
    return out;
  }

  async embed(snap) {
    const img = snap.getContext('2d').getImageData(0, 0, 112, 112).data;
    const data = new Float32Array(1 * 112 * 112 * 3);
    for (let i = 0, j = 0; i < img.length; i += 4) {
      data[j++] = (img[i] - 127.5) / 128;
      data[j++] = (img[i + 1] - 127.5) / 128;
      data[j++] = (img[i + 2] - 127.5) / 128;
    }
    const tensor = new ort.Tensor('float32', data, [1, 112, 112, 3]);
    const out = await this.session.run({ [this.inName]: tensor });
    const vec = out[this.outName].data;
    let norm = 0;
    for (let i = 0; i < vec.length; i++) norm += vec[i] * vec[i];
    norm = Math.sqrt(norm) || 1;
    return Array.from(vec, v => v / norm);
  }

  // 属性推理：性别 + 年龄段 + 是否戴眼镜（输入对齐人脸，缩放至 224，NCHW，(x/255-0.5)/0.5）
  async attributes(snap) {
    if (!this.sessGender && !this.sessAge && !this.sessGlasses) return null;
    this.attrCtx.drawImage(snap, 0, 0, 224, 224);
    const img = this.attrCtx.getImageData(0, 0, 224, 224).data;
    const plane = 224 * 224;
    const data = new Float32Array(3 * plane);
    for (let i = 0, j = 0; i < img.length; i += 4, j++) {
      data[j] = img[i] / 127.5 - 1;
      data[plane + j] = img[i + 1] / 127.5 - 1;
      data[2 * plane + j] = img[i + 2] / 127.5 - 1;
    }
    // proxy 模式会把 Tensor 的 ArrayBuffer transfer 给 Worker（原 buffer 被 detach），
    // 因此每个模型都要用独立副本
    const cloneTensor = () => new ort.Tensor('float32', new Float32Array(data), [1, 3, 224, 224]);
    const argmax = d => { let bi = 0; for (let i = 1; i < d.length; i++) if (d[i] > d[bi]) bi = i; return bi; };
    // 各 session 串行推理，避免并发问题
    const res = {};
    if (this.sessGender) {
      const o = await this.sessGender.run({ pixel_values: cloneTensor() });
      res.gender = argmax(o[this.sessGender.outputNames[0]].data) === 1 ? 'male' : 'female';
    }
    if (this.sessAge) {
      const o = await this.sessAge.run({ pixel_values: cloneTensor() });
      res.age_range = FaceEngine.AGE_LABELS[argmax(o[this.sessAge.outputNames[0]].data)];
    }
    if (this.sessGlasses) {
      const o = await this.sessGlasses.run({ pixel_values: cloneTensor() });
      res.glasses = argmax(o[this.sessGlasses.outputNames[0]].data) === 0 ? 'yes' : 'no';
    }
    return res;
  }

  crop(video, box, maxW = 320) {
    const canvas = document.createElement('canvas');
    const scale = Math.min(1, maxW / Math.max(1, box.w));
    canvas.width = Math.max(32, Math.round(box.w * scale));
    canvas.height = Math.max(32, Math.round(box.h * scale));
    const ctx = canvas.getContext('2d');
    ctx.drawImage(video, box.x, box.y, box.w, box.h, 0, 0, canvas.width, canvas.height);
    return canvas;
  }
}

FaceEngine.AGE_LABELS = ['0-2', '3-9', '10-19', '20-29', '30-39', '40-49', '50-59', '60-69', '70+'];

window.FaceEngine = FaceEngine;
window.faceIou = iou;
