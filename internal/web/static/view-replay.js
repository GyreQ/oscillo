// 请求回放器「检查器」：上下文区（system/tools 折叠卡带预览）→ 对话流 → 右侧元数据。
// 公共渲染器挂在 AI.render 供 diff / 会话视图复用。
AI.views = AI.views || {};
AI.render = (() => {
  const F = AI.fmt;

  // ---- 小块渲染 ----

  function thinkBlock(text, open) {
    if (!text) return "";
    return `<details class="think"${open ? " open" : ""}>
      <summary>思考过程 · ${F.tok(text.length)} 字</summary>
      <div class="think-body">${F.esc(text)}</div>
    </details>`;
  }

  function textBlock(text) {
    return text ? `<div class="msg-text">${F.esc(text)}</div>` : "";
  }

  // 工具卡：名字 + 参数（折叠带预览）+ 结果（折叠带预览，短的自动展开）
  function toolCard(name, args, result) {
    const err = result && result.isError;
    let out = `<div class="tool-card"><div class="tc-head">
      <span class="tc-name">${F.esc(name || "工具")}</span>
      ${err ? '<span class="badge err">出错</span>' : ""}
    </div>`;
    if (args != null && args !== "") {
      const argsStr = typeof args === "string" ? args : JSON.stringify(args);
      out += collapsible("参数", F.prettyJSON(argsStr), false);
    }
    if (result) {
      const rc = typeof result.content === "string" ? result.content
        : Array.isArray(result.content)
          ? result.content.map(c => c.text || JSON.stringify(c)).join("\n") : "";
      out += collapsible("结果", rc.length > 4000 ? rc.slice(0, 4000) + "\n…（截断）" : rc,
        rc.length && rc.length <= 200); // 短结果直接展开
    }
    return out + `</div>`;
  }

  // 参数/结果共用的折叠块：summary 带单行预览
  function collapsible(label, content, open) {
    if (content == null || content === "") return "";
    const preview = content.split("\n").find(l => l.trim()) || "";
    return `<details${open ? " open" : ""}><summary>${label}<span class="pv">${F.esc(preview.slice(0, 110))}</span></summary>
      <pre class="${label === "结果" ? "tool-result" : "tool-args"}">${F.esc(content)}</pre></details>`;
  }

  // 把消息 content 归一化成块数组
  function contentBlocks(m) {
    const c = m.content;
    if (typeof c === "string") return [{ type: "text", text: c }];
    if (Array.isArray(c)) return c;
    return [];
  }

  // ---- 对话流：请求 messages → HTML ----
  // toolNames: tool_use id → name 映射（工具结果块要显示工具名）
  function messageBlock(m, toolNames) {
    const role = m.role || "?";
    if (role === "tool") { // OpenAI 工具结果
      return toolCard(toolNames[m.tool_call_id] || m.name || "工具",
        null, { content: m.content, isError: false });
    }
    const blocks = contentBlocks(m);
    let inner = "";
    for (const b of blocks) {
      if (b.type === "text") inner += textBlock(b.text);
      else if (b.type === "thinking") inner += thinkBlock(b.thinking || b.text);
      else if (b.type === "tool_use") inner += toolCard(b.name, JSON.stringify(b.input || {}));
      else if (b.type === "tool_result") {
        inner += toolCard(toolNames[b.tool_use_id] || "工具", null,
          { content: b.content, isError: !!b.is_error });
      } else if (b.type === "image") {
        inner += `<div class="dim" style="font-size:var(--fs-sm)">[图片 ${b.source ? (b.source.media_type || "") : ""}]</div>`;
      } else if (b.type === "document") {
        inner += `<div class="dim" style="font-size:var(--fs-sm)">[文档 ${F.esc(b.title || "")}]</div>`;
      }
    }
    // OpenAI 风格 assistant.tool_calls
    if (Array.isArray(m.tool_calls)) {
      for (const tc of (m.tool_calls || [])) {
        const fn = tc.function || {};
        toolNames[tc.id] = fn.name;
        inner += toolCard(fn.name, fn.arguments || "{}");
      }
    }
    if (!inner) return "";
    const cls = role === "user" ? "user" : role === "system" ? "system" : "assistant";
    return `<div class="msg ${cls}"><div class="msg-role">${F.esc(role)}</div>${inner}</div>`;
  }

  // 本轮回复（body.result）
  function resultBlock(res) {
    if (!res) return "";
    let inner = "";
    if (res.thinking) inner += thinkBlock(res.thinking);
    if (res.text) inner += textBlock(res.text);
    for (const tc of (res.toolCalls || [])) {
      inner += toolCard(tc.name, typeof tc.args === "string" ? tc.args : JSON.stringify(tc.args || {}));
    }
    const badges = [];
    if (res.stopReason) badges.push(`<span class="badge">${F.esc(F.stopReason(res.stopReason))}</span>`);
    if (res.error) badges.push(`<span class="badge err">${F.esc(res.error.slice(0, 80))}</span>`);
    if (!inner && !badges.length) return "";
    return `<div class="msg assistant">
      <div class="msg-role"><span class="name">本轮回复</span><span class="badges">${badges.join("")}</span></div>
      ${inner || '<div class="dim">（无内容）</div>'}
    </div>`;
  }

  // token 解剖：总量 + 四段构成条 + 带占比的图例
  function tokbar(u) {
    if (!u || !(u.input || u.cache_read || u.cache_write || u.output)) return "";
    const total = (u.input || 0) + (u.cache_read || 0) + (u.cache_write || 0) + (u.output || 0) || 1;
    const seg = (v, cls) => v > 0 ? `<i class="${cls}" style="width:${(v / total * 100).toFixed(1)}%"></i>` : "";
    const leg = (label, v, color) => `
      <div class="l"><i style="background:${color}"></i>${label}<b>${F.num(v)}</b><span class="pct">${F.pct(v, total) || "0%"}</span></div>`;
    return `
      <div class="tok-total"><b>${F.tok(total)}</b><span>总 token</span></div>
      <div class="tokbar">${seg(u.input, "i")}${seg(u.cache_read, "cr")}${seg(u.cache_write, "cw")}${seg(u.output, "o")}</div>
      <div class="tok-legend">
        ${leg("输入", u.input || 0, "var(--dv-in)")}
        ${leg("缓存读", u.cache_read || 0, "var(--dv-cr)")}
        ${leg("缓存写", u.cache_write || 0, "var(--dv-cw)")}
        ${leg("输出", u.output || 0, "var(--dv-out)")}
      </div>`;
  }

  return { thinkBlock, textBlock, toolCard, messageBlock, resultBlock, tokbar, contentBlocks };
})();

// ===================== 回放器视图 =====================
// 使用旅程重设计：顶部"链路条"呈现请求全貌（提示词 → 对话 → 工具 → 回答），
// 点节点切换分区；默认直接看"回答"。对话/工具均为可折叠结构：
//   对话 —— 每条消息一个折叠行（角色 + 预览），默认只展开最后一条
//   工具 —— 按工具名分组，调用与结果配对成条目
AI.views.replay = (() => {
  const F = AI.fmt, R = AI.render;

  async function render(container, id, params) {
    let data;
    try {
      data = await AI.api.record(id);
    } catch (e) {
      container.innerHTML = `<div class="view-inner"><div class="empty-state"><b>记录不存在</b>${F.esc(e.message)}</div></div>`;
      return;
    }
    // ?turn=<id>：从用户请求轮次点进来的，返回链接指回该轮
    const fromTurn = params && params.get("turn");
    const backlink = fromTurn
      ? `<a class="backlink" href="#/turn/${encodeURIComponent(fromTurn)}">← 用户请求</a>`
      : `<a class="backlink" href="#/requests">← 请求</a>`;
    const rec = data.record, body = data.body || {};
    const req = parseJSON(body.requestBody) || {};
    const messages = req.messages || [];
    const result = body.result;

    // tool_use id → name
    const toolNames = {};
    for (const m of messages) {
      for (const b of R.contentBlocks(m)) {
        if (b.type === "tool_use") toolNames[b.id] = b.name;
      }
      for (const tc of (m.tool_calls || [])) {
        const fn = tc.function || {};
        if (fn.name) toolNames[tc.id] = fn.name;
      }
    }

    // ---- 各分区内容 ----
    const sysText = systemText(req, messages);
    const toolsDef = Array.isArray(req.tools) ? req.tools : [];
    const toolEntries = collectTools(messages, result, toolNames);

    // 回答：本轮回复（默认分区）
    const resText = (result && result.text) || "";
    const resThink = (result && result.thinking) || "";
    const resToolCalls = (result && result.toolCalls) || [];
    const answerHTML = R.resultBlock(result) ||
      `<div class="empty-state">本次响应没有可展示的回答（无解码结果），看「原文」里的响应原文</div>`;

    // 对话：分「本次调用新增」与「历史上下文」。协议上每条请求都携带此前
    // 完整对话，平铺会被几百条历史淹没；从最后一条 user 消息切到结尾的是
    // 这次调用真正新增的内容（用户输入或工具结果 + 收尾系统注入），其余
    // 折叠到下面。
    let newFrom = 0;
    for (let i = messages.length - 1; i >= 0; i--) {
      if (String(messages[i].role || "").toLowerCase() === "user") { newFrom = i; break; }
    }
    const newMsgs = messages.slice(newFrom);
    const histMsgs = messages.slice(0, newFrom);
    const newRows = newMsgs.map((m, j) =>
      turnBlock(m, newFrom + j, toolNames, newFrom + j === messages.length - 1)).join("");
    const histRows = histMsgs.map((m, j) => turnBlock(m, j, toolNames, false)).join("");
    const turnsHTML = messages.length
      ? chatToolbar()
        + (histMsgs.length ? `<div class="pv-sec chat-sec">本次调用新增 · ${newMsgs.length} 条</div>` : "")
        + newRows
        + (histMsgs.length ? `
      <details class="turn hist-block">
        <summary>
          <span class="t-chip">历史</span>
          <span class="t-prev">此前对话上下文 · ${histMsgs.length} 条（协议原样携带，非本次新增）</span>
        </summary>
        <div class="t-body">${histRows}</div>
      </details>` : "")
      : `<div class="empty-state">请求体里没有 messages（${F.esc(rec.path || "")}），完整内容见「原文」</div>`;

    // 工具：按名字分组
    const toolsHTML = toolGroupsHTML(toolEntries);

    // 提示词
    let promptHTML = "";
    if (sysText) {
      promptHTML += `<div class="pv-sec">系统提示 · ${F.tok(sysText.length)} 字</div>
        <pre class="pv-code">${F.esc(sysText)}</pre>`;
    }
    if (toolsDef.length) {
      const names = toolsDef.map(t => t.name || (t.function && t.function.name)).filter(Boolean);
      promptHTML += `<div class="pv-sec mt">工具定义 · ${toolsDef.length} 个（${F.esc(names.join("、"))}）</div>
        <pre class="pv-code">${F.esc(F.prettyJSON(toolsDef))}</pre>`;
    }

    // 原文
    const rawHTML = `
      <div class="pv-sec">请求体 JSON · ${F.bytes(rec.reqBytes)}</div>
      <pre class="pv-code">${F.esc(F.prettyJSON(body.requestBody || ""))}</pre>
      <div class="pv-sec mt">响应原文（SSE / JSON）· ${F.bytes(rec.respBytes)}</div>
      <pre class="pv-code">${F.esc(body.responseBody || "（空）")}</pre>
      <div class="pv-sec mt">请求头（已脱敏）</div>
      <pre class="pv-code">${F.esc(JSON.stringify(body.requestHeaders || {}, null, 2))}</pre>`;

    // ---- 链路条（即 Tab 栏）：提示词 → 对话 → 工具 → 回答 ｜ 原文 ----
    const chain = [
      { k: "prompt", label: "提示词", n: sysText || toolsDef.length ? F.tok(sysText.length) + " 字" : "", hide: !promptHTML },
      { k: "chat", label: "对话", n: messages.length
        ? (histMsgs.length ? `新增 ${newMsgs.length} · 历史 ${histMsgs.length}` : messages.length + " 条")
        : "" },
      { k: "tools", label: "工具", n: toolEntries.length ? toolEntries.length + " 次" : "", hide: !toolEntries.length },
      { k: "answer", label: "回答", n: resText ? F.tok(resText.length) + " 字" : "", goal: true },
      { k: "raw", label: "原文" },
    ].filter(t => !t.hide);
    const active = (resText || resThink || resToolCalls.length) ? "answer"
      : messages.length ? "chat" : "raw";

    // ---- 右：元数据侧栏 ----
    const u = rec.usage || (result && result.usage);
    const sideHTML = `
      <div class="list"><div class="row head"><b>请求</b></div>
        <div class="card-b">
        <dl class="kv">
          <dt>状态</dt><dd>${rec.error ? F.statusBadge(rec.status, rec.error) : F.statusBadge(rec.status)}</dd>
          <dt>耗时</dt><dd>${F.dur(rec.durationMs)}</dd>
          <dt>首字节</dt><dd>${rec.ttftMs ? F.dur(rec.ttftMs) : "—"}</dd>
          <dt>请求大小</dt><dd>${F.bytes(rec.reqBytes)}</dd>
          <dt>响应大小</dt><dd>${F.bytes(rec.respBytes)}</dd>
          <dt>协议</dt><dd class="mono">${F.esc(rec.protocol || "—")}</dd>
          <dt>路径</dt><dd class="mono" title="${F.esc(rec.path || "")}">${F.esc(rec.path || "—")}</dd>
          <dt>Agent</dt><dd>${F.esc(F.agentName(rec.agent))}</dd>
          ${rec.subagent ? `<dt>子代理</dt><dd class="mono">${F.esc(rec.subagent)}</dd>` : ""}
          <dt>时间</dt><dd>${F.datetime(rec.time)}</dd>
        </dl>
        </div>
      </div>
      <div class="list mt"><div class="row head"><b>Token 用量</b></div>
        <div class="card-b">
        ${u ? R.tokbar(u) : `<div class="dim" style="font-size:var(--fs-md)">此请求没有 usage 数据</div>`}
        </div>
      </div>
      ${rec.session ? `
      <div class="list mt"><div class="row head"><b>会话</b></div>
        <div class="card-b">
        <a class="btn" href="#/tsession/${encodeURIComponent(rec.session)}">查看此流量会话</a>
        <a class="btn mt8" href="#/diff?day=${dayOf(id)}&session=${encodeURIComponent(rec.session)}">对比上下文膨胀</a>
        </div>
      </div>` : ""}`;

    container.innerHTML = `
      <div class="view-inner">
        <div class="view-h">
          ${backlink}
          <h1>${F.esc(F.agentName(rec.agent))} · ${F.esc(rec.model || "未知模型")}</h1>
          ${rec.error ? F.statusBadge(rec.status, rec.error) : F.statusBadge(rec.status)}
        </div>
        <div class="replay">
          <div class="replay-main">
            <div class="rtabs chain" role="tablist">
              ${chain.map((t, i) => `
              ${i > 0 && t.k !== "raw" && chain[i - 1].k !== "answer" ? '<i class="arr">→</i>' : ""}
              <button type="button" role="tab" data-tab="${t.k}"
                class="${t.k === active ? "on" : ""}${t.goal ? " goal" : ""}${t.k === "raw" ? " rawbtn" : ""}">
                ${t.label}${t.n ? `<span class="n">${t.n}</span>` : ""}
              </button>`).join("")}
            </div>
            ${promptHTML ? `<div class="rtab ${active === "prompt" ? "on" : ""}" data-panel="prompt"><div class="flow">${promptHTML}</div></div>` : ""}
            <div class="rtab ${active === "chat" ? "on" : ""}" data-panel="chat"><div class="flow">${turnsHTML}</div></div>
            ${toolEntries.length ? `<div class="rtab ${active === "tools" ? "on" : ""}" data-panel="tools"><div class="flow">${toolsHTML}</div></div>` : ""}
            <div class="rtab ${active === "answer" ? "on" : ""}" data-panel="answer"><div class="flow answer">${answerHTML}</div></div>
            <div class="rtab ${active === "raw" ? "on" : ""}" data-panel="raw"><div class="flow">${rawHTML}</div></div>
          </div>
          <div class="side">${sideHTML}</div>
        </div>
      </div>`;

    bindTabs(container);
    bindTurns(container);
  }

  // ---- Tab / 链路切换 ----
  function bindTabs(container) {
    container.querySelectorAll(".rtabs button").forEach(btn => {
      btn.onclick = () => {
        container.querySelectorAll(".rtabs button").forEach(b => b.classList.toggle("on", b === btn));
        container.querySelectorAll(".rtab").forEach(p =>
          p.classList.toggle("on", p.dataset.panel === btn.dataset.tab));
      };
    });
  }

  // ---- 对话：折叠行交互（展开全部 / 收起全部） ----
  function chatToolbar() {
    return `<div class="turn-bar">
      <span class="dim" style="font-size:11.5px">本次新增在上 · 历史折叠在下 · 默认展开最后一条</span>
      <span style="margin-left:auto;display:inline-flex;gap:6px">
        <button type="button" class="btn sm" data-turns="open">展开全部</button>
        <button type="button" class="btn sm" data-turns="close">收起全部</button>
      </span>
    </div>`;
  }

  function turnBlock(m, i, toolNames, open) {
    const html = R.messageBlock(m, toolNames);
    if (!html) return "";
    const role = m.role || (Array.isArray(m.content_block) ? "user" : "?");
    const blocks = R.contentBlocks(m);
    const hasResult = role === "tool" || blocks.some(b => b.type === "tool_result");
    const hasCall = (m.tool_calls && m.tool_calls.length) || blocks.some(b => b.type === "tool_use");
    const kind = hasResult || hasCall ? "tool" : role;
    const chip = hasResult ? "工具结果" : hasCall ? "工具调用"
      : role === "user" ? "用户" : role === "assistant" ? "助手" : role;
    return `<details class="turn t-${kind}" ${open ? "open" : ""}>
      <summary>
        <span class="t-chip">${chip}</span>
        <span class="t-prev">${F.esc(turnPreview(m, toolNames))}</span>
        <span class="t-idx">${i + 1}</span>
      </summary>
      <div class="t-body">${html}</div>
    </details>`;
  }

  // 一条消息的摘要行：用户/助手取首段文本；工具取名字与参数预览
  function turnPreview(m, toolNames) {
    if (m.role === "tool") return "结果 → " + previewText(contentToText(m.content));
    const parts = [];
    if (Array.isArray(m.tool_calls) && m.tool_calls.length) {
      parts.push("调用 " + m.tool_calls.map(tc => (tc.function && tc.function.name) || "?").join("、"));
    }
    for (const b of R.contentBlocks(m)) {
      if (b.type === "tool_use") parts.push("调用 " + (b.name || "?"));
      else if (b.type === "tool_result") parts.push("结果 → " + previewText(contentToText(b.content)));
      else if (b.type === "text" && b.text) parts.push(previewText(b.text));
      else if (b.type === "thinking") parts.push("（思考 " + F.tok((b.thinking || b.text || "").length) + " 字）");
    }
    if (typeof m.content === "string" && m.content) parts.push(previewText(m.content));
    if (Array.isArray(m.content_block)) {
      for (const cb of m.content_block) {
        const t = (cb.content || {}).text_block;
        if (t && t.text) parts.push(previewText(t.text));
      }
    }
    return parts.join(" · ") || "（无文本）";
  }

  function previewText(t) {
    t = (t || "").replace(/\s+/g, " ").trim();
    return t.length > 90 ? t.slice(0, 90) + "…" : t;
  }

  function contentToText(c) {
    if (typeof c === "string") return c;
    if (Array.isArray(c)) return c.map(x => x.text || x).join(" ");
    return c == null ? "" : JSON.stringify(c);
  }

  function bindTurns(container) {
    const bar = container.querySelector(".turn-bar");
    if (!bar) return;
    bar.querySelector('[data-turns="open"]').onclick = () =>
      container.querySelectorAll('.rtab[data-panel="chat"] details.turn').forEach(d => d.open = true);
    bar.querySelector('[data-turns="close"]').onclick = () =>
      container.querySelectorAll('.rtab[data-panel="chat"] details.turn').forEach(d => d.open = false);
  }

  // ---- 工具：调用与结果配对，按工具名分组 ----
  function collectTools(messages, result, toolNames) {
    const calls = [];
    const results = {};
    for (const m of messages) {
      for (const b of R.contentBlocks(m)) {
        if (b.type === "tool_use") calls.push({ id: b.id, name: b.name, args: JSON.stringify(b.input || {}) });
        else if (b.type === "tool_result") results[b.tool_use_id] = { content: contentToText(b.content), isError: !!b.is_error };
      }
      for (const tc of (m.tool_calls || [])) {
        const fn = tc.function || {};
        calls.push({ id: tc.id, name: fn.name, args: fn.arguments || "{}" });
      }
      if (m.role === "tool") {
        results[m.tool_call_id] = { content: contentToText(m.content), isError: false };
      }
    }
    const entries = calls.map(c => ({
      name: c.name || toolNames[c.id] || "工具",
      args: c.args,
      result: results[c.id] || null,
    }));
    if (result && Array.isArray(result.toolCalls)) {
      for (const tc of result.toolCalls) {
        entries.push({ name: tc.name, args: typeof tc.args === "string" ? tc.args : JSON.stringify(tc.args || {}), result: null });
      }
    }
    return entries;
  }

  function toolGroupsHTML(entries) {
    if (!entries.length) return "";
    const groups = new Map();
    entries.forEach(e => {
      if (!groups.has(e.name)) groups.set(e.name, []);
      groups.get(e.name).push(e);
    });
    let out = "";
    let gi = 0;
    for (const [name, list] of groups) {
      out += `<details class="tgroup" ${gi === 0 ? "open" : ""}>
        <summary><span class="t-chip tool">${F.esc(name)}</span>
          <span class="t-count">${list.length} 次</span></summary>
        <div class="tg-body">${list.map((e, i) => `
          <details class="tcall">
            <summary><span class="t-idx">#${i + 1}</span>
              <span class="t-prev">${F.esc(previewText(e.args))}</span>
              ${e.result ? `<span class="badge${e.result.isError ? " err" : ""}">有结果</span>` : '<span class="badge">无结果</span>'}</summary>
            <div class="tc-detail">
              <div class="pv-sec">参数</div>
              <pre class="tool-args">${F.esc(F.prettyJSON(e.args))}</pre>
              ${e.result ? `<div class="pv-sec mt">结果</div>
                <pre class="tool-result">${F.esc(previewText((e.result.content || "").slice(0, 2000)))}</pre>` : ""}
            </div>
          </details>`).join("")}</div>
      </details>`;
      gi++;
    }
    return out;
  }

  // ---- 其他 ----
  // systemText 系统提示归一：anthropic req.system 或 openai 风格 system 消息
  function systemText(req, messages) {
    if (req.system) {
      return typeof req.system === "string" ? req.system
        : (Array.isArray(req.system) ? req.system.map(s => s.text || "").join("\n") : JSON.stringify(req.system));
    }
    const sys = messages.filter(m => m.role === "system").map(m =>
      typeof m.content === "string" ? m.content : JSON.stringify(m.content));
    return sys.join("\n\n");
  }

  // parseJSON 容错解析：后端 json.RawMessage 会把请求体作为内嵌 JSON
  // 对象返回（不是字符串），两者都要认。
  function parseJSON(raw) {
    if (raw == null) return null;
    if (typeof raw === "object") return raw;
    if (typeof raw !== "string" || !raw) return null;
    try { return JSON.parse(raw); } catch { return null; }
  }

  function dayOf(id) {
    // id 形如 20260118-150405-xxxx
    if (id && id.length >= 8) return id.slice(0, 4) + "-" + id.slice(4, 6) + "-" + id.slice(6, 8);
    return F.today();
  }

  return { render };
})();
