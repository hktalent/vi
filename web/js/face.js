// 人脸链路：MediaPipe FaceLandmarker 检测 + 关键点对齐 + ArcFace 512 维特征
// 注: _modelAB() Cache API 封装在 common.js 里，全局共享

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
    // 肤色检测用小画布：每帧每张脸都 new OffscreenCanvas 是主要的 GC 抖动源
    this.skinCanvas = new OffscreenCanvas(64, 64);
    this.skinCtx = this.skinCanvas.getContext('2d', { willReadFrequently: true });
    // 上传用的裁剪画布：toBlob 在调用瞬间就快照位图，轮转两张即可安全复用
    this.cropPool = [];
    this.cropIdx = 0;
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
    // ⚠️ iOS Safari：proxy 会额外创建一个 Worker 副本 WASM 内存 → 爆内存 → 关闭
    const isIOS = window._isIOSSafari;
    ort.env.wasm.proxy = !isIOS;
    ort.env.logLevel = 'error';

    // 预热 WASM runtime（显式 init 比第一次 createSession 隐式初始化快）
    // 注意：ort.env.wasm.init() 在某些 iOS Safari 构建上不存在，要先判
    if (typeof ort.env.wasm.init === 'function') {
      try { await ort.env.wasm.init(); } catch (e) { /* 忽略 */ }
    }

    onStep && onStep('载入人脸模型...');

    let arc, gender, age, glasses;
    // iOS 不提前 import MediaPipe（避免和 ORT 模型抢内存峰值），桌面端提前并行省时间
    let visionImport = null;

    if (isIOS) {
      // ── iOS Safari: 全部串行加载 ──
      // 峰值控制：arc(130MB) AB → createSession → 释放 AB → MediaPipe import(小) → landmarker create
      const loadAttrs = new URLSearchParams(location.search).get('iosfull') === '1';
      const opts = { executionProviders: ['wasm'], graphOptimizationLevel: 'basic' };

      // 1. arc.onnx (必需) — 130MB
      onStep && onStep('载入 ArcFace...');
      let ab = await _modelAB('/models/arc.onnx');
      arc = await ort.InferenceSession.create(ab, opts);
      ab = null;  // 立即释放原始 AB！让 GC 回收
      await new Promise(r => setTimeout(r, 30));  // 等一帧 GC

      if (loadAttrs) {
        // 2-4. 属性模型 (可选，iOS 默认跳过省 ~250MB)
        onStep && onStep('载入性别模型...');
        try { ab = await _modelAB('/models/face_gender.onnx'); gender = await ort.InferenceSession.create(ab, opts); }
        catch (e) { gender = null; }
        ab = null;

        onStep && onStep('载入年龄模型...');
        try { ab = await _modelAB('/models/face_age.onnx'); age = await ort.InferenceSession.create(ab, opts); }
        catch (e) { age = null; }
        ab = null;

        onStep && onStep('载入眼镜模型...');
        try { ab = await _modelAB('/models/face_glasses.onnx'); glasses = await ort.InferenceSession.create(ab, opts); }
        catch (e) { glasses = null; }
        ab = null;
      } else {
        onStep && onStep('跳过属性模型（iOS 内存优化，加 ?iosfull=1 启用）');
      }

      // 显式提示 GC
      if (typeof globalThis.gc === 'function') { try { globalThis.gc(); } catch (e) {} }
    } else {
      // ── 桌面端：并行加载最快 ──
      visionImport = import('/vendor/mp/vision_bundle.mjs');  // MediaPipe 提前并行
      onStep && onStep('并行载入人脸模型...');
      const [arcAB, genderAB, ageAB, glassesAB] = await Promise.all([
        _modelAB('/models/arc.onnx'),
        _modelAB('/models/face_gender.onnx').catch(() => null),
        _modelAB('/models/face_age.onnx').catch(() => null),
        _modelAB('/models/face_glasses.onnx').catch(() => null),
      ]);
      const [a, g, ag, gl] = await Promise.all([
        ort.InferenceSession.create(arcAB, { executionProviders: ['wasm'], graphOptimizationLevel: 'basic' }),
        genderAB ? ort.InferenceSession.create(genderAB, { executionProviders: ['wasm'], graphOptimizationLevel: 'basic' }).catch(() => null) : Promise.resolve(null),
        ageAB    ? ort.InferenceSession.create(ageAB,    { executionProviders: ['wasm'], graphOptimizationLevel: 'basic' }).catch(() => null) : Promise.resolve(null),
        glassesAB? ort.InferenceSession.create(glassesAB,{ executionProviders: ['wasm'], graphOptimizationLevel: 'basic' }).catch(() => null) : Promise.resolve(null),
      ]);
      arc = a; gender = g; age = ag; glasses = gl;
    }

    this.session = arc;
    this.inName = arc.inputNames[0];
    this.outName = arc.outputNames[0];
    this.provider = (arc.executionProviders && arc.executionProviders()?.[0]) || '';
    this.sessGender = gender;
    this.sessAge = age;
    this.sessGlasses = glasses;

    // MediaPipe: iOS 串行 import（arc 已释放无峰值冲突），桌面端取提前并行的结果
    onStep && onStep('载入 MediaPipe...');
    const vision = visionImport ? await visionImport : await import('/vendor/mp/vision_bundle.mjs');
    const fileset = await vision.FilesetResolver.forVisionTasks('/vendor/mp/wasm');
    // GPU delegate 在部分机器的 ANGLE/Metal 上会导致 WebGL 上下文丢失（检测直接失效甚至拖崩系统），
    // 默认用 CPU(XNNPACK)；需要 GPU 时用 ?mpgpu=1 显式开启
    const wantGPU = !isIOS && new URLSearchParams(location.search).get('mpgpu') === '1';
    // iOS Safari WKWebView 内存紧张：numFaces 10→3 省 WASM 内存 + 检测 CPU 开销
    this.maxFaces = isIOS ? 3 : 10;
    this.vision = vision;
    this.fileset = fileset;
    this.landmarker = await this.createLandmarker(wantGPU ? 'GPU' : 'CPU', this.maxFaces);
    onStep && onStep('人脸模型就绪');
  }

  async createLandmarker(delegate, maxFaces = 10) {
    const mk = d => this.vision.FaceLandmarker.createFromOptions(this.fileset, {
      baseOptions: { modelAssetPath: '/models/face_landmarker.task', delegate: d },
      runningMode: 'VIDEO',
      numFaces: maxFaces,
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
      this.landmarker = await this.createLandmarker('CPU', this.maxFaces || 10);
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

      let minX = vw, minY = vh, maxX = 0, maxY = 0;
      for (const l of landmarks) {
        const x = l.x * vw, y = l.y * vh;
        if (x < minX) minX = x;
        if (y < minY) minY = y;
        if (x > maxX) maxX = x;
        if (y > maxY) maxY = y;
      }
      const fw = maxX - minX, fh = maxY - minY;
      if (fw < 16 || fh < 16) continue;
      const padX = fw * 0.22, padY = fh * 0.28;
      const box = {
        x: Math.max(0, minX - padX),
        y: Math.max(0, minY - padY),
        w: Math.min(vw - Math.max(0, minX - padX), fw + padX * 2),
        h: Math.min(vh - Math.max(0, minY - padY), fh + padY * 2)
      };

      // 肤色检测：过滤误检（领带、花朵、布料等无肤色区域）
      if (!this._hasSkinTone(video, box)) continue;

      // 只返回框与 5 点：对齐图仅在真正要推理时才生成（每 track 约 3s 一次），
      // 原来每帧每张脸都要 drawImage + 新建 112×112 canvas，是逐帧的主要开销之一
      out.push({ box, five });
    }
    return out;
  }

  // align 把 5 点对齐到 112×112，返回复用的对齐画布。
  // 调用方必须立刻消费（embed/attributes）；推理队列是串行的，因此单画布安全。
  align(video, five) {
    const t = estimateSimilarity(five, FACE_REF);
    const ctx = this.alignCtx;
    ctx.setTransform(t.a, t.b, -t.b, t.a, t.tx, t.ty);
    ctx.drawImage(video, 0, 0, video.videoWidth, video.videoHeight);
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    return this.alignCanvas;
  }

  // _hasSkinTone 检查裁剪区域中肤色像素比例（RGB 简化判断，避免 HSV 转换开销）。
  // 缩到 64×64 再统计：判定是"比例"阈值，分辨率对结果几乎无影响，但像素量降到 1/N。
  _hasSkinTone(video, box) {
    const { x, y, w, h } = box;
    if (w < 20 || h < 20) return false;
    const sw = 64, sh = Math.max(16, Math.min(64, Math.round(64 * h / w)));
    const canvas = this.skinCanvas;
    if (canvas.width !== sw || canvas.height !== sh) { canvas.width = sw; canvas.height = sh; }
    const ctx = this.skinCtx;
    ctx.drawImage(video, x, y, w, h, 0, 0, sw, sh);
    const data = ctx.getImageData(0, 0, sw, sh).data;
    let skinCount = 0;
    const total = sw * sh;
    for (let i = 0; i < data.length; i += 4) {
      const r = data[i], g = data[i + 1], b = data[i + 2];
      // RGB 肤色近似判断：R > G > B 且有一定饱和度
      if (r > 95 && g > 40 && b > 20 && r > g && r > b && (r - Math.min(g, b)) > 15) {
        skinCount++;
      }
    }
    return (skinCount / total) >= 0.15;
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
    // embedding norm 过滤：非人脸区域（领带/花朵/布料）的 norm 通常异常
    if (norm < 1.5 || norm > 8.0) return null;
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
    // 轮转复用两张 canvas：toBlob 在调用瞬间快照位图，轮转可避免同时被两次上传引用
    if (this.cropPool.length < 2) {
      this.cropPool.push(document.createElement('canvas'));
    }
    const canvas = this.cropPool[this.cropIdx++ % this.cropPool.length];
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
