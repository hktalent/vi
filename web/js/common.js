// 通用工具：API 请求、时间格式、提示、鸣叫、登录态
const TOKEN_KEY = 'vi_token';

// Auth 模块：token 存取、登录/登出/会话检查
const Auth = {
  get token() { return localStorage.getItem(TOKEN_KEY) || ''; },
  set token(v) {
    if (v) localStorage.setItem(TOKEN_KEY, v);
    else localStorage.removeItem(TOKEN_KEY);
  },
  isLoggedIn() { return !!this.token; },
  requireLogin(loginUrl = '/login.html') {
    if (!this.isLoggedIn()) {
      const q = new URLSearchParams({ next: location.pathname + location.search });
      location.replace(loginUrl + '?' + q.toString());
      return false;
    }
    return true;
  },
  async login(username, password) {
    const r = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password })
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    this.token = j.token;
    return j;
  },
  async logout() {
    if (this.token) {
      await fetch('/api/logout', {
        method: 'POST',
        headers: this._headers()
      }).catch(() => {});
    }
    this.token = '';
  },
  async me() {
    const r = await fetch('/api/me', { headers: this._headers() });
    if (!r.ok) { this.token = ''; return null; }
    return r.json().catch(() => null);
  },
  async changePassword(oldPwd, newPwd) {
    const r = await fetch('/api/password', {
      method: 'POST',
      headers: { ...this._headers(), 'Content-Type': 'application/json' },
      body: JSON.stringify({ old_password: oldPwd, new_password: newPwd })
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    return j;
  },
  // mediaUrl: 传入图片相对路径（如 "2026/11/05/face-xxx.jpg"），返回带签名的临时 URL
  // 签名 URL 15 分钟有效，浏览器原生 <img> 可直接访问，不能被长期复用
  async mediaUrl(path) {
    if (!path) return '';
    // 绝对 URL（http/https）直接返回
    if (/^https?:\/\//i.test(path)) return path;
    // 规范化相对路径，确保不带前导 /
    let rel = path.replace(/^\/media\//, '').replace(/^\//, '');
    // 优先：已登录 → 调 /api/media-url 拿签名 URL
    if (this.token) {
      try {
        const r = await fetch('/api/media-url?path=' + encodeURIComponent(rel), {
          headers: this._headers()
        });
        if (r.ok) {
          const j = await r.json();
          return j.url; // 形如 /media/xxx.jpg?sig=xxx&exp=xxx
        }
        // 401 → token 过期/无效，清掉，不要再 fallback 用过期 token
        if (r.status === 401) {
          this.token = ''; // 触发 setter → localStorage.removeItem
          // 不 return，继续往下走 fallback（此时 this.token 已清空）
        }
      } catch (e) { /* 网络错误：token 仍可能有效，fallback 用主 token */ }
    }
    // Fallback：签名拿不到 → 如果还有主 token 就拼 query；没有就返回空字符串（由调用方处理）
    let url = '/media/' + rel;
    if (this.token) {
      // 还有有效 token（网络错误导致签名失败，但 token 没过期）
      url += '?token=' + encodeURIComponent(this.token);
      return url;
    }
    // 真没 token / token 过期 → 返回空字符串让调用方处理（addFeed 会跳过 img 或显示占位）
    return '';
  },
  _headers(extra = {}) {
    const h = { ...extra };
    if (this.token) h['Authorization'] = 'Bearer ' + this.token;
    return h;
  }
};

// ── Cache API 持久化缓存模型文件 ──
// Safari/iOS 限制：单个 origin 通常 500MB 上限，resp.clone() 大文件可能失败
// 关键：iOS Safari WKWebView per-process 内存上限 ~500MB
//       Cache API hit.arrayBuffer() 把模型全读进内存 → ORT create 再拷贝到 WASM → 双份内存
//       arc.onnx(130MB) + FairFace三模型(250MB) × 2 = 760MB → 💥 崩溃
// 修复：iOS Safari 禁用 Cache API + 串行加载 + 每模型 createSession 后立刻释放原始 ArrayBuffer
const _MODEL_CACHE = 'vi-models-v1';
const _cacheDisabled = new Set();   // 记录哪些 URL 的 Cache API 存失败过，后续直接 fetch
const _isIOSSafari = /iP(hone|od|ad)/.test(navigator.platform) ||
  (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);  // iPadOS 也算
const _disableCacheAPI = _isIOSSafari;  // iOS/iPadOS 一律禁用 Cache API

async function _modelAB(url) {
  // iOS/iPadOS Safari: 禁用 Cache API — 400MB 模型全进 JS 内存 + WASM 拷贝 → 内存爆
  if (_disableCacheAPI || typeof caches === 'undefined' || _cacheDisabled.has(url)) {
    return (await fetch(url)).arrayBuffer();
  }
  try {
    const cache = await caches.open(_MODEL_CACHE);
    const hit = await cache.match(url);
    if (hit) return hit.arrayBuffer();
    const resp = await fetch(url);
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    // resp.clone() 在 Safari 大文件上可能失败 → try/catch 包
    try {
      await cache.put(url, resp.clone());
    } catch (cloneErr) {
      // Safari 有时 clone 大 Response 失败 → 不存，继续用 resp 本身
      console.warn('Cache API clone 失败（Safari 大文件），跳过持久化：', url, cloneErr.message);
      _cacheDisabled.add(url);
    }
    return resp.arrayBuffer();
  } catch (e) {
    // 整个 Cache API 流程挂了（磁盘满 / 隐私模式）→ 降级 fetch
    console.warn('Cache API 异常，降级 fetch：', e.message);
    _cacheDisabled.add(url);
    return (await fetch(url)).arrayBuffer();
  }
}

const api = {
  _buildInit(method, body) {
    const h = { ...(Auth._headers()), 'Content-Type': 'application/json' };
    const init = { method, headers: h };
    if (body !== undefined) init.body = JSON.stringify(body);
    return init;
  },
  async _req(url, init) {
    const r = await fetch(url, init);
    const j = await r.json().catch(() => ({}));
    if (r.status === 401) {
      Auth.token = '';
      // 只清 token，不自动跳转——让 UI 层（labels/search/stats 页面守卫）决定
      throw new Error('未登录');
    }
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    return j;
  },
  async get(url)    { return this._req(url, { headers: Auth._headers() }); },
  async post(url, body) { return this._req(url, this._buildInit('POST', body)); },
  async patch(url, body){ return this._req(url, this._buildInit('PATCH', body)); },
  async del(url)    { return this._req(url, { method: 'DELETE', headers: Auth._headers() }); }
};

function toast(msg, kind = '', ms = 3200) {
  let wrap = document.querySelector('.toast-wrap');
  if (!wrap) {
    wrap = document.createElement('div');
    wrap.className = 'toast-wrap';
    document.body.appendChild(wrap);
  }
  const el = document.createElement('div');
  el.className = 'toast ' + kind;
  el.innerHTML = msg;
  wrap.appendChild(el);
  setTimeout(() => el.remove(), ms);
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  }[c]));
}

// 用 WebAudio 合成“鸣叫”，无需音频文件
const beeper = {
  ctx: null,
  beep(times = 2) {
    try {
      if (!this.ctx) this.ctx = new (window.AudioContext || window.webkitAudioContext)();
      const ctx = this.ctx;
      if (ctx.state === 'suspended') ctx.resume();
      for (let i = 0; i < times; i++) {
        const t0 = ctx.currentTime + i * 0.28;
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.type = 'square';
        osc.frequency.setValueAtTime(880, t0);
        osc.frequency.setValueAtTime(1180, t0 + 0.12);
        gain.gain.setValueAtTime(0.0001, t0);
        gain.gain.exponentialRampToValueAtTime(0.25, t0 + 0.01);
        gain.gain.exponentialRampToValueAtTime(0.0001, t0 + 0.2);
        osc.connect(gain).connect(ctx.destination);
        osc.start(t0);
        osc.stop(t0 + 0.22);
      }
    } catch (e) {
      console.warn('beep failed', e);
    }
  }
};

function nowStamp() {
  const d = new Date();
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function todayStr() {
  return nowStamp().slice(0, 10);
}

// 记录定位的全局状态，供采集与页面展示
const geoState = {
  lat: null,
  lon: null,
  accuracy: null,
  watchId: null,
  timer: null,
  start(onChange) {
    if (!navigator.geolocation) {
      if (onChange) onChange({ error: '浏览器不支持定位' });
      return;
    }
    const handle = pos => {
      geoState.lat = pos.coords.latitude;
      geoState.lon = pos.coords.longitude;
      geoState.accuracy = pos.coords.accuracy;
      if (onChange) onChange({ lat: geoState.lat, lon: geoState.lon, accuracy: geoState.accuracy });
    };
    const fail = err => {
      if (onChange) onChange({ error: err.message || '定位失败' });
    };
    // watchPosition 在局域网 http 下可能不可用，同时用 getCurrentPosition 兜底
    geoState.watchId = navigator.geolocation.watchPosition(handle, fail, {
      enableHighAccuracy: true, maximumAge: 3000, timeout: 8000
    });
    navigator.geolocation.getCurrentPosition(handle, fail, {
      enableHighAccuracy: true, maximumAge: 3000, timeout: 8000
    });
  },
  stop() {
    if (geoState.watchId !== null && navigator.geolocation) {
      navigator.geolocation.clearWatch(geoState.watchId);
      geoState.watchId = null;
    }
  }
};

function setChip(id, ok, text) {
  const el = document.getElementById(id);
  if (!el) return;
  el.className = 'chip ' + (ok === true ? 'ok' : ok === false ? 'bad' : 'warn');
  el.innerHTML = '<span class="dot"></span>' + esc(text);
}

function fmtCoord(lat, lon) {
  if (lat == null || lon == null) return '';
  return `${Number(lat).toFixed(6)}, ${Number(lon).toFixed(6)}`;
}

window.api = api;
window.Auth = Auth;
window.toast = toast;
window.esc = esc;
window.beeper = beeper;
window.nowStamp = nowStamp;
window.todayStr = todayStr;
window.geoState = geoState;
window.setChip = setChip;
window.fmtCoord = fmtCoord;
window._isIOSSafari = _isIOSSafari;
