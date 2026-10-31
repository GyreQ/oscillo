// 上下文 diff：同一会话的两个请求并排对比，看上下文怎么膨胀、
// 哪些消息是新增的、缓存命中怎么变化。
//
// 对齐算法：消息级 LCS（内容签名相等视为同一消息）。
AI.views = AI.views || {};
AI.views.diff = (() => {
  const F = AI.fmt, R = AI.render;
  const state = { day: "", session: "", a: "", b: "" };
  let el = null, recsList = [];

  async function render(container, params) {
    el = container;
    state.day = params.get("day") || F.today();
    state.session = params.get("session") || "";
    if (!state.session) {
      el.innerHTML = `<div class="view-inner"><div class="empty-state"><b>需要先选一个会话</b>
        从「请求」列表或回放器侧栏的「对比此会话的请求」进入</div></div>`;
      return;
    }
    const resp = await AI.api.records({ day: state.day, session: state.session })
      .catch(() => ({ records: [] }));
    recsList = resp.records || [];
    if (recsList.length < 2) {
      el.innerHTML = `<div class="view-inner"><div class="empty-state"><b>这个会话只有 ${recsList.length} 个请求，没法对比</b>
        会话：${F.esc(state.session)}</div></div>`;
      return;
    }
    // 默认对比最后两个（最新的两个请求最能看出膨胀）
    if (!state.b) state.b = recsList[0].id;
    if (!state.a) state.a = recsList[1].id;
    if (state.a === state.b) {
      const other = recsList.find(r => r.id !== state.b);
      if (other) state.a = other.id;
    }
    renderSelectors();
    await renderDiff();
  }

  function renderSelectors() {
    const opt = (r) => {
      const u = r.usage;
      const p = u ? u.input + u.cache_read + u.cache_write : 0;
      return `#${F.time(r.time)} · ${F.tok(p)} 输入${r.error ? " · 错误" : ""} (${r.id.slice(9, 15)})`;
    };
    el.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <a class="backlink" href="#/requests">← 请求</a>
          <h1>上下文对比</h1>
          <span class="sub">绿色 = B 新增，红色 = 只在 A 里</span>
        </div>
        <div class="card mb"><div class="card-b">
          <div class="filters" style="margin:0">
            <select id="selA">${recsList.map(r => `<option value="${r.id}" ${r.id === state.a ? "selected" : ""}>A · ${F.esc(opt(r))}</option>`).join("")}</select>
            <span class="dim" style="font-size:12px">对比</span>
            <select id="selB">${recsList.map(r => `<option value="${r.id}" ${r.id === state.b ? "selected" : ""}>B · ${F.esc(opt(r))}</option>`).join("")}</select>
            <button class="btn primary" id="swapAB">交换 A / B</button>
          </div>
        </div></div>
        <div id="diffBody"><div class="empty-state">加载中…</div></div>
      </div>`;
    el.querySelector("#selA").onchange = (e) => { state.a = e.target.value; renderDiff(); };
    el.querySelector("#selB").onchange = (e) => { state.b = e.target.value; renderDiff(); };
    el.querySelector("#swapAB").onclick = () => {
      [state.a, state.b] = [state.b, state.a];
      el.querySelector("#selA").value = state.a;
      el.querySelector("#selB").value = state.b;
      renderDiff();
    };
  }

  async function renderDiff() {
    const box = el.querySelector("#diffBody");
    if (!box) return;
    if (state.a === state.b) {
      box.innerHTML = `<div class="empty-state"><b>请选择两个不同的请求</b></div>`;
      return;
    }
    const [da, db] = await Promise.all([AI.api.record(state.a), AI.api.record(state.b)]);
    const A = parse(da), B = parse(db);
    if (!A || !B) {
      box.innerHTML = `<div class="empty-state"><b>请求体解析失败</b></div>`;
      return;
    }
    align(A.msgs, B.msgs); // LCS 对齐打 same/add/del 标记

    // 汇总条
    const ua = da.record.usage || {}, ub = db.record.usage || {};
    const pa = (ua.input || 0) + (ua.cache_read || 0) + (ua.cache_write || 0);
    const pb = (ub.input || 0) + (ub.cache_read || 0) + (ub.cache_write || 0);
    box.innerHTML = `
      <div class="stats mb">
        ${deltaCard("消息数", A.msgs.length, B.msgs.length)}
        ${deltaCard("输入 token", pa, pb)}
        ${deltaCard("缓存读", ua.cache_read || 0, ub.cache_read || 0)}
        ${deltaCard("输出 token", ua.output || 0, ub.output || 0)}
      </div>
      <div class="diff-wrap">
        <div class="diff-col"><div class="diff-col-h"><span>A · ${F.time(da.record.time)}</span><span class="mono">${F.esc(da.record.model || "")}</span></div>${colHTML(A.msgs)}</div>
        <div class="diff-col"><div class="diff-col-h"><span>B · ${F.time(db.record.time)}</span><span class="mono">${F.esc(db.record.model || "")}</span></div>${colHTML(B.msgs)}</div>
      </div>`;

    box.querySelectorAll("[data-id]").forEach(node => {
      node.onclick = () => { location.hash = "#/replay/" + node.dataset.id; };
    });
  }

  function deltaCard(label, va, vb) {
    const d = vb - va;
    const up = d > 0;
    const pct = va > 0 ? Math.round(d / va * 100) : (d > 0 ? 100 : 0);
    return `<div class="kpi"><b>${F.tok(vb)}</b><span>${label}</span>
      <small class="${d === 0 ? "" : up ? "warn" : "up"}">${d === 0 ? "无变化" :
        `${up ? "↑" : "↓"} ${F.tok(Math.abs(d))}（${pct}%）vs A 的 ${F.tok(va)}`}</small></div>`;
  }

  // 一列消息：LCS 对齐后标 same / add / del
  function colHTML(msgs) {
    return msgs.map(m => {
      const cls = m.__diff === "same" ? "same" : m.__diff === "add" ? "add" : "del";
      const role = (m.role || "?").toUpperCase();
      const preview = msgPreview(m).slice(0, 220);
      return `<div class="diff-item ${cls}" data-id="${m.__id || ""}">
        <span class="role-tag">${role}</span><span>${F.esc(preview)}${msgPreview(m).length > 220 ? "…" : ""}</span>
      </div>`;
    }).join("") || `<div class="empty-state">无消息</div>`;
  }

  function msgPreview(m) {
    const blocks = R.contentBlocks(m);
    const parts = [];
    for (const b of blocks) {
      if (b.type === "text" && b.text) parts.push(b.text.split("\n")[0]);
      else if (b.type === "tool_use") parts.push(`〔调用 ${b.name}〕`);
      else if (b.type === "tool_result") parts.push("〔工具结果〕");
      else if (b.type === "thinking") parts.push("〔思考〕");
    }
    if (Array.isArray(m.tool_calls)) {
      for (const tc of m.tool_calls) parts.push(`〔调用 ${tc.function && tc.function.name}〕`);
    }
    if (m.role === "tool") parts.push("〔工具结果〕");
    return parts.join(" · ") || "（空消息）";
  }

  // 请求解析：messages + __diff 标记（LCS 对齐）。
  // requestBody 可能是内嵌 JSON 对象（后端 json.RawMessage）也可能是字符串。
  function parse(data) {
    if (!data.body) return null;
    const raw = data.body.requestBody;
    let req = null;
    if (raw != null && typeof raw === "object") req = raw;
    else if (typeof raw === "string" && raw) { try { req = JSON.parse(raw); } catch { return null; } }
    if (!req || typeof req !== "object") return null;
    const msgs = req.messages || [];
    return { req, msgs };
  }

  // LCS 对齐两列消息并打标记（原地）
  function align(A, B) {
    const sig = (m) => m.role + "|" + JSON.stringify(m.content) + "|" + JSON.stringify(m.tool_calls || null);
    const sa = A.map(sig), sb = B.map(sig);
    const n = sa.length, m = sb.length;
    // dp[i][j] = A[i:] 与 B[j:] 的最长公共子序列长度
    const dp = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        dp[i][j] = sa[i] === sb[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
    let i = 0, j = 0;
    const marksA = [], marksB = [];
    while (i < n && j < m) {
      if (sa[i] === sb[j]) { marksA.push("same"); marksB.push("same"); i++; j++; }
      else if (dp[i + 1][j] >= dp[i][j + 1]) { marksA.push("del"); i++; }
      else { marksB.push("add"); j++; }
    }
    while (i < n) { marksA.push("del"); i++; }
    while (j < m) { marksB.push("add"); j++; }
    A.forEach((msg, k) => msg.__diff = marksA[k]);
    B.forEach((msg, k) => msg.__diff = marksB[k]);
  }

  // 成本对比需要逐请求价格数据（API 暂未提供单条成本），先展示 token 维度
  return { render, align };
})();
