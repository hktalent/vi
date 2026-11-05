function initDates() {
  const today = new Date();
  const to = today.toISOString().slice(0, 10);
  const fromDate = new Date(today.getTime() - 6 * 24 * 3600 * 1000);
  const from = fromDate.toISOString().slice(0, 10);
  document.getElementById('from').value = from;
  document.getElementById('to').value = to;
  document.getElementById('summaryDate').value = to;
}

async function query() {
  const from = document.getElementById('from').value;
  const to = document.getElementById('to').value;
  if (!from || !to) { toast('请选择起止日期', 'err'); return; }
  try {
    const d = await api.get(`/api/stats?from=${from}&to=${to}`);
    document.getElementById('sFaces').textContent = d.faces;
    document.getElementById('sPlates').textContent = d.plates;
    document.getElementById('sPersons').textContent = d.persons;
    document.getElementById('sUniquePlates').textContent = d.unique_plates;
    renderBars(d.daily);
    document.getElementById('labeledInfo').textContent =
      `${d.from} ~ ${d.to} · 其中带标签记录 ${d.labeled_events} 条`;
  } catch (e) {
    toast('查询失败：' + esc(e.message), 'err');
  }
}

function renderBars(daily) {
  const wrap = document.getElementById('bars');
  if (!daily || !daily.length) { wrap.innerHTML = '<div class="note">暂无数据</div>'; return; }
  const max = Math.max(1, ...daily.map(d => Math.max(d.faces, d.plates)));
  wrap.innerHTML = daily.map(d => {
    const hf = Math.round((d.faces / max) * 100);
    const hp = Math.round((d.plates / max) * 100);
    return `<div class="bar-col" title="${d.date} · 人脸 ${d.faces} / 车牌 ${d.plates}">
      <div class="plate" style="height:${hp}%"></div>
      <div class="face" style="height:${hf}%"></div>
      <div class="cap">${d.date.slice(5)}</div>
    </div>`;
  }).join('');
}

async function loadReports() {
  const tbody = document.getElementById('reportRows');
  try {
    const d = await api.get('/api/summary/list');
    if (!d.items.length) {
      tbody.innerHTML = '<tr><td colspan="3" class="muted">还没有总结文档。</td></tr>';
      return;
    }
    tbody.innerHTML = d.items.map(it => `
      <tr>
        <td><a href="${it.url}" target="_blank">${esc(it.name)}</a></td>
        <td class="muted">${esc(it.mtime)}</td>
        <td class="muted">${(it.size / 1024).toFixed(1)} KB</td>
      </tr>`).join('');
  } catch (e) {
    tbody.innerHTML = `<tr><td colspan="3" class="muted">加载失败：${esc(e.message)}</td></tr>`;
  }
}

document.getElementById('btnQuery').addEventListener('click', query);
document.getElementById('btnSummary').addEventListener('click', async () => {
  const date = document.getElementById('summaryDate').value;
  try {
    const d = await api.post('/api/summary/run?date=' + encodeURIComponent(date), {});
    toast(`已生成 <a href="${d.url}" target="_blank">${esc(d.file)}</a>`);
    loadReports();
  } catch (e) {
    toast('生成失败：' + esc(e.message), 'err');
  }
});

initDates();
query();
loadReports();
