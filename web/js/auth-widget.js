// auth-widget.js —— 顶部登录态 UI（所有页面共用）
// 引入前必须先引入 common.js
(function(){
  if (!window.Auth) return;

  // 给 .auth 区域加样式（inline，避免改 style.css 影响太多）
  const styleText = `
    .auth { display: flex; align-items: center; margin-left: auto; gap: 6px; }
    .auth .btn-auth {
      padding: 6px 12px; font-size: 13px; border-radius: 8px;
      background: var(--panel-2, #1f2524); border: 1px solid var(--border);
      color: var(--text); cursor: pointer;
    }
    .auth .btn-auth:hover { background: var(--accent); color: #08120f; }
    .auth .btn-auth.btn-logout:hover { background: #c0392b; color: #fff; }
    .auth .user-chip {
      display: flex; align-items: center; gap: 6px; padding: 4px 10px 4px 4px;
      background: var(--panel-2, #1f2524); border: 1px solid var(--border);
      border-radius: 20px; font-size: 13px; cursor: pointer;
      position: relative;
    }
    .auth .user-chip:hover { border-color: var(--accent); }
    .auth .avatar {
      width: 26px; height: 26px; border-radius: 50%;
      background: var(--accent); color: #08120f; font-weight: 700;
      display: flex; align-items: center; justify-content: center;
      font-size: 13px;
    }
    .auth .username { max-width: 110px; overflow: hidden; text-overflow: ellipsis; }
    .auth .caret { font-size: 10px; opacity: .6; }
    .auth .dropdown {
      display: none; position: absolute; top: calc(100% + 6px); right: 0;
      background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
      min-width: 160px; box-shadow: 0 8px 24px rgba(0,0,0,.3); z-index: 1000;
    }
    .auth .user-chip.open .dropdown { display: block; }
    .auth .dropdown-item {
      padding: 8px 14px; cursor: pointer; font-size: 13px;
    }
    .auth .dropdown-item:hover { background: var(--panel-2); }
    .auth .dropdown-item.danger { color: #ff8a8a; }
    .auth .dropdown hr { border: none; border-top: 1px solid var(--border); margin: 4px 0; }
    /* 修改密码弹窗 */
    .pw-modal-mask {
      position: fixed; inset: 0; background: rgba(0,0,0,.55);
      display: flex; align-items: center; justify-content: center; z-index: 2000;
    }
    .pw-modal {
      background: var(--panel); border: 1px solid var(--border); border-radius: 14px;
      padding: 24px 22px; width: 320px; box-shadow: 0 12px 40px rgba(0,0,0,.4);
    }
    .pw-modal h3 { margin: 0 0 14px; font-size: 16px; }
    .pw-modal label { display: block; font-size: 12px; color: var(--muted); margin: 8px 0 4px; }
    .pw-modal input {
      width: 100%; padding: 8px 10px; font-size: 14px;
      background: var(--bg-2, #181d1c); border: 1px solid var(--border);
      border-radius: 7px; color: var(--text); outline: none; box-sizing: border-box;
    }
    .pw-modal input:focus { border-color: var(--accent); }
    .pw-modal .row { display: flex; gap: 8px; margin-top: 16px; }
    .pw-modal .row button { flex: 1; padding: 8px; font-size: 13px; }
    .pw-modal .err { color: #ff6f6f; font-size: 12px; margin-top: 6px; min-height: 16px; }
    .pw-modal .ok { color: #6affa6; font-size: 12px; margin-top: 6px; }
    @media (max-width: 600px) {
      .auth .username { display: none; }
    }
  `;
  const s = document.createElement('style');
  s.textContent = styleText;
  document.head.appendChild(s);

  function render() {
    const host = document.getElementById('authArea');
    if (!host) return;
    host.innerHTML = '';
    if (!Auth.isLoggedIn()) {
      const btn = document.createElement('button');
      btn.className = 'btn-auth';
      btn.textContent = '登 录';
      btn.onclick = () => location.href = '/login.html?next=' + encodeURIComponent(location.pathname + location.search);
      host.appendChild(btn);
      return;
    }
    // 已登录：头像 + 用户名 + 修改密码 + 退出登录（全部可见）
    const avatar = document.createElement('div');
    avatar.className = 'avatar';
    avatar.textContent = (Auth._meCache?.username || Auth.token.slice(0,2)).toUpperCase().slice(0,1);
    const name = document.createElement('span');
    name.className = 'username';
    name.textContent = Auth._meCache?.username || '已登录';
    host.append(avatar, name);

    const btnChange = document.createElement('button');
    btnChange.className = 'btn-auth';
    btnChange.textContent = '修改密码';
    btnChange.onclick = () => openChangePwd();
    host.appendChild(btnChange);

    const btnLogout = document.createElement('button');
    btnLogout.className = 'btn-auth btn-logout';
    btnLogout.textContent = '退出登录';
    btnLogout.onclick = async () => {
      await Auth.logout();
      location.href = '/login.html';
    };
    host.appendChild(btnLogout);
  }

  // 修改密码弹窗
  function openChangePwd() {
    if (document.querySelector('.pw-modal-mask')) return;
    const mask = document.createElement('div');
    mask.className = 'pw-modal-mask';
    mask.innerHTML = `
      <div class="pw-modal">
        <h3>修改密码</h3>
        <label>原密码</label><input id="cpOld" type="password" />
        <label>新密码（至少 6 位）</label><input id="cpNew1" type="password" />
        <label>确认新密码</label><input id="cpNew2" type="password" />
        <div class="err" id="cpErr"></div>
        <div class="ok" id="cpOk"></div>
        <div class="row">
          <button class="btn" id="cpCancel">取消</button>
          <button class="btn primary" id="cpSubmit">确认修改</button>
        </div>
      </div>`;
    document.body.appendChild(mask);
    const $old = mask.querySelector('#cpOld');
    const $n1 = mask.querySelector('#cpNew1');
    const $n2 = mask.querySelector('#cpNew2');
    const $err = mask.querySelector('#cpErr');
    const $ok = mask.querySelector('#cpOk');
    const $submit = mask.querySelector('#cpSubmit');
    mask.querySelector('#cpCancel').onclick = () => mask.remove();
    mask.onclick = e => { if (e.target === mask) mask.remove(); };
    $old.focus();

    $submit.onclick = async () => {
      $err.textContent = ''; $ok.textContent = '';
      const oldP = $old.value, n1 = $n1.value, n2 = $n2.value;
      if (!oldP || !n1 || !n2) { $err.textContent = '请全部填写'; return; }
      if (n1 !== n2) { $err.textContent = '两次新密码不一致'; return; }
      if (n1.length < 6) { $err.textContent = '新密码至少 6 位'; return; }
      $submit.disabled = true;
      try {
        await Auth.changePassword(oldP, n1);
        $ok.textContent = '✓ 密码已更新，将在 2 秒后跳登录';
        setTimeout(() => { Auth.token = ''; location.href = '/login.html'; }, 1500);
      } catch (e) {
        $err.textContent = e.message || '修改失败';
      } finally { $submit.disabled = false; }
    };
  }

  // 启动时 fetch /api/me 更新用户信息，失败则 token 作废
  (async function init() {
    if (Auth.isLoggedIn()) {
      const me = await Auth.me();
      Auth._meCache = me || null;
      if (!me) render(); // token 已被 me() 作废
      else render();
    } else {
      render();
    }
  })();
})();
