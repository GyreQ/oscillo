// 轮次详情：一轮用户请求的完整链路。置顶用户输入，下面按时间序每次
// 模型调用一张可展开卡（默认收起，末次默认展开），展开体是旁路聚合的
// 回答（思考/文本/工具调用），卡脚跳完整回放。
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
    const sub = [
      F.datetime(t.time),
      !t.agent || t.agent === "unknown" ? '<span class="dim">未识别</span>' : F.esc(F.agentName(t.agent)),
      t.subagent ? "子代理" : "",
      t.session ? `<a href="#/tsession/${encodeURIComponent(t.session)}">所属会话</a>` : "",
    ].filter(Boolean).join(" · ");

    container.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <a class="backlink" href="#/requests">← 请求</a>
          <h1>${F.esc(t.text || "（无用户文本）")}</h1>
          <span class="sub">${sub}</span>
        </div>
        <div class="stats mb">
          <div class="kpi"><b>${t.calls}</b><span>模型调用</span></div>
          <div class="kpi"><b>${F.tok(u.input + u.cache_read + u.cache_write)}</b><span>输入（含缓存）</span></div>
          <div class="kpi"><b>${F.tok(u.output)}</b><span>输出</span></div>
          <div class="kpi"><b>${F.dur(t.durationMs)}</b><span>整轮耗时</span></div>
          ${(t.tools || []).length ? `<div class="kpi"><b>${t.tools.length}</b><span>${F.esc(t.tools.join("、"))}</span></div>` : ""}
        </div>
        ${t.errors ? `<div class="list mb"><div class="row err"><div class="who"><div class="name">这一轮有 ${t.errors} 次调用失败</div><div class="sub">失败卡片在下方标红，可展开看错误信息</div></div></div></div>` : ""}
        <div class="chain">
          ${calls.map((c, i) => callCard(c, i, calls.length)).join("") || '<div class="empty-state">这一轮没有模型调用记录</div>'}
        </div>
      </div>`;
  }

  // 一次模型调用卡：summary = 序号 · 模型 · 状态 · 耗时；展开体 = 聚合回答
  function callCard(c, i, total) {
    const r = c.record || {};
    const res = c.result;
    const err = !!(r.error || r.status >= 400);
    const out = r.usage ? r.usage.output : null;
    const last = i === total - 1;
    return `<details class="turn${err ? " call-err" : ""}"${last && !err ? " open" : ""}>
      <summary>
        <span class="t-chip">${F.esc(r.model || r.path || "调用 " + (i + 1))}</span>
        <span class="t-prev">${F.statusBadge(r.status, r.error)}
          ${F.time(r.time)} · ${F.dur(r.durationMs)}${r.ttftMs ? " · 首字节 " + F.dur(r.ttftMs) : ""}
          ${out != null ? " · 出 " + F.tok(out) : ""}</span>
        <span class="t-idx">#${i + 1}</span>
      </summary>
      <div class="t-body">
        ${res ? (R.resultBlock(res) || '<div class="dim" style="padding:6px 0">（无内容）</div>')
          : '<div class="dim" style="padding:6px 0">未解析的响应，原始内容见完整回放</div>'}
        <div class="call-foot">
          <a class="btn" href="#/replay/${encodeURIComponent(r.id)}">完整回放</a>
          <span class="sub">${F.esc(r.path || "")}</span>
        </div>
      </div>
    </details>`;
  }

  return { render };
})();
