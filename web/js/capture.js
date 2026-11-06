// 实时采集编排：摄像头 → 人脸 / 车牌推理 → 上传 → 告警鸣叫
const video = document.getElementById('video');
const overlay = document.getElementById('overlay');
const ctx = overlay.getContext('2d');

const state = {
  running: false,
  face: null,
  plate: null,
  faceBoxes: [],
  plateBoxes: [],
  faceTracks: [],
  plateLast: new Map(),
  counters: { faces: 0, plates: 0, alerts: 0, seen: 0 },
  busyFace: false,
  busyPlate: false,
  // 推理队列：多个 track 入队后串行推理（ORT WASM 不支持并发），推理完立即上传
  inferQueue: [],
  inferWorkerRunning: false,
  timers: []
};

function updateCounters() {
  document.getElementById('numFaces').textContent = state.counters.faces;
  document.getElementById('numPlates').textContent = state.counters.plates;
  document.getElementById('numAlerts').textContent = state.counters.alerts;
  document.getElementById('numSeen').textContent = state.counters.seen;
}

// 实时诊断行：已由 chip 状态条替代，不再单独显示
let lastLive = '';
function setLive(msg) {
  // 保留函数签名但不写 DOM，chipCamera/chipFace 等已经覆盖状态展示
  if (msg === lastLive) return;
  lastLive = msg;
  const el = document.getElementById('liveMsg');
  if (el) el.textContent = '';
}

// 本地 blob URL 集合，组件卸载/reload 时统一 revoke 防内存泄漏
const _blobUrls = new Set();
function _blobUrl(blob) {
  const u = URL.createObjectURL(blob);
  _blobUrls.add(u);
  return u;
}
window.addEventListener('beforeunload', () => { _blobUrls.forEach(u => URL.revokeObjectURL(u)); _blobUrls.clear(); });

// addFeed: 首页实时采集场景，优先用传入的本地 blob URL（零延迟零网络），
// 没有 blob 时（历史回放/标签页刷新）才走后端 mediaUrl 鉴权
async function addFeed(res, kind, localBlobUrl) {
  const feed = document.getElementById('feed');
  if (feed.querySelector('.note')) feed.innerHTML = '';
  const e = res.event;
  const title = kind === 'plate'
    ? (e.plate_text || '车牌')
    : (e.labels && e.labels.length ? e.labels.join('、') : '未命名人脸');
  const attrs = kind === 'face' ? attrText({
    gender: e.gender, age_range: e.age_range, glasses: e.glasses
  }) : '';
  // 优先：本地 blob URL（首页实时采集 → 浏览器内存 → 零延迟零鉴权）
  let imgUrl = localBlobUrl || '';
  if (!imgUrl && e.image) {
    // 备选：后端鉴权 URL（历史数据 / 标签页刷新后）
    imgUrl = await Auth.mediaUrl(e.image);
  }
  // 附加元信息：海拔、方向
  const extras = [];
  if (e.altitude != null && !isNaN(e.altitude)) extras.push(`海拔 ${Math.round(e.altitude)}m`);
  if (e.heading != null && !isNaN(e.heading)) extras.push(`方向 ${headingToDirection(e.heading)}`);
  const div = document.createElement('div');
  div.className = 'feed-item';
  const imgHtml = imgUrl
    ? `<img src="${imgUrl}" alt="" />`
    : `<div class="img-placeholder" title="登录后可查看图片">🔒</div>`;
  div.innerHTML = `
    ${imgHtml}
    <div>
      <div class="title">${esc(title)} <span class="badge ${kind}">${kind === 'plate' ? '车牌' : '人脸'}</span>
        ${e.labels && e.labels.length ? '<span class="badge beep">' + esc(e.labels.join('、')) + '</span>' : ''}</div>
      ${attrs ? '<div class="meta">' + esc(attrs) + '</div>' : ''}
      <div class="meta">${esc(e.captured_at)} · ${esc(e.address || '无地址')}</div>
      <div class="meta">${esc(fmtCoord(e.lat, e.lon))}</div>
      ${extras.length ? '<div class="meta muted">' + esc(extras.join(' · ')) + '</div>' : ''}
    </div>`;
  feed.prepend(div);
  while (feed.children.length > 30) { feed.lastChild.remove(); }
}

async function handleResult(res, kind, localBlobUrl) {
  if (!res || res.skipped) return;
  state.counters.seen++;
  if (kind === 'face') state.counters.faces++; else state.counters.plates++;
  addFeed(res, kind, localBlobUrl);
  if (res.alert) {
    state.counters.alerts++;
    beeper.beep(2);
    const who = kind === 'plate' ? (res.event.plate_text || '') : '';
    toast(`<b>${esc(res.alert_text)}</b>${who ? ' · ' + esc(who) : ''}<br><span class="muted">${esc(res.event.address || '')}</span>`, 'alert', 4200);
  }
  updateCounters();
}

function formData(kind, capturedAt) {
  const fd = new FormData();
  fd.append('kind', kind);
  fd.append('captured_at', capturedAt);
  if (geoState.lat != null) {
    fd.append('lat', String(geoState.lat));
    fd.append('lon', String(geoState.lon));
  }
  if (geoState.altitude != null && !isNaN(geoState.altitude)) {
    fd.append('altitude', String(geoState.altitude));
  }
  if (geoState.heading != null && !isNaN(geoState.heading)) {
    fd.append('heading', String(geoState.heading));
  }
  // 主界面"输入名字"框：自动采集到人脸时把名字一起提交，后端自动打标签（留空则不打）
  if (kind === 'face') {
    const el = document.getElementById('captureName');
    const name = el ? el.value.trim() : '';
    if (name) fd.append('label_name', name);
  }
  return fd;
}

async function uploadFace(canvas, embedding, attrs) {
  const blob = await blobOf(canvas);                 // ← 一个 canvas → 一个 blob → 一次编码
  const localUrl = _blobUrl(blob);                   // ← 本地 blob URL（零延迟零鉴权）
  const fd = formData('face', nowStamp());
  fd.append('vector', JSON.stringify(embedding));
  if (attrs) {
    if (attrs.gender) fd.append('gender', attrs.gender);
    if (attrs.age_range) fd.append('age_range', attrs.age_range);
    if (attrs.glasses) fd.append('glasses', attrs.glasses);
  }
  fd.append('image', blob, 'face.jpg');              // ← 同一个 blob 传给 FormData
  const r = await fetch('/api/capture', { method: 'POST', body: fd, headers: Auth._headers() });
  const j = await r.json();
  if (!r.ok) throw new Error(j.error || '上传失败');
  await handleResult(j, 'face', localUrl);           // ← 本地 URL 传给 addFeed
}

async function uploadPlate(canvas, text, conf) {
  const blob = await blobOf(canvas);
  const localUrl = _blobUrl(blob);
  const fd = formData('plate', nowStamp());
  fd.append('plate_text', text);
  fd.append('plate_conf', String(conf));
  fd.append('image', blob, 'plate.jpg');
  const r = await fetch('/api/capture', { method: 'POST', body: fd, headers: Auth._headers() });
  const j = await r.json();
  if (!r.ok) throw new Error(j.error || '上传失败');
  await handleResult(j, 'plate', localUrl);
}

function blobOf(canvas) {
  return new Promise(resolve => canvas.toBlob(b => resolve(b), 'image/jpeg', 0.88));
}

// 属性短文本：男 30-39 戴眼镜
function attrText(a) {
  if (!a) return '';
  const parts = [];
  if (a.gender === 'male') parts.push('男');
  else if (a.gender === 'female') parts.push('女');
  if (a.age_range) parts.push(a.age_range);
  if (a.glasses === 'yes') parts.push('戴眼镜');
  else if (a.glasses === 'no') parts.push('无眼镜');
  return parts.join(' ');
}

// 推理队列 worker：串行处理 inferQueue 中的 track（ORT WASM 不支持跨 session 并发）
// 推理完 embedding + attrs 后**立即**上传（不卡 6s，6s 只防旁路重复）
function ensureInferWorker() {
  if (state.inferWorkerRunning) return;
  state.inferWorkerRunning = true;
  (async () => {
    while (state.inferQueue.length > 0) {
      const track = state.inferQueue.shift();
      if (!track) continue;
      // track 已消失（超过 1.5s 没被看到），跳过
      if (performance.now() - track.lastSeen > 1500) { track.inferring = false; continue; }
      try {
        const embedding = await state.face.embed(track._snap);
        track.embedding = embedding;
        const attrs = await state.face.attributes(track._snap);
        if (attrs) track.attrs = attrs;
        // 推理完立即上传——不卡 6s 防抖，推理本身已经 ~8s 足够冷却
        if (track.embedding) {
          track.lastSent = performance.now();
          await uploadFace(state.face.crop(video, track.box), track.embedding, track.attrs);
        }
      } catch (e) {
        console.warn('face infer worker', e);
        // 出错也设置 lastSent，避免 faceLoop 旁路立即再试（给 3s 冷却）
        track.lastSent = performance.now();
      }
      track.inferring = false;
    }
    state.inferWorkerRunning = false;
  })();
}

// 人脸循环：rAF 驱动，FaceLandmarker 每 N 帧跑一次（中间帧由 MediaPipe 跟踪器衔接）；
// embedding+属性合并推理按 track 低频（3s）排队串行执行；上传按 track 防抖（≥6s）。
// iOS Safari: 每 3 帧一次（省 CPU，WKWebView 不允许长时间主线程阻塞）
let faceFrame = 0;
const FACE_FRAME_SKIP = window._isIOSSafari ? 3 : 2;
async function faceLoop() {
  if (!state.running) return;
  requestAnimationFrame(faceLoop);
  if (state.busyFace || !state.face || !video.videoWidth) return;
  faceFrame++;
  if (faceFrame % FACE_FRAME_SKIP !== 0) return;
  state.busyFace = true;
  try {
    const faces = await state.face.detect(video, performance.now());
    const now = performance.now();
    state.faceBoxes = [];
    for (const f of faces) {
      let track = state.faceTracks.find(t => faceIou(t.box, f.box) > 0.45);
      if (!track) {
        track = { box: f.box, lastSent: -1e9, lastInfer: 0, lastSeen: now, attrs: null, embedding: null, inferring: false };
        state.faceTracks.push(track);
      }
      track.box = f.box;
      track.lastSeen = now;
      track._snap = f.snap;  // 最新对齐快照，供推理 worker 使用
      state.faceBoxes.push({ box: f.box, attrs: track.attrs });

      // 排队推理：新 track 立即入队；已有的每 3s 间隔
      if (!track.inferring && (track.lastInfer === 0 || now - track.lastInfer >= 3000)) {
        track.lastInfer = now;
        track.inferring = true;
        state.inferQueue.push(track);
        ensureInferWorker();
      }
      // 已有 embedding 且防抖期过了：直接上传（复用已有特征，不再推理）
      if (track.embedding && !track.inferring && now - track.lastSent > 6000) {
        track.lastSent = now;
        await uploadFace(state.face.crop(video, f.box), track.embedding, track.attrs);
      }
    }
    // 清理消失的 track，上限 12 个（支持多路同时跟踪）
    state.faceTracks = state.faceTracks
      .filter(t => now - t.lastSeen < 1500)
      .slice(-12);
    setLive(`人脸 ${faces.length} 张 · 跟踪 ${state.faceTracks.length} 个 · 队列 ${state.inferQueue.length}${state.inferWorkerRunning ? ' 推理中' : ''}`);
  } catch (e) {
    console.warn('face loop', e);
    setLive('人脸检测异常：' + e.message);
  } finally {
    state.busyFace = false;
  }
}

// 车牌循环：rAF 驱动，每 5 帧评估一次；画面基本静止（帧差极小）直接跳过推理。
let plateFrame = 0;
const diffCanvas = new OffscreenCanvas(96, 54);
const diffCtx = diffCanvas.getContext('2d', { willReadFrequently: true });
let prevFrame = null;
function frameChanged() {
  diffCtx.drawImage(video, 0, 0, 96, 54);
  const d = diffCtx.getImageData(0, 0, 96, 54).data;
  if (!prevFrame) { prevFrame = d.slice(); return true; }
  let sum = 0, n = 0;
  for (let i = 0; i < d.length; i += 16) { sum += Math.abs(d[i] - prevFrame[i]); n++; }
  prevFrame.set(d);
  return sum / n > 2.5;
}

async function plateLoop() {
  if (!state.running) return;
  requestAnimationFrame(plateLoop);
  if (state.busyPlate || !state.plate || !video.videoWidth) return;
  plateFrame++;
  if (plateFrame % 5 !== 0) return;
  if (!frameChanged()) return;
  state.busyPlate = true;
  try {
    const results = await state.plate.detectAndRecognize(video);
    state.plateBoxes = results.map(r => r.box);
    for (const r of results) {
      const key = r.text;
      const last = state.plateLast.get(key) || 0;
      if (performance.now() - last < 120000) continue;
      if (r.conf < 0.18) continue;
      state.plateLast.set(key, performance.now());
      await uploadPlate(r.crop, r.text, r.conf);
    }
  } catch (e) {
    console.warn('plate loop', e);
  } finally {
    state.busyPlate = false;
  }
}

function drawLoop() {
  requestAnimationFrame(drawLoop);
  if (!video.videoWidth) return;
  if (overlay.width !== video.videoWidth) {
    overlay.width = video.videoWidth;
    overlay.height = video.videoHeight;
  }
  ctx.clearRect(0, 0, overlay.width, overlay.height);
  ctx.lineWidth = Math.max(2, overlay.width / 500);
  ctx.font = `${Math.max(12, overlay.width / 55)}px -apple-system, sans-serif`;
  ctx.textBaseline = 'top';

  for (const f of state.faceBoxes) {
    const b = f.box || f;
    const text = '人脸' + (f.attrs ? ' ' + attrText(f.attrs) : '');
    ctx.strokeStyle = '#4dd4ac';
    ctx.strokeRect(b.x, b.y, b.w, b.h);
    const tw = ctx.measureText(text).width + 12;
    ctx.fillStyle = '#4dd4ac';
    ctx.fillRect(b.x, b.y - 20, tw, 20);
    ctx.fillStyle = '#06120d';
    ctx.fillText(text, b.x + 6, b.y - 18);
  }
  for (const b of state.plateBoxes) {
    ctx.strokeStyle = '#5aa9ff';
    ctx.strokeRect(b.x, b.y, b.w, b.h);
    ctx.fillStyle = '#5aa9ff';
    ctx.fillRect(b.x, b.y - 20, 44, 20);
    ctx.fillStyle = '#06120d';
    ctx.fillText('车牌', b.x + 6, b.y - 18);
  }
}

async function loadEngines() {
  const isIOS = window._isIOSSafari;
  // iOS Safari: 必须串行！并行加载 face + plate 会叠加 WASM 内存峰值爆 500MB
  // 桌面端：并行最快，两者互不干扰
  if (isIOS) {
    if (!state.face) {
      state.face = new FaceEngine();
      try {
        await state.face.init(msg => setChip('chipFace', null, msg));
        setChip('chipFace', true, '人脸模型就绪');
      } catch (e) {
        setChip('chipFace', false, '人脸模型失败');
        toast('人脸模型加载失败：' + esc(e.message), 'err', 6000);
        console.error(e);
        state.face = null;
      }
    }
    // 显式 GC 一次（Safari 可能不听，但多等一帧让内存释放）
    await new Promise(r => setTimeout(r, 50));
    if (!state.plate) {
      state.plate = new PlateEngine();
      try {
        await state.plate.init(msg => setChip('chipPlate', null, msg));
        setChip('chipPlate', true, '车牌模型就绪');
      } catch (e) {
        setChip('chipPlate', false, '车牌模型失败');
        toast('车牌模型加载失败：' + esc(e.message), 'err', 6000);
        console.error(e);
        state.plate = null;
      }
    }
  } else {
    // 桌面端并行最快
    const jobs = [];
    if (!state.face) {
      state.face = new FaceEngine();
      jobs.push(state.face.init(msg => setChip('chipFace', null, msg))
        .then(() => { setChip('chipFace', true, '人脸模型就绪'); })
        .catch(e => {
          setChip('chipFace', false, '人脸模型失败');
          toast('人脸模型加载失败：' + esc(e.message), 'err', 6000);
          console.error(e);
          state.face = null;
        }));
    }
    if (!state.plate) {
      state.plate = new PlateEngine();
      jobs.push(state.plate.init(msg => setChip('chipPlate', null, msg))
        .then(() => { setChip('chipPlate', true, '车牌模型就绪'); })
        .catch(e => {
          setChip('chipPlate', false, '车牌模型失败');
          toast('车牌模型加载失败：' + esc(e.message), 'err', 6000);
          console.error(e);
          state.plate = null;
        }));
    }
    await Promise.all(jobs);
  }
}

async function start() {
  document.getElementById('btnStart').disabled = true;
  try {
    // iOS 13+ 方向传感器授权必须在当前用户手势内（click）调用 requestPermission
    if (geoState._needOrientPerm) {
      try { await geoState.requestOrientPermission(); } catch (_) {}
    }
    const stream = await openCamera();
    video.srcObject = stream;
    await video.play();
    document.getElementById('emptyTip').style.display = 'none';
    setChip('chipCamera', true, '摄像头运行中');
    state.running = true;
    document.getElementById('btnStop').disabled = false;
    refreshCamerasDebounced(false); // 授权后设备名才可见（debounce 避免和 video.play 叠）
    await loadEngines();
    state.timers.forEach(clearTimeout);
    state.timers = [];
    faceLoop();
    plateLoop();
    setLive('模型就绪，正在检测人脸/车牌…');
    if (geoState.watchId === null) geoState.start(onGeo);
  } catch (e) {
    setChip('chipCamera', false, '摄像头不可用');
    toast('无法启动摄像头：' + esc(e.message), 'err', 6000);
    document.getElementById('btnStart').disabled = false;
  }
}

// 按选择的设备取流；设备被拔出/切换时退回默认摄像头
async function openCamera() {
  const sel = document.getElementById('selCamera');
  const devId = sel && sel.value;
  // iOS Safari：用 640×480 省 video stream frame buffer 内存
  // （WKWebView ~500MB 上限，1280×720 的 YUV buffer 比 640×480 多 4 倍）
  const isIOS = window._isIOSSafari;
  const base = isIOS
    ? { width: { ideal: 640 }, height: { ideal: 480 }, frameRate: { ideal: 25 } }
    : { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 25 } };
  const make = v => navigator.mediaDevices.getUserMedia({ video: v, audio: false });
  if (devId) {
    try {
      return await make({ ...base, deviceId: { exact: devId } });
    } catch (e) {
      // exact deviceId 在 iOS 上经常 OverconstrainedError → 退化为 ideal
      if (e.name === 'OverconstrainedError') {
        try { return await make({ ...base, deviceId: { ideal: devId } }); } catch (e2) {
          console.warn('deviceId ideal 也失败', e2.message);
        }
      }
      toast('所选摄像头不可用，改用默认摄像头', 'warn');
      if (sel) sel.value = '';
      // 明确选了 deviceId 但失败 → fallback，但按用户直觉 iOS 上默认前置
      return await make({ ...base, facingMode: isIOS ? { ideal: 'user' } : { ideal: 'environment' } });
    }
  }
  // 没选 deviceId：iOS Safari 默认前置（下拉框第一个通常是前置占位），桌面默认后置
  return await make({ ...base, facingMode: isIOS ? { ideal: 'user' } : { ideal: 'environment' } });
}

// 温和停止：iOS Safari 上必须等 track 完全 stop 再 release stream，否则下一次 getUserMedia 会崩
async function stop() {
  state.running = false;
  document.getElementById('btnStop').disabled = true;  // 立即锁按钮防连点
  const stream = video.srcObject;
  if (stream) {
    stream.getTracks().forEach(t => { t.enabled = false; t.stop(); });
    // iOS Safari: 等一个 macrotask 让 AVFoundation 释放
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  video.srcObject = null;
  ctx.clearRect(0, 0, overlay.width, overlay.height);
  document.getElementById('emptyTip').style.display = 'grid';
  setChip('chipCamera', null, '摄像头已停止');
  document.getElementById('btnStart').disabled = false;
  setLive('已停止');
  if (geoState.watchId !== null) { geoState.stop(); }
}

// Haversine: 两点 GPS 坐标的球面距离（米）。
function _haversine(lat1, lon1, lat2, lon2) {
  const R = 6371000; // 地球半径 m
  const toRad = d => d * Math.PI / 180;
  const dLat = toRad(lat2 - lat1);
  const dLon = toRad(lon2 - lon1);
  const a = Math.sin(dLat / 2) ** 2 +
            Math.cos(toRad(lat1)) * Math.cos(toRad(lat2)) * Math.sin(dLon / 2) ** 2;
  return 2 * R * Math.asin(Math.sqrt(a));
}

// 逆地理缓存：位移超过 GEO_REFRESH_DIST_M 米才重新请求后端。
// 一旦拿到地址就稳定显示，不会被坐标噪声打断。
const GEO_REFRESH_DIST_M = 50;   // 50 米内的 GPS 抖动不再刷新
let _cachedAddr = '';             // 上次逆地理结果（稳定显示）
let _cachedLat = null, _cachedLon = null; // 上次请求时的坐标
let _geoRefreshPending = false;   // 防止并发重复请求

async function refreshStageGeoHud() {
  const el = document.getElementById('stageGeoMeta');
  if (!el) return;
  const g = geoState;
  if (g.lat == null) { el.textContent = '等待定位...'; return; }

  // 速度（gps 自带 m/s）：≥ 4 m/s 视为车载/骑行 → km/h，否则步行 → m/s
  const extras = [];
  if (g.speed != null && !isNaN(g.speed) && g.speed > 0.2) {
    if (g.speed >= 4) {
      extras.push(`${(g.speed * 3.6).toFixed(1)} km/h`);     // 车载/骑行
    } else {
      extras.push(`${g.speed.toFixed(1)} m/s`);              // 步行/跑步
    }
  }
  if (g.altitude != null && !isNaN(g.altitude)) {
    extras.push(`海拔 ${Math.round(g.altitude)}m`);
  }
  if (g.heading != null && !isNaN(g.heading)) {
    extras.push(`方向 ${headingToDirection(g.heading)}`);
  }

  // 判断是否需要重新逆地理（位移超阈值 / 首次）
  const needRefresh =
    (_cachedLat == null) ||                                    // 首次
    (_cachedAddr === '') ||                                    // 上次没拿到地址
    (_haversine(g.lat, g.lon, _cachedLat, _cachedLon) > GEO_REFRESH_DIST_M);

  if (needRefresh && !_geoRefreshPending) {
    _geoRefreshPending = true;
    _cachedLat = g.lat; _cachedLon = g.lon;
    try {
      const j = await fetch(`/api/geocode?lat=${g.lat}&lon=${g.lon}`).then(r => r.json());
      if (j && j.address && j.address.trim()) _cachedAddr = j.address.trim();
    } catch (_) { /* 失败保持上次的地址，不要清空 */ }
    _geoRefreshPending = false;
  }

  // 显示：优先地址，地址为空才用坐标兜底
  const mainText = _cachedAddr || fmtCoord(g.lat, g.lon);
  el.textContent = mainText + (extras.length ? ' | ' + extras.join(' · ') : '');
}

function onGeo(g) {
  const el = document.getElementById('geoText');
  if (g.error) {
    setChip('chipGeo', false, '定位失败');
    el.textContent = '定位失败：' + g.error + '（采集仍会继续）';
    refreshStageGeoHud();
    return;
  }
  setChip('chipGeo', true, '已定位 ±' + Math.round(g.accuracy || 0) + 'm');
  const parts = [`当前坐标：${fmtCoord(g.lat, g.lon)}`];
  if (g.altitude != null && !isNaN(g.altitude)) parts.push(`海拔 ${Math.round(g.altitude)}m`);
  if (geoState.heading != null && !isNaN(geoState.heading)) {
    parts.push(`方向 ${headingToDirection(geoState.heading)} (${geoState.heading}°)`);
  }
  parts.push(`精度约 ${Math.round(g.accuracy || 0)} 米；每次上传都会携带定位与时间。`);
  el.textContent = parts.join('，');
  // 同步更新视频左上角 hud（chipCamera 同一行）
  refreshStageGeoHud();
}

async function health() {
  try {
    const h = await api.get('/api/health');
    setChip('chipQdrant', !!h.qdrant, h.qdrant ? 'Qdrant 已连接' : 'Qdrant 未连接');
    document.getElementById('thresholdText').textContent =
      `同一人脸余弦阈值 ${h.face_threshold}；重复冷却 ${h.cooldown_seconds} 秒；已入库 ${h.events} 条，标签 ${h.labels} 个。`;
    const missing = Object.entries(h.models).filter(([, v]) => !v).map(([k]) => k);
    if (missing.length) toast('缺少模型文件：' + esc(missing.join(', ')), 'err', 6000);
  } catch (e) {
    setChip('chipQdrant', false, '后端未连接');
  }
}

document.getElementById('btnStart').addEventListener('click', start);
document.getElementById('btnStop').addEventListener('click', () => stop());
document.getElementById('btnBeep').addEventListener('click', () => beeper.beep(2));

// 摄像头设备列表：授权前只能拿到占位名，授权后刷新才能显示真实名称
const CAM_KEY = 'vi_camera_id';
// refreshCameras: 枚举摄像头设备
// forceAuth = true: 在 iOS Safari 上如果还没授权，先静默请求一次 getUserMedia 拿权限
//                  （必须在用户手势内调用，比如按钮 click）
// refreshCamerasDebounced：防 iOS Safari 短时间多次触发 enumerateDevices 崩溃
// visibilitychange + video.play + start() 授权成功可能在 500ms 内连续触发 3 次
let _rcTimer = null;
let _rcLastAuth = false;
let _rcInProgress = false;  // 重入锁：防 iOS Safari tmp.stop() 触发 devicechange 形成并发
function refreshCamerasDebounced(forceAuth = false) {
  if (_rcInProgress) return;  // 正在跑就不再入队
  if (_rcTimer) clearTimeout(_rcTimer);
  _rcLastAuth = _rcLastAuth || forceAuth;
  _rcTimer = setTimeout(() => {
    const auth = _rcLastAuth;
    _rcTimer = null;
    _rcLastAuth = false;
    refreshCameras(auth);
  }, 300);
}

async function refreshCameras(forceAuth = false) {
  if (_rcInProgress) return;
  _rcInProgress = true;
  try {
  const sel = document.getElementById('selCamera');
  if (!sel) return;
  if (!navigator.mediaDevices || !navigator.mediaDevices.enumerateDevices) {
    sel.innerHTML = '<option value="">浏览器不支持设备枚举</option>';
    return;
  }
  let list = [];
  try {
    list = (await navigator.mediaDevices.enumerateDevices()).filter(d => d.kind === 'videoinput');
  } catch (e) {
    sel.innerHTML = '<option value="">枚举失败：' + esc(e.message) + '</option>';
    return;
  }
  const needsAuth = !list.length || !list.some(d => d.label && d.label.trim());
  if (forceAuth && needsAuth) {
    let tmp = null;
    try {
      tmp = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: { ideal: 'environment' } }, audio: false
      });
      // iOS Safari: 等 AVFoundation 完全初始化好再枚举
      await new Promise(r => setTimeout(r, 200));
      list = (await navigator.mediaDevices.enumerateDevices()).filter(d => d.kind === 'videoinput');
    } catch (e) {
      const msg = e.name === 'NotAllowedError'
        ? '⚠️ 未授权摄像头，请点击"启动摄像头"允许'
        : e.name === 'NotFoundError' ? '未检测到摄像头'
        : '⚠️ 设备枚举受限：' + esc(e.message.slice(0, 40));
      sel.innerHTML = '<option value="">' + msg + '</option>';
      return;
    } finally {
      // 立即 stop！重入锁 + debounce 已经防死循环，延迟反而导致页面 reload 时旧定时器执行
      if (tmp) tmp.getTracks().forEach(t => t.stop());
    }
  }
  if (!list.length) {
    sel.innerHTML = '<option value="">未检测到摄像头</option>';
    return;
  }
  let saved = '';
  try { saved = localStorage.getItem(CAM_KEY) || ''; } catch (e) {}
  sel.innerHTML = list.map((d, i) =>
    `<option value="${esc(d.deviceId)}">${esc(d.label || ('摄像头 ' + (i + 1)))}</option>`
  ).join('');
  let pick = '';
  if (saved && list.some(d => d.deviceId === saved)) {
    pick = saved;
  } else {
    const rear = list.find(d => /后|rear|environment|back/i.test(d.label || ''));
    pick = rear ? rear.deviceId : list[list.length - 1].deviceId;
  }
  sel.value = pick;
  } finally { _rcInProgress = false; }
}

// 刷新按钮（用户手势内 → forceAuth=true，能拿到真实设备名）
document.getElementById('btnRefreshCam').addEventListener('click', async () => {
  await refreshCameras(true);
  toast('已刷新设备列表', '', 1600);
});
document.getElementById('selCamera').addEventListener('change', async e => {
  try { localStorage.setItem(CAM_KEY, e.target.value); } catch (err) {}
  if (state.running) {
    await stop();   // iOS 必须等完全 stop 再 start
    setTimeout(() => start(), 200);  // 再等 200ms 让 AVFoundation 彻底释放
  }
});
// 事件回调：全部用 debounced + 首次屏蔽
const _pageLoadedAt = Date.now();
const _isIOS = /iP(hone|od|ad)/.test(navigator.platform);

// devicechange: iOS Safari 已知 bug —— iOS 上根本不触发，直接不监听
// 桌面端保留（Chrome/Edge/Firefox 正常）
if (!_isIOS && navigator.mediaDevices && navigator.mediaDevices.addEventListener) {
  navigator.mediaDevices.addEventListener('devicechange', () => refreshCamerasDebounced(false));
}

// visibilitychange: iOS Safari reload 时旧页面 hidden + 新页面 visible 几乎同时触发
// 加首次屏蔽：页面加载 2 秒内不响应，防 reload 过渡期间的混乱事件
document.addEventListener('visibilitychange', () => {
  if (Date.now() - _pageLoadedAt < 2000) return;  // 首次 2 秒屏蔽
  if (document.hidden) return;
  refreshCamerasDebounced(false);
});

// video.play: 只在页面加载后 3 秒才响应（防页面刚加载时 video.autoplay 触发）
video.addEventListener('play', () => {
  if (Date.now() - _pageLoadedAt < 3000) return;
  setTimeout(() => refreshCamerasDebounced(false), 500);
});

// beforeunload: 立即清理所有 pending debounce 定时器 + 重入锁 + 同步释放 AVFoundation
// iOS Safari 上页面 reload 必须在 beforeunload 里**同步** stop 所有 video track
// （不能等 async stop() 的 await 链，beforeunload 结束后 WKWebView 会硬杀旧页面）
window.addEventListener('beforeunload', () => {
  if (_rcTimer) { clearTimeout(_rcTimer); _rcTimer = null; }
  _rcInProgress = false;
  // 同步释放 video track — iOS Safari 上 AVFoundation 旧 session 不释放 → 新页面 getUserMedia 崩
  const s = video && video.srcObject;
  if (s) { s.getTracks().forEach(t => { try { t.enabled = false; t.stop(); } catch (_) {} }); }
  // 清空视频源
  try { video && (video.srcObject = null); } catch (_) {}
  // 清空推理队列 — 不让 worker 在新页面里继续跑
  state.inferQueue.length = 0;
  state.inferWorkerRunning = false;
  state.running = false;
  // MediaPipe landmarker 同步 close（iOS Safari reload 时不 close → WASM 内存泄漏爆）
  // 注意：ORT InferenceSession 没有 close()（WASM 内存由 GC 管），只有 MediaPipe landmarker 有
  try { if (state.face && state.face.landmarker && state.face.landmarker.close) state.face.landmarker.close(); } catch (_) {}
  try { if (geoState.watchId !== null) navigator.geolocation.clearWatch(geoState.watchId); } catch (_) {}
  // 清理 feed blob URLs（防止内存泄漏）
  _blobUrls.forEach(u => { try { URL.revokeObjectURL(u); } catch (_) {} });
  _blobUrls.clear();
});

// 初始枚举：forceAuth=false（页面加载时没有用户手势）
refreshCameras();

// 运行状态栏：默认收起（手机优先），用户手动展开后记住选择
const STATUS_COLLAPSE_KEY = 'vi_status_collapsed';
function setStatusCollapsed(v) {
  document.body.classList.toggle('status-collapsed', v);
  try { localStorage.setItem(STATUS_COLLAPSE_KEY, v ? '1' : '0'); } catch (e) {}
}
document.getElementById('btnCollapseStatus').addEventListener('click', (e) => { e.stopPropagation(); setStatusCollapsed(true); });
document.getElementById('statusMini').addEventListener('click', (e) => { e.stopPropagation(); setStatusCollapsed(false); });
// 收缩后整个 status-panel 都可点击展开（比只点小图标区域大）
document.getElementById('statusPanel').addEventListener('click', () => {
  if (document.body.classList.contains('status-collapsed')) {
    setStatusCollapsed(false);
  }
});
(function initStatusCollapse() {
  let v = null;
  try { v = localStorage.getItem(STATUS_COLLAPSE_KEY); } catch (e) {}
  setStatusCollapsed(v === null ? true : v === '1');
})();

// 定位默认开启，无需按钮
geoState.start(onGeo);

// 手机用 http://局域网IP 访问属于非安全上下文，浏览器会直接禁用摄像头
if (!window.isSecureContext) {
  toast('当前页面不是安全上下文：手机浏览器会禁止摄像头，请改用 HTTPS 或在本机 127.0.0.1 打开。', 'err', 9000);
}

updateCounters();
health();
drawLoop();

// 页面一打开就后台异步预加载所有模型，不等点击"启动摄像头"
// loadEngines 内部幂等（已加载则跳过），start() 再次调用会直接复用
loadEngines().catch(e => console.warn('预加载失败（不影响手动启动）:', e.message));
