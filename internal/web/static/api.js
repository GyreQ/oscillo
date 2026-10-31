// API 封装。全局命名空间 AI.api。
AI.api = (() => {

  async function get(path) {
    const resp = await fetch(path);
    if (!resp.ok) {
      let msg = resp.status + "";
      try { msg = (await resp.json()).error || msg; } catch {}
      throw new Error(msg);
    }
    return resp.json();
  }

  async function post(path, body) {
    const resp = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!resp.ok) {
      let msg = resp.status + "";
      try { msg = (await resp.json()).error || msg; } catch {}
      throw new Error(msg);
    }
    return resp.json();
  }

  // query 构造（跳过空值）
  function qs(params) {
    const u = new URLSearchParams();
    for (const [k, v] of Object.entries(params || {})) {
      if (v != null && v !== "") u.set(k, v);
    }
    const s = u.toString();
    return s ? "?" + s : "";
  }

  return {
    get, post, qs,
    days: () => get("/api/days"),
    records: (params) => get("/api/records" + qs(params)),
    record: (id) => get("/api/records/" + encodeURIComponent(id)),
    // 用户请求轮次：一轮 = 一次用户输入触发的 N 次模型调用
    turns: (params) => get("/api/turns" + qs(params)),
    turn: (id) => get("/api/turns/" + encodeURIComponent(id)),
    // 清理指定 agent 的全部记录（如 MITM 误录的 unknown 杂流量）
    purge: (agent) => post("/api/records/purge", { agent }),
    // 一键清空全部录制数据（账本/请求体/技能沉淀；配置与证书保留）
    reset: () => post("/api/reset", {}),
    stats: (days) => get("/api/stats" + qs({ days })),
    sessions: () => get("/api/sessions"),
    session: (agent, id) => get(`/api/sessions/${encodeURIComponent(agent)}/${encodeURIComponent(id)}`),
    tsessions: () => get("/api/tsessions"),
    tsession: (id) => get("/api/tsessions/" + encodeURIComponent(id)),
    skills: () => get("/api/skills"),
    skillDetail: (name) => get("/api/skills/" + encodeURIComponent(name)),
    adapters: () => get("/api/adapters"),
    captureState: () => get("/api/capture"),
    captureOn: () => post("/api/capture", { action: "on" }),
    captureOff: () => post("/api/capture", { action: "off" }),
  };
})();
