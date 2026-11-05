let kind = 'face';
let page = 1;
let total = 0;
const PAGE_SIZE = 20;

async function loadLabels() {
  const tbody = document.getElementById('labelRows');
  try {
    const data = await api.get('/api/labels');
    if (!data.items.length) {
      tbody.innerHTML = '<tr><td colspan="7" class="muted">还没有标签，先在下面的身份卡片里命名。</td></tr>';
      return;
    }
    tbody.innerHTML = data.items.map(l => `
      <tr>
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
    tbody.innerHTML = `<tr><td colspan="7" class="muted">加载失败：${esc(e.message)}</td></tr>`;
  }
}

async function loadIdentities() {
  const wrap = document.getElementById('cards');
  const q = document.getElementById('q').value.trim();
  try {
    const data = await api.get(`/api/identities?kind=${kind}&page=${page}&q=${encodeURIComponent(q)}`);
    total = data.total;
    document.getElementById('pageInfo').textContent = `共 ${total} 个身份 · 第 ${page} 页`;
    document.getElementById('prev').disabled = page <= 1;
    document.getElementById('next').disabled = page * PAGE_SIZE >= total;
    if (!data.items.length) {
      wrap.innerHTML = '<div class="note">没有匹配的身份。</div>';
      return;
    }
    wrap.innerHTML = data.items.map(it => {
      const labels = it.labels || [];
      const attrs = kind === 'face' ? [
        it.gender === 'male' ? '男' : it.gender === 'female' ? '女' : '',
        it.age_range ? it.age_range + '岁' : '',
        it.glasses === 'yes' ? '戴眼镜' : it.glasses === 'no' ? '无眼镜' : ''
      ].filter(Boolean).join(' · ') : '';
      return `
      <div class="card">
        <a href="${Auth.mediaUrl(it.image)}" target="_blank"><img class="cover" src="${Auth.mediaUrl(it.image)}" alt="" /></a>
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

document.querySelectorAll('.tabs [data-kind]').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tabs [data-kind]').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    kind = btn.dataset.kind;
    page = 1;
    loadIdentities();
  });
});

document.getElementById('btnSearch').addEventListener('click', () => { page = 1; loadIdentities(); });
document.getElementById('q').addEventListener('keydown', e => { if (e.key === 'Enter') { page = 1; loadIdentities(); } });
document.getElementById('prev').addEventListener('click', () => { if (page > 1) { page--; loadIdentities(); } });
document.getElementById('next').addEventListener('click', () => { if (page * PAGE_SIZE < total) { page++; loadIdentities(); } });

loadLabels();
loadIdentities();
