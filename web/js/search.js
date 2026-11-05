let mode = 'date';
let page = 1;
let total = 0;
const SIZE = 20;

const q = document.getElementById('q');

const config = {
  date: { type: 'date', placeholder: '选择日期', hint: '例如：2026-11-04' },
  plate: { type: 'text', placeholder: '输入车牌号，如 浙A12345', hint: '支持部分匹配，如 12345' },
  label: { type: 'text', placeholder: '输入标签，如 老爹 / 老表', hint: '标签在「标签」页面创建' },
  place: { type: 'text', placeholder: '输入地名或街道，如 文一西路', hint: '按采集时解析出的地址做包含匹配' }
};

function applyMode() {
  const c = config[mode];
  q.type = c.type;
  q.placeholder = c.placeholder;
  q.value = mode === 'date' ? (q.value || todayStr()) : '';
  document.getElementById('hint').textContent = c.hint;
}

async function search(p = 1) {
  page = p;
  const v = q.value.trim();
  const tbody = document.getElementById('rows');
  if (!v) {
    tbody.innerHTML = '<tr><td colspan="6" class="muted">请输入查询条件。</td></tr>';
    return;
  }
  let url = '';
  if (mode === 'date') url = `/api/search/date?date=${encodeURIComponent(v)}&page=${page}`;
  if (mode === 'plate') url = `/api/search/plate?plate=${encodeURIComponent(v)}&page=${page}`;
  if (mode === 'label') url = `/api/search/label?name=${encodeURIComponent(v)}&page=${page}`;
  if (mode === 'place') url = `/api/search/place?q=${encodeURIComponent(v)}&page=${page}`;

  try {
    const data = await api.get(url);
    total = data.total;
    document.getElementById('resultTitle').textContent =
      `结果 · 共 ${data.total} 条 · 第 ${data.page} 页（每页 ${data.page_size}）`;
    document.getElementById('pageInfo').textContent = data.total ? `${(page - 1) * SIZE + 1} - ${(page - 1) * SIZE + data.items.length}` : '';
    document.getElementById('prev').disabled = page <= 1;
    document.getElementById('next').disabled = !data.has_more;
    if (!data.items.length) {
      tbody.innerHTML = '<tr><td colspan="6" class="muted">没有匹配的记录。</td></tr>';
      return;
    }
    // 批量换签名 URL
    const signedItems = await Promise.all(data.items.map(async e => ({
      ...e,
      _signedImage: await Auth.mediaUrl(e.image)
    })));
    tbody.innerHTML = signedItems.map(e => {
      const isPlate = e.kind === 'plate';
      const labels = e.labels || [];
      const obj = isPlate
        ? `<b class="mono">${esc(e.plate_text)}</b>`
        : (labels.length ? `<b>${esc(labels.join('、'))}</b>` : `<span class="muted mono">${esc((e.person_id || '').slice(0, 10))}…</span>`);
      const attrs = isPlate ? '' : [
        e.gender === 'male' ? '男' : e.gender === 'female' ? '女' : '',
        e.age_range ? e.age_range + '岁' : '',
        e.glasses === 'yes' ? '戴眼镜' : e.glasses === 'no' ? '无眼镜' : ''
      ].filter(Boolean).join(' · ');
      return `
        <tr class="${isPlate ? 'plate' : 'face'}">
          <td class="mono">${esc(e.captured_at)}</td>
          <td><span class="badge ${e.kind}">${isPlate ? '车牌' : '人脸'}</span></td>
          <td>${obj}${attrs ? `<div class="muted" style="font-size:11px;">${esc(attrs)}</div>` : ''}${e.sim ? `<div class="muted" style="font-size:11px;">相似度 ${(e.sim * 100).toFixed(1)}%</div>` : ''}</td>
          <td>${esc(e.address || '—')}</td>
          <td class="mono muted" style="font-size:11px;">${esc(fmtCoord(e.lat, e.lon) || '—')}</td>
          <td><a href="${e._signedImage}" target="_blank"><img class="thumb ${e.kind}" src="${e._signedImage}" alt="" /></a></td>
        </tr>`;
    }).join('');
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="6" class="muted">查询失败：${esc(e.message)}</td></tr>`;
  }
}

document.querySelectorAll('.tabs [data-mode]').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tabs [data-mode]').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    mode = btn.dataset.mode;
    applyMode();
    document.getElementById('rows').innerHTML = '<tr><td colspan="6" class="muted">输入条件后查询。</td></tr>';
    document.getElementById('resultTitle').textContent = '结果';
  });
});

document.getElementById('btnSearch').addEventListener('click', () => search(1));
q.addEventListener('keydown', e => { if (e.key === 'Enter') search(1); });
document.getElementById('prev').addEventListener('click', () => { if (page > 1) search(page - 1); });
document.getElementById('next').addEventListener('click', () => { if (page * SIZE <= total) search(page + 1); });

applyMode();
