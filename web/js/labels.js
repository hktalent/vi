let page = 1;
let total = 0;
const PAGE_SIZE = 20;
let kind = 'face'; // 当前身份类型 tab（人脸/车牌）
let selected = new Set(); // 选中的 event_id（采集到的身份）
let selectedLabels = new Set(); // 选中的标签 id（已有标签）
let onlyUnnamed = false; // 只显示未命名（labels 为空）

async function loadLabels() {
  const tbody = document.getElementById('labelRows');
  try {
    const data = await api.get('/api/labels');
    if (!data.items.length) {
      tbody.innerHTML = '<tr><td colspan="8" class="muted">还没有标签，先在下面的身份卡片里命名。</td></tr>';
      return;
    }
    tbody.innerHTML = data.items.map(l => `
      <tr>
        <td><input type="checkbox" class="labelSelBox" data-id="${l.id}" ${selectedLabels.has(l.id) ? 'checked' : ''} /></td>
        <td><b>${esc(l.name)}</b></td>
        <td><span class="badge ${l.kind}">${l.kind === 'plate' ? '车牌' : '人脸'}</span></td>
        <td class="mono muted">${esc(l.target)}</td>
        <td>${l.count}</td>
        <td class="muted">${esc(l.last_seen || '')}</td>
        <td><label class="chip ${l.beep ? 'ok' : ''}" style="cursor:pointer">
          <input type="checkbox" ${l.beep ? 'checked' : ''} data-id="${l.id}" class="beepToggle" />
          ${l.beep ? '会鸣叫' : '不鸣叫'}
        </label></td>
        <td><button class="btn small danger" data-del="${l.id}">删除</button></td>
      </tr>`).join('');
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="8" class="muted">加载失败：${esc(e.message)}</td></tr>`;
  }
}

function updateLabelBatchBar() {
  const has = selectedLabels.size > 0;
  const btn = document.getElementById('btnDelLabels');
  btn.style.display = has ? 'inline-block' : 'none';
  if (has) btn.textContent = `删除选中 (${selectedLabels.size})`;
  if (!has) document.getElementById('selAllLabels').checked = false;
}

function updateBatchBar() {
  const has = selected.size > 0;
  document.getElementById('batchLabel').style.display = has ? 'block' : 'none';
  document.getElementById('btnBatchLabel').style.display = has ? 'inline-block' : 'none';
  document.getElementById('btnDelete').style.display = has ? 'inline-block' : 'none';
  if (has) document.getElementById('btnDelete').textContent = `删除选中 (${selected.size})`;
}

function renderPageNumbers(totalPages) {
  const el = document.getElementById('pageNumbers');
  if (totalPages <= 1) { el.innerHTML = ''; return; }
  let html = '';
  const maxShow = 7;
  let start = Math.max(1, page - 3);
  let end = Math.min(totalPages, start + maxShow - 1);
  if (end - start < maxShow - 1) start = Math.max(1, end - maxShow + 1);

  if (start > 1) {
    html += `<button class="btn small" onclick="goPage(1)">1</button>`;
    if (start > 2) html += `<span class="muted">…</span>`;
  }
  for (let i = start; i <= end; i++) {
    html += `<button class="btn small ${i === page ? 'primary' : ''}" onclick="goPage(${i})">${i}</button>`;
  }
  if (end < totalPages) {
    if (end < totalPages - 1) html += `<span class="muted">…</span>`;
    html += `<button class="btn small" onclick="goPage(${totalPages})">${totalPages}</button>`;
  }
  el.innerHTML = html;
}

function goPage(p) {
  page = p;
  loadIdentities();
}

async function loadIdentities() {
  const wrap = document.getElementById('cards');
  const q = document.getElementById('q').value.trim();
  try {
    const data = await api.get(`/api/identities?kind=${kind}&page=${page}&q=${encodeURIComponent(q)}`);
    let items = data.items || [];
    // ✅ 本地过滤：只显示未命名（labels 为空）
    if (onlyUnnamed) {
      items = items.filter(it => !(it.labels && it.labels.length));
    }
    // 服务端返回的 total 是过滤前的；本地过滤后重新算总数需要再跑一次不带 onlyUnnamed 的查询
    // 简化：前端分页不受 onlyUnnamed 影响，只过滤当前页内容
    const localTotal = onlyUnnamed ? (data.total - (data.total - items.length)) : data.total;
    total = data.total;
    const totalPages = Math.ceil(total / PAGE_SIZE);
    const filteredInfo = onlyUnnamed ? `· 未命名 ${items.length}/${localTotal}` : '';
    document.getElementById('pageInfo').textContent = `共 ${total} 个身份 · 第 ${page} 页${filteredInfo}`;
    document.getElementById('prev').disabled = page <= 1;
    document.getElementById('next').disabled = page >= totalPages;
    renderPageNumbers(totalPages);
    if (!items.length) {
      wrap.innerHTML = onlyUnnamed
        ? '<div class="note">当前页没有未命名的身份（翻页试试，或关闭“只显示未命名”）。</div>'
        : '<div class="note">没有匹配的身份。</div>';
      return;
    }
    // 批量换签名 URL
    const signedItems = await Promise.all(items.map(async it => ({
      ...it,
      _signedImage: await Auth.mediaUrl(it.image)
    })));
    wrap.innerHTML = signedItems.map(it => {
      const labels = it.labels || [];
      const attrs = kind === 'face' ? [
        it.gender === 'male' ? '男' : it.gender === 'female' ? '女' : '',
        it.age_range ? it.age_range + '岁' : '',
        it.glasses === 'yes' ? '戴眼镜' : it.glasses === 'no' ? '无眼镜' : ''
      ].filter(Boolean).join(' · ') : '';
      const checked = selected.has(it.event_id) ? 'checked' : '';
      return `
      <div class="card" data-eid="${it.event_id}">
        <a href="${it._signedImage}" target="_blank"><img class="cover" src="${it._signedImage}" alt="" /></a>
        <div class="body">
          <div class="name">${kind === 'plate' ? esc(it.plate_text || it.key) : (labels.length ? esc(labels.join('、')) : '未命名人脸')}</div>
          ${attrs ? `<div class="meta">${esc(attrs)}</div>` : ''}
          <div class="labels">${labels.map(n => `<span class="badge beep">${esc(n)}</span>`).join('')}</div>
          <div class="meta">出现 ${it.count} 次 · 最近 ${esc(it.last)}</div>
          <div class="meta">${esc(it.address || '无地址')}</div>
          <div class="form-row">
            <input class="input" style="flex:1; min-width:100px;" placeholder="命名，如 老爹" data-name="${it.event_id}" />
            <label class="chip ok" style="cursor:pointer"><input type="checkbox" checked data-beep="${it.event_id}" /> 鸣叫</label>
            <button class="btn small primary" data-save="${it.event_id}">保存</button>
          </div>
          <div class="meta mono" title="${esc(it.key)}">ID ${esc(it.key.slice(0, 18))}…</div>
          <div style="margin-top:6px;">
            <label style="cursor:pointer;font-size:12px;color:var(--muted);">
              <input type="checkbox" class="selBox" data-id="${it.event_id}" ${checked} /> 选择
            </label>
          </div>
        </div>
      </div>`;
    }).join('');
  } catch (e) {
    wrap.innerHTML = `<div class="note">加载失败：${esc(e.message)}</div>`;
  }
}

document.addEventListener('click', async ev => {
  const del = ev.target.closest('[data-del]');
  if (del) {
    if (!confirm('删除这个标签？')) return;
    try {
      await api.del('/api/labels/' + del.dataset.del);
      toast('标签已删除');
      loadLabels();
    } catch (e) { toast('删除失败：' + esc(e.message), 'err'); }
  }
  const save = ev.target.closest('[data-save]');
  if (save) {
    const id = save.dataset.save;
    const input = document.querySelector(`[data-name="${id}"]`);
    const beep = document.querySelector(`[data-beep="${id}"]`);
    const name = input.value.trim();
    if (!name) { toast('请先输入名字', 'err'); return; }
    try {
      await api.post('/api/labels', { name, event_id: id, beep: !!beep.checked });
      toast(`已标记为「${esc(name)}」`);
      loadLabels();
      loadIdentities();
    } catch (e) { toast('保存失败：' + esc(e.message), 'err'); }
  }
});

document.addEventListener('change', async ev => {
  const t = ev.target.closest('.beepToggle');
  if (!t) return;
  try {
    await api.patch('/api/labels/' + t.dataset.id, { beep: t.checked });
    toast(t.checked ? '已开启鸣叫' : '已关闭鸣叫');
    loadLabels();
  } catch (e) { toast('更新失败：' + esc(e.message), 'err'); }
});

// 多选（采集到的身份）
document.addEventListener('change', ev => {
  const box = ev.target.closest('.selBox');
  if (!box) return;
  const id = box.dataset.id;
  if (box.checked) selected.add(id);
  else selected.delete(id);
  updateBatchBar();
});

// 多选（已有标签）
document.addEventListener('change', ev => {
  const box = ev.target.closest('.labelSelBox');
  if (!box) return;
  const id = box.dataset.id;
  if (box.checked) selectedLabels.add(id);
  else selectedLabels.delete(id);
  updateLabelBatchBar();
});

// 全选（已有标签）
document.getElementById('selAllLabels').addEventListener('change', ev => {
  document.querySelectorAll('.labelSelBox').forEach(cb => {
    cb.checked = ev.target.checked;
    if (ev.target.checked) selectedLabels.add(cb.dataset.id);
    else selectedLabels.delete(cb.dataset.id);
  });
  updateLabelBatchBar();
});

// 批量删除标签
document.getElementById('btnDelLabels').addEventListener('click', async () => {
  if (!selectedLabels.size) return;
  if (!confirm(`确定删除选中的 ${selectedLabels.size} 个标签？`)) return;
  try {
    for (const id of selectedLabels) {
      await api.del('/api/labels/' + id);
    }
    toast(`已删除 ${selectedLabels.size} 个标签`);
    selectedLabels.clear();
    updateLabelBatchBar();
    loadLabels();
  } catch (e) { toast('删除失败：' + esc(e.message), 'err'); }
});

// 全选
document.getElementById('btnSelectAll').addEventListener('click', () => {
  const boxes = document.querySelectorAll('.selBox');
  const allChecked = selected.size === boxes.length && boxes.length > 0;
  boxes.forEach(cb => {
    cb.checked = !allChecked;
    if (!allChecked) selected.add(cb.dataset.id);
    else selected.delete(cb.dataset.id);
  });
  updateBatchBar();
});

// 批量打标
document.getElementById('btnBatchLabel').addEventListener('click', async () => {
  const name = document.getElementById('batchLabel').value.trim();
  if (!name) { toast('请输入标签名', 'err'); return; }
  if (!selected.size) { toast('请先选择头像', 'err'); return; }
  try {
    for (const id of selected) {
      await api.post('/api/labels', { name, event_id: id, beep: true });
    }
    toast(`已为 ${selected.size} 个身份打上「${esc(name)}」`);
    selected.clear();
    document.getElementById('batchLabel').value = '';
    updateBatchBar();
    loadLabels();
    loadIdentities();
  } catch (e) { toast('批量打标失败：' + esc(e.message), 'err'); }
});

// 批量删除
document.getElementById('btnDelete').addEventListener('click', async () => {
  if (!selected.size) return;
  if (!confirm(`确定删除选中的 ${selected.size} 条记录？此操作不可恢复。`)) return;
  try {
    const data = await api.post('/api/events/batch-delete', { ids: [...selected] });
    toast(`已删除 ${data.deleted} 条`);
    selected.clear();
    updateBatchBar();
    loadIdentities();
  } catch (e) { toast('删除失败：' + esc(e.message), 'err'); }
});

document.querySelectorAll('.tabs [data-kind]').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tabs [data-kind]').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    kind = btn.dataset.kind;
    page = 1;
    selected.clear();
    updateBatchBar();
    loadIdentities();
  });
});

document.getElementById('btnSearch').addEventListener('click', () => { page = 1; loadIdentities(); });
// ✅ 只显示未命名 checkbox
const onlyUnnamedCb = document.getElementById('onlyUnnamed');
if (onlyUnnamedCb) {
  onlyUnnamedCb.addEventListener('change', () => {
    onlyUnnamed = onlyUnnamedCb.checked;
    page = 1;
    loadIdentities();
  });
}
document.getElementById('q').addEventListener('keydown', e => { if (e.key === 'Enter') { page = 1; loadIdentities(); } });
document.getElementById('prev').addEventListener('click', () => { if (page > 1) { page--; loadIdentities(); } });
document.getElementById('next').addEventListener('click', () => { if (page * PAGE_SIZE < total) { page++; loadIdentities(); } });

loadLabels();
loadIdentities();
