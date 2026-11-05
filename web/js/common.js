// 通用工具：API 请求、时间格式、提示、鸣叫
const api = {
  async get(url) {
    const r = await fetch(url);
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    return j;
  },
  async post(url, body) {
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    return j;
  },
  async patch(url, body) {
    const r = await fetch(url, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    return j;
  },
  async del(url) {
    const r = await fetch(url, { method: 'DELETE' });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || ('HTTP ' + r.status));
    return j;
  }
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
window.toast = toast;
window.esc = esc;
window.beeper = beeper;
window.nowStamp = nowStamp;
window.todayStr = todayStr;
window.geoState = geoState;
window.setChip = setChip;
window.fmtCoord = fmtCoord;
