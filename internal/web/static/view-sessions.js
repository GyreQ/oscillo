// 会话视图。两个数据源：
//   流量会话 —— 从录制账本按 session 分组合成（WorkBuddy / 豆包等全部被录制 agent）
//   日志会话 —— 本地日志（Claude Code / Codex）解析出的历史会话
// #/sessions 列表 · #/tsession/<id> 流量会话详情 · #/session/<agent>/<id> 日志会话回放
AI.views = AI.views || {};
AI.views.sessions = (() => {
  const F = AI.fmt;

  async function render(container) {
    container.innerHTML = `<div class="view-inner">
      <div class="view-h">
        <h1>会话</h1><span class="sub">流量按 session 聚合 · 本地日志解析的历史会话</span>
      </div>
      <div class="list"><div class="empty-state">加载中…</div></div></div>`;

    const [tresp, lresp] = await Promise.all([
      AI.api.tsessions().catch(() => ({ sessions: [] })),
      AI.api.sessions().catch(() => ({ sessions: [] })),
    ]);
    const ts = tresp.sessions || [];
    const ls = lresp.sessions || [];
    if (!ts.length && !ls.length) {
      container.querySelector(".list").innerHTML = `<div class="empty-state">
        <b>还没有会话</b>流量会话在录制请求后自动聚合；日志会话支持 Claude Code 与 Codex</div>`;
      return;
    }

    let html = "";
    if (ts.length) {
      html += `<div class="list">
        <div class="row" style="min-height:0;padding:9px 14px"><b style="font-size:12.5px">流量会话</b>
          <span class="sub">从录制的请求聚合 · ${ts.length} 个</span></div>` +
        ts.map(s => `
        <div class="row click" data-tsession="${F.esc(s.id)}">
          <div class="who">
            <div class="name">${s.title ? F.esc(s.title) : "（无标题会话）"}</div>
            <div class="sub">${s.agents.map(F.agentName).map(F.esc).join(" · ")}
              ${s.models.length ? " · " + F.esc(s.models.join(" / ")) : ""}
              · ${s.turns} 轮 · ${s.requests} 次调用 · ${F.datetime(s.firstSeen)} → ${F.datetime(s.lastSeen)}</div>
          </div>
          <div class="sess-num">
            <b>${F.tok(s.usage.input)} 入 / ${F.tok(s.usage.output)} 出</b>
          </div>
        </div>`).join("") + `</div>`;
    }
    if (ls.length) {
      html += `<div class="list ${ts.length ? "mt" : ""}">
        <div class="row" style="min-height:0;padding:9px 14px"><b style="font-size:12.5px">日志会话</b>
          <span class="sub">Claude Code / Codex 本地日志 · ${ls.length} 个</span></div>` +
        ls.map(s => `
        <div class="row click" data-agent="${F.esc(s.agent)}" data-id="${F.esc(s.id)}">
          <div class="who">
            <div class="name">${s.title ? F.esc(s.title) : "（无标题会话）"}</div>
            <div class="sub">${F.esc(F.agentName(s.agent))}${s.project ? " · " + F.esc(s.project.split("/").pop()) : ""}
              · ${s.turnCount} 轮 · ${F.ago(s.modTime)}</div>
          </div>
          <div class="sess-num">
            <b>${F.tok(s.totalUsage && s.totalUsage.input)} 入 / ${F.tok(s.totalUsage && s.totalUsage.output)} 出</b>
          </div>
        </div>`).join("") + `</div>`;
    }
    const listHost = container.querySelector(".list");
    listHost.outerHTML = html;

    container.querySelectorAll(".row[data-tsession]").forEach(item => {
      item.onclick = () => {
        location.hash = `#/tsession/${encodeURIComponent(item.dataset.tsession)}`;
      };
    });
    container.querySelectorAll(".row[data-id]").forEach(item => {
      item.onclick = () => {
        location.hash = `#/session/${encodeURIComponent(item.dataset.agent)}/${encodeURIComponent(item.dataset.id)}`;
      };
    });
  }

  return { render };
})();

// ---- 流量会话详情：会话内的用户请求轮次（点击进轮次链路） ----
AI.views.tsession = (() => {
  const F = AI.fmt;

  async function render(container, id) {
    container.innerHTML = `<div class="view-inner"><div class="empty-state">加载中…</div></div>`;
    let d;
    try {
      d = await AI.api.tsession(id);
    } catch (e) {
      container.innerHTML = `<div class="view-inner"><div class="empty-state"><b>会话加载失败</b>${F.esc(e.message)}</div></div>`;
      return;
    }
    const s = d.session || {};
    const turns = d.turns || [];
    const u = s.usage || {};
    const calls = turns.reduce((n, t) => n + (t.calls || 0), 0);
    container.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <a class="backlink" href="#/sessions">← 会话</a>
          <h1>${s.title ? F.esc(s.title) : "流量会话"}</h1>
          <span class="sub">${(s.agents || []).map(F.agentName).map(F.esc).join(" · ")} · ${s.turns} 轮 · ${calls} 次调用</span>
        </div>
        <div class="stats mb">
          <div class="kpi"><b>${F.tok(u.input)}</b><span>累计输入</span></div>
          <div class="kpi"><b>${F.tok(u.cache_read)}</b><span>缓存读</span></div>
          <div class="kpi"><b>${F.tok(u.output)}</b><span>累计输出</span></div>
          <div class="kpi"><b>${s.turns}</b><span>用户请求</span></div>
          <div class="kpi"><b>${calls}</b><span>模型调用</span></div>
        </div>
        <div class="list">
          <div class="row" style="min-height:0;padding:9px 14px"><b style="font-size:12.5px">用户请求</b>
            <span class="sub">按时间降序 · 一轮 = 一次用户输入 · 点击看整轮链路</span></div>
          ${turns.map(t => `
          <div class="row click${t.errors ? " err" : ""}" data-turn="${F.esc(t.id)}">
            <div class="who">
              <div class="name">${F.esc(t.text || "（无用户文本）")}${t.subagent ? ' <span class="badge">子代理</span>' : ""}</div>
              <div class="sub">${F.datetime(t.time)} · ${t.calls} 次调用${(t.tools || []).length ? " · " + F.esc(t.tools.join("、")) : ""}</div>
            </div>
            <div class="sess-num">
              ${t.errors ? `<span class="badge err">${t.errors} 次失败</span>` : ""}
              <b>${F.tok((t.usage || {}).input)} 入 / ${F.tok((t.usage || {}).output)} 出</b>
              <div class="sub">${F.dur(t.durationMs)}</div>
            </div>
          </div>`).join("") || '<div class="empty-state">无用户请求</div>'}
        </div>
      </div>`;
    container.querySelectorAll(".row[data-turn]").forEach(item => {
      item.onclick = () => {
        location.hash = `#/turn/${encodeURIComponent(item.dataset.turn)}`;
      };
    });
  }

  return { render };
})();

// ---- 单会话回放 ----
AI.views.session = (() => {
  const F = AI.fmt, R = AI.render;

  async function render(container, agent, id) {
    container.innerHTML = `<div class="view-inner"><div class="empty-state">加载中…</div></div>`;
    let s;
    try {
      s = await AI.api.session(agent, id);
    } catch (e) {
      container.innerHTML = `<div class="view-inner"><div class="empty-state"><b>会话加载失败</b>${F.esc(e.message)}</div></div>`;
      return;
    }

    const total = s.totalUsage || {};
    container.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          <a class="backlink" href="#/sessions">← 会话</a>
          <h1>${s.title ? F.esc(s.title) : "会话回放"}</h1>
          <span class="sub">${F.esc(F.agentName(s.agent))}${s.project ? " · " + F.esc(s.project) : ""} · ${s.turnCount} 轮</span>
        </div>
        <div class="stats mb">
          <div class="kpi"><b>${F.tok(total.input)}</b><span>累计输入</span></div>
          <div class="kpi"><b>${F.tok(total.output)}</b><span>累计输出</span></div>
          <div class="kpi"><b>${(s.entries || []).length}</b><span>日志条目</span></div>
        </div>
        <div class="flow">${(s.entries || []).map(entryHTML).join("") || '<div class="empty-state">无条目</div>'}</div>
      </div>`;
  }

  function entryHTML(e) {
    const cls = e.role === "user" ? "user" : "assistant";
    let inner = "";
    if (e.thinking) inner += R.thinkBlock(e.thinking);
    if (e.text) inner += R.textBlock(e.text);
    for (const tu of (e.toolUses || [])) {
      inner += R.toolCard(tu.name, typeof tu.args === "string" ? tu.args : JSON.stringify(tu.args || {}));
    }
    for (const tr of (e.toolResults || [])) {
      inner += R.toolCard("工具结果", null, { content: tr.content || "", isError: tr.isError });
    }
    const badges = [];
    if (e.sidechain) badges.push('<span class="badge">子代理</span>');
    if (e.model) badges.push(`<span class="badge">${F.esc(e.model)}</span>`);
    if (e.usage && e.usage.output) {
      badges.push(`<span class="badge">+${F.tok(e.usage.output)} 出</span>`);
    }
    if (!inner && !badges.length) return "";
    return `<div class="msg ${cls}">
      <div class="msg-role"><span class="name">${F.time(e.time)} ${F.esc(e.role)}</span>${badges.join("")}</div>
      ${inner || '<div class="dim" style="font-size:11.5px">（无文本内容）</div>'}
    </div>`;
  }

  return { render };
})();
