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
      } catch (e) { /* 网络错误继续 fallback */ }
    }
    // Fallback：签名 URL 拿不到时 → 用主 token 做 query 参数（兼容后端 tokenFromHeader 的 query 兜底）
    // 主 token 24h 有效，比签名 URL 更久但也更容易泄露（当前 15min 签名没拿到时的降级）
    let url = '/media/' + rel;
    if (this.token) {
      url += '?token=' + encodeURIComponent(this.token);
    } else {
      // 真的没登录 → 也拼上 token= 让后端 401（而不是 fallback 成无 token URL 直接 401 让 img 显示坏图）
      url += '?token='; // 空 token → 后端 authenticate 失败 → 401
    }
    return url;
  },
  _headers(extra = {}) {
    const h = { ...extra };
    if (this.token) h['Authorization'] = 'Bearer ' + this.token;
    return h;
  }
};

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
