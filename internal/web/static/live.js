// LIVE 实时流：订阅 /api/live（SSE），把 IR 事件增量聚合成"正在进行的请求"。
// 全局命名空间 AI.live。
//
// 消息协议（见 internal/live/live.go）：
//   req_start {id, agent, session, model, path}   请求已发出
//   event     {id, ev:{kind,text,args...}}        流式增量
//   req_end   {id, record}                        完成（record 为完整账本记录）
AI.live = (() => {
  const active = new Map();   // id → 进行中的请求
  const done = [];            // 刚完成的（保留 40s 供概览页展示，然后清理）
  const listeners = new Set();
  let es = null;
  let connected = false;
  const connListeners = new Set();

  // 单个进行中的请求（前端版 Collector：把增量事件拼成可展示的状态）
  function newRec(m) {
    return {
      id: m.id, agent: m.agent || "", session: m.session || "", subagent: m.subagent || "",
      model: m.model || "", path: m.path || "", proto: m.proto || "",
      startAt: Date.now(),           // 收到 req_start 的本地时刻（近似请求开始）
      text: "", thinking: "", evCount: 0,
      tools: [],                     // [{id, name, args}]
      stop: "", usage: null, record: null, doneAt: 0,
    };
  }

  function applyEvent(r, ev) {
    r.evCount++;
    switch (ev.kind) {
      case "text": r.text += ev.text || ""; break;
      case "think": r.thinking += ev.text || ""; break;
      case "tool_start":
        r.tools.push({ id: ev.toolId, name: ev.name, args: "" });
        break;
      case "tool_args":
        for (const t of r.tools) {
          if (t.id === ev.toolId) { t.args += ev.args || ""; break; }
        }
        break;
      case "stop": r.stop = ev.stop || ""; break;
      case "usage":
        if (ev.usage) {
          r.usage = Object.assign({}, r.usage, ev.usage);
        }
        break;
    }
  }

  function finish(r, record) {
    r.record = record;
    r.doneAt = Date.now();
    active.delete(r.id);
    done.unshift(r);
    if (done.length > 20) done.pop();
    // 40 秒后清理终态条目（列表页会从账本看到它们）
    setTimeout(() => {
      const i = done.indexOf(r);
      if (i >= 0) { done.splice(i, 1); notify(); }
    }, 40_000);
  }

  function notify() {
    for (const fn of listeners) {
      try { fn(); } catch (e) { console.error("live listener", e); }
    }
  }

  let notifyTimer = 0;
  // 节流通知：token 级事件很密，界面 100ms 刷一次足够
  function scheduleNotify() {
    if (notifyTimer) return;
    notifyTimer = setTimeout(() => { notifyTimer = 0; notify(); }, 100);
  }

  function setConnected(v) {
    if (connected === v) return;
    connected = v;
    for (const fn of connListeners) {
      try { fn(v); } catch {}
    }
  }

  function start() {
    if (es) return;
    try {
      es = new EventSource("/api/live");
    } catch {
      return; // 极老浏览器：无实时功能，页面其余部分照常
    }
    es.onopen = () => setConnected(true);
    es.onerror = () => setConnected(false); // EventSource 会自动重连
    es.onmessage = (e) => {
      let m;
      try { m = JSON.parse(e.data); } catch { return; }
      if (m.type === "req_start") {
        active.set(m.id, newRec(m));
      } else if (m.type === "event" && m.ev) {
        const r = active.get(m.id);
        if (r) { applyEvent(r, m.ev); }
      } else if (m.type === "req_end") {
        let r = active.get(m.id);
        if (!r) r = newRec(m); // 页面打开前就开始的请求，直接收到终态
        finish(r, m.record);
      }
      scheduleNotify();
    };
  }

  return {
    start,
    get connected() { return connected; },
    // 当前进行中（按开始时间倒序）
    actives: () => [...active.values()].sort((a, b) => b.startAt - a.startAt),
    // 刚完成（40 秒窗口内）
    dones: () => done.slice(),
    onUpdate(fn) { listeners.add(fn); return () => listeners.delete(fn); },
    onConn(fn) { connListeners.add(fn); fn(connected); return () => connListeners.delete(fn); },
  };
})();
