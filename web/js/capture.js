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

// 实时诊断行：只在内容变化时写 DOM
let lastLive = '';
function setLive(msg) {
  if (msg === lastLive) return;
  lastLive = msg;
  const el = document.getElementById('liveStat');
  if (el) el.textContent = msg;
}

function addFeed(res, kind) {
  const feed = document.getElementById('feed');
  if (feed.querySelector('.note')) feed.innerHTML = '';
  const e = res.event;
  const title = kind === 'plate'
    ? (e.plate_text || '车牌')
    : (e.labels && e.labels.length ? e.labels.join('、') : '未命名人脸');
  const attrs = kind === 'face' ? attrText({
    gender: e.gender, age_range: e.age_range, glasses: e.glasses
  }) : '';
  const div = document.createElement('div');
  div.className = 'feed-item';
  div.innerHTML = `
    <img src="${e.image}" alt="" />
    <div>
      <div class="title">${esc(title)} <span class="badge ${kind}">${kind === 'plate' ? '车牌' : '人脸'}</span>
        ${e.labels && e.labels.length ? '<span class="badge beep">' + esc(e.labels.join('、')) + '</span>' : ''}</div>
      ${attrs ? '<div class="meta">' + esc(attrs) + '</div>' : ''}
      <div class="meta">${esc(e.captured_at)} · ${esc(e.address || '无地址')}</div>
      <div class="meta">${esc(fmtCoord(e.lat, e.lon))}</div>
    </div>`;
  feed.prepend(div);
  while (feed.children.length > 30) feed.lastChild.remove();
}

async function handleResult(res, kind) {
  if (!res || res.skipped) return;
  state.counters.seen++;
  if (kind === 'face') state.counters.faces++; else state.counters.plates++;
  addFeed(res, kind);
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
  return fd;
}

async function uploadFace(canvas, embedding, attrs) {
  const fd = formData('face', nowStamp());
  fd.append('vector', JSON.stringify(embedding));
  if (attrs) {
    if (attrs.gender) fd.append('gender', attrs.gender);
    if (attrs.age_range) fd.append('age_range', attrs.age_range);
    if (attrs.glasses) fd.append('glasses', attrs.glasses);
  }
  fd.append('image', await blobOf(canvas), 'face.jpg');
  const r = await fetch('/api/capture', { method: 'POST', body: fd });
  const j = await r.json();
  if (!r.ok) throw new Error(j.error || '上传失败');
  await handleResult(j, 'face');
}

async function uploadPlate(canvas, text, conf) {
  const fd = formData('plate', nowStamp());
  fd.append('plate_text', text);
  fd.append('plate_conf', String(conf));
  fd.append('image', await blobOf(canvas), 'plate.jpg');
  const r = await fetch('/api/capture', { method: 'POST', body: fd });
  const j = await r.json();
  if (!r.ok) throw new Error(j.error || '上传失败');
  await handleResult(j, 'plate');
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
// 推理完 embedding + attrs 后立即尝试上传，然后取下一个
function ensureInferWorker() {
  if (state.inferWorkerRunning) return;
  state.inferWorkerRunning = true;
  (async () => {
    while (state.inferQueue.length > 0) {
      // 取出最早入队的 track（先进先出）
      const track = state.inferQueue.shift();
      if (!track) continue;
      // track 已消失（超过 1.5s 没被看到），跳过
      if (performance.now() - track.lastSeen > 1500) { track.inferring = false; continue; }
      try {
        const embedding = await state.face.embed(track._snap);
        track.embedding = embedding;
        const attrs = await state.face.attributes(track._snap);
        if (attrs) track.attrs = attrs;
        // 推理结束立即尝试上传（页面切后台时 rAF 会停，不等下次检测帧）
        const t = performance.now();
        if (track.embedding && t - track.lastSent > 6000) {
          track.lastSent = t;
          await uploadFace(state.face.crop(video, track.box), track.embedding, track.attrs);
        }
      } catch (e) { console.warn('face infer worker', e); }
      track.inferring = false;
    }
    state.inferWorkerRunning = false;
  })();
}

// 人脸循环：rAF 驱动，FaceLandmarker 每 2 帧跑一次（中间帧由 MediaPipe 跟踪器衔接）；
// embedding+属性合并推理按 track 低频（3s）排队串行执行；上传按 track 防抖（≥6s）。
let faceFrame = 0;
async function faceLoop() {
  if (!state.running) return;
  requestAnimationFrame(faceLoop);
  if (state.busyFace || !state.face || !video.videoWidth) return;
  faceFrame++;
  if (faceFrame % 2 !== 0) return;
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

      // 排队推理：每 track 独立 3s 间隔，入队后不重复
      if (!track.inferring && now - track.lastInfer >= 3000) {
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
}

async function start() {
  document.getElementById('btnStart').disabled = true;
  try {
    const stream = await openCamera();
    video.srcObject = stream;
    await video.play();
    document.getElementById('emptyTip').style.display = 'none';
    setChip('chipCamera', true, '摄像头运行中');
    state.running = true;
    document.getElementById('btnStop').disabled = false;
    refreshCameras(); // 授权后设备名才可见
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
  const base = { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 25 } };
  const make = v => navigator.mediaDevices.getUserMedia({ video: v, audio: false });
  if (devId) {
    try {
      return await make({ ...base, deviceId: { exact: devId } });
    } catch (e) {
      if (e.name === 'OverconstrainedError' || e.name === 'NotFoundError') {
        toast('所选摄像头不可用，改用默认摄像头', 'warn');
        if (sel) sel.value = '';
      } else throw e;
    }
  }
  return await make({ ...base, facingMode: { ideal: 'environment' } });
}

function stop() {
  state.running = false;
  const stream = video.srcObject;
  if (stream) stream.getTracks().forEach(t => t.stop());
  video.srcObject = null;
  document.getElementById('emptyTip').style.display = 'grid';
  setChip('chipCamera', null, '摄像头已停止');
  document.getElementById('btnStart').disabled = false;
  document.getElementById('btnStop').disabled = true;
  ctx.clearRect(0, 0, overlay.width, overlay.height);
  setLive('已停止');
}

function onGeo(g) {
  const el = document.getElementById('geoText');
  if (g.error) {
    setChip('chipGeo', false, '定位失败');
    el.textContent = '定位失败：' + g.error + '（采集仍会继续）';
    return;
  }
  setChip('chipGeo', true, '已定位 ±' + Math.round(g.accuracy || 0) + 'm');
  el.textContent = `当前坐标：${fmtCoord(g.lat, g.lon)}，精度约 ${Math.round(g.accuracy || 0)} 米；每次上传都会携带定位与时间。`;
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
document.getElementById('btnStop').addEventListener('click', stop);
document.getElementById('btnBeep').addEventListener('click', () => beeper.beep(2));

// 摄像头设备列表：授权前只能拿到占位名，授权后刷新才能显示真实名称
const CAM_KEY = 'vi_camera_id';
async function refreshCameras() {
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
  let saved = '';
  try { saved = localStorage.getItem(CAM_KEY) || ''; } catch (e) {}
  if (!list.length) {
    sel.innerHTML = '<option value="">未检测到摄像头（请先允许摄像头权限）</option>';
    return;
  }
  sel.innerHTML = list.map((d, i) =>
    `<option value="${esc(d.deviceId)}">${esc(d.label || ('摄像头 ' + (i + 1)))}</option>`
  ).join('');
  const pick = saved && list.some(d => d.deviceId === saved) ? saved : list[0].deviceId;
  sel.value = pick;
}
document.getElementById('btnRefreshCam').addEventListener('click', () => {
  refreshCameras();
  toast('已刷新设备列表', '', 1600);
});
document.getElementById('selCamera').addEventListener('change', e => {
  try { localStorage.setItem(CAM_KEY, e.target.value); } catch (err) {}
  if (state.running) { stop(); start(); }  // 运行中切换立即生效
});
if (navigator.mediaDevices && navigator.mediaDevices.addEventListener) {
  navigator.mediaDevices.addEventListener('devicechange', refreshCameras);
}
refreshCameras();

// 运行状态栏：默认收起（手机优先），用户手动展开后记住选择
const STATUS_COLLAPSE_KEY = 'vi_status_collapsed';
function setStatusCollapsed(v) {
  document.body.classList.toggle('status-collapsed', v);
  try { localStorage.setItem(STATUS_COLLAPSE_KEY, v ? '1' : '0'); } catch (e) {}
}
document.getElementById('btnCollapseStatus').addEventListener('click', () => setStatusCollapsed(true));
document.getElementById('statusMini').addEventListener('click', () => setStatusCollapsed(false));
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
