// 轮次详情：一轮用户请求的完整链路。用户输入以对话气泡置顶，下面按时间
// 序每次模型调用一张卡：整卡可点进单次请求详情（?turn= 参数让详情页能
// 返回本页），右上角小箭头只负责展开/收起，两种意图互不打架。
AI.views = AI.views || {};
AI.views.turn = (() => {
  const F = AI.fmt;
  const R = AI.render;

  async function render(container, id) {
    container.innerHTML = `<div class="view-inner"><div class="empty-state">加载中…</div></div>`;
    let d;
    try {
      d = await AI.api.turn(id);
    } catch (e) {
      container.innerHTML = `<div class="view-inner"><div class="empty-state"><b>轮次加载失败</b>${F.esc(e.message || e)}</div></div>`;
      return;
    }
    const t = d.turn || {};
    const calls = d.calls || [];
    const u = t.usage || {};
    const inTok = (u.input || 0) + (u.cache_read || 0) + (u.cache_write || 0);
    const agent = !t.agent || t.agent === "unknown" ? "未识别" : F.agentName(t.agent);

    container.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <a class="backlink" href="#/requests">← 请求</a>
          <h1>用户请求 · ${F.esc(agent)}${t.subagent ? ' <span class="badge">子代理</span>' : ""}</h1>
          <span class="sub">${F.datetime(t.time)}${t.session ? ` · <a href="#/tsession/${encodeURIComponent(t.session)}">所属会话</a>` : ""}</span>
        </div>
        <div class="flow mb">
          <div class="msg user">
            <div class="msg-role"><span class="name">用户输入</span>
              <span class="badges">${t.errors ? `<span class="badge err">${t.errors} 次调用失败</span>` : ""}</span>
            </div>
            <div class="msg-text">${F.esc(t.text || "（无用户文本，可能为未识别流量）")}</div>
          </div>
        </div>
        <div class="stats mb">
          <div class="kpi"><b>${t.calls}</b><span>模型调用</span></div>
          <div class="kpi"><b>${F.tok(inTok)}</b><span>输入（含缓存）</span></div>
          <div class="kpi"><b>${F.tok(u.output)}</b><span>输出</span></div>
          <div class="kpi"><b>${F.dur(t.durationMs)}</b><span>整轮耗时</span></div>
        </div>
        <div class="turn-bar">
          <b style="font-size:12.5px">模型调用链</b>
          <span class="dim" style="font-size:11.5px">最新在前</span>
          ${(t.tools || []).map(x => `<span class="badge">${F.esc(x)}</span>`).join("")}
          <span style="margin-left:auto;display:inline-flex;gap:6px">
            <button type="button" class="btn sm" data-chain="open">展开全部</button>
            <button type="button" class="btn sm" data-chain="close">收起全部</button>
          </span>
        </div>
        <div class="chain">
          ${calls.map((c, i) => callCard(c, i, calls.length, t.id)).reverse().join("") || '<div class="empty-state">这一轮没有模型调用记录</div>'}
        </div>
      </div>`;

    // 整卡点击 → 单次请求详情（右上角箭头除外，它只展开/收起）。
    // 只绑外层卡片：展开体里的思考过程折叠块也是 details.turn，不能误伤。
    container.querySelectorAll(".chain > details.turn > summary").forEach(s => {
      s.addEventListener("click", (e) => {
        if (e.target.closest(".t-caret")) return;
        e.preventDefault();
        const det = s.closest("details");
        location.hash = `#/replay/${encodeURIComponent(det.dataset.id)}?turn=${encodeURIComponent(t.id)}`;
      });
    });
    container.querySelectorAll("[data-chain]").forEach(b => {
      b.onclick = () => {
        container.querySelectorAll(".chain > details").forEach(x => { x.open = b.dataset.chain === "open"; });
      };
    });
  }

  // 一次模型调用卡：#序号 · 模型 · 状态/耗时；展开体 = 旁路聚合的回答
  function callCard(c, i, total, turnId) {
    const r = c.record || {};
    const res = c.result;
    const err = !!(r.error || r.status >= 400);
    const out = r.usage ? r.usage.output : null;
    const last = i === total - 1;
    const tail = err
      ? `<span class="badge err">${F.esc(String(r.error || "请求失败").slice(0, 46))}</span>`
      : (res && res.stopReason ? `<span class="badge">${F.esc(F.stopReason(res.stopReason))}</span>` : "");
    return `<details class="turn call${err ? " call-err" : ""}" data-id="${F.esc(r.id)}"${last && !err ? " open" : ""}>
      <summary>
        <span class="t-chip c-idx">#${i + 1}</span>
        <span class="c-model mono">${F.esc(r.model || r.path || "—")}</span>
        <span class="c-meta">${F.statusBadge(r.status)} ${F.time(r.time)} · ${F.dur(r.durationMs)}${r.ttftMs ? ` · 首字节 ${F.dur(r.ttftMs)}` : ""}${out != null ? ` · 出 ${F.tok(out)}` : ""}</span>
        <span style="flex:1"></span>
        ${tail}
        <span class="t-caret"></span>
      </summary>
      <div class="t-body">
        ${res ? (R.resultBlock(res) || '<div class="dim" style="padding:6px 0">（无内容）</div>')
          : '<div class="dim" style="padding:6px 0">未解析的响应，点卡片进详情看原始内容</div>'}
      </div>
    </details>`;
  }

  return { render };
})();
