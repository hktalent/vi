let page = 1;
let total = 0;
const SIZE = 20;
let selected = new Set(); // 选中的事件 ID

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

function updateDeleteBtn() {
  const btn = document.getElementById('btnDelete');
  btn.style.display = selected.size > 0 ? 'inline-block' : 'none';
  btn.textContent = `删除选中 (${selected.size})`;
}

function renderPageNumbers(totalPages) {
  const el = document.getElementById('pageNumbers');
  if (totalPages <= 1) { el.innerHTML = ''; return; }
  let html = '';
  const maxShow = 7; // 最多显示 7 个页码
  let start = Math.max(1, page - 3);
  let end = Math.min(totalPages, start + maxShow - 1);
  if (end - start < maxShow - 1) start = Math.max(1, end - maxShow + 1);

  if (start > 1) {
    html += `<button class="btn small" onclick="search(1)">1</button>`;
    if (start > 2) html += `<span class="muted">…</span>`;
  }
  for (let i = start; i <= end; i++) {
    html += `<button class="btn small ${i === page ? 'primary' : ''}" onclick="search(${i})">${i}</button>`;
  }
  if (end < totalPages) {
    if (end < totalPages - 1) html += `<span class="muted">…</span>`;
    html += `<button class="btn small" onclick="search(${totalPages})">${totalPages}</button>`;
  }
  el.innerHTML = html;
}

async function search(p = 1) {
  page = p;
  selected.clear();
  updateDeleteBtn();
  const v = q.value.trim();
  const tbody = document.getElementById('rows');
  if (!v) {
    tbody.innerHTML = '<tr><td colspan="7" class="muted">请输入查询条件。</td></tr>';
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
    const totalPages = Math.ceil(total / SIZE);
    document.getElementById('resultTitle').textContent =
      `结果 · 共 ${data.total} 条 · 第 ${data.page} 页（每页 ${data.page_size}）`;
    document.getElementById('pageInfo').textContent = data.total ? `${(page - 1) * SIZE + 1} - ${(page - 1) * SIZE + data.items.length}` : '';
    document.getElementById('prev').disabled = page <= 1;
    document.getElementById('next').disabled = !data.has_more;
    renderPageNumbers(totalPages);
    if (!data.items.length) {
      tbody.innerHTML = '<tr><td colspan="7" class="muted">没有匹配的记录。</td></tr>';
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
          <td><input type="checkbox" data-id="${e.id}" onchange="toggleSelect('${e.id}', this.checked)"></td>
          <td class="mono">${esc(e.captured_at)}</td>
          <td><span class="badge ${e.kind}">${isPlate ? '车牌' : '人脸'}</span></td>
          <td>${obj}${attrs ? `<div class="muted" style="font-size:11px;">${esc(attrs)}</div>` : ''}${e.sim ? `<div class="muted" style="font-size:11px;">相似度 ${(e.sim * 100).toFixed(1)}%</div>` : ''}</td>
          <td>${esc(e.address || '—')}</td>
          <td class="mono muted" style="font-size:11px;">${esc(fmtCoord(e.lat, e.lon) || '—')}</td>
          <td><a href="${e._signedImage}" target="_blank"><img class="thumb ${e.kind}" src="${e._signedImage}" alt="" /></a></td>
        </tr>`;
    }).join('');
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="7" class="muted">查询失败：${esc(e.message)}</td></tr>`;
  }
}

function toggleSelect(id, checked) {
  if (checked) selected.add(id);
  else selected.delete(id);
  updateDeleteBtn();
}

function selectAll() {
  const boxes = document.querySelectorAll('#rows input[type="checkbox"]');
  const allChecked = selected.size === boxes.length && boxes.length > 0;
  boxes.forEach(cb => {
    cb.checked = !allChecked;
    toggleSelect(cb.dataset.id, !allChecked);
  });
}

async function deleteSelected() {
  if (selected.size === 0) return;
  if (!confirm(`确定删除选中的 ${selected.size} 条记录？此操作不可恢复。`)) return;
  try {
    const data = await api.post('/api/events/batch-delete', { ids: [...selected] });
    toast(`已删除 ${data.deleted} 条`);
    selected.clear();
    search(page);
  } catch (e) {
    toast('删除失败: ' + e.message);
  }
}

document.querySelectorAll('.tabs [data-mode]').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tabs [data-mode]').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    mode = btn.dataset.mode;
    applyMode();
    document.getElementById('rows').innerHTML = '<tr><td colspan="7" class="muted">输入条件后查询。</td></tr>';
    document.getElementById('resultTitle').textContent = '结果';
    document.getElementById('pageNumbers').innerHTML = '';
  });
});

document.getElementById('btnSearch').addEventListener('click', () => search(1));
q.addEventListener('keydown', e => { if (e.key === 'Enter') search(1); });
document.getElementById('prev').addEventListener('click', () => { if (page > 1) search(page - 1); });
document.getElementById('next').addEventListener('click', () => { if (page * SIZE <= total) search(page + 1); });
document.getElementById('btnSelectAll').addEventListener('click', selectAll);
document.getElementById('btnDelete').addEventListener('click', deleteSelected);

applyMode();
