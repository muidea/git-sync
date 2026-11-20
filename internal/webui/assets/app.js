const state = {
  authRequired: false,
  authenticated: false,
  targets: [],
  filter: "all",
  search: "",
  jobs: [],
  pollTimer: null,
};

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(opts.headers || {}) },
    credentials: "same-origin",
    ...opts,
  });
  if (res.status === 401) {
    showModal("loginModal");
    throw new Error("未认证，请先输入 API Token");
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) {
    const msg = (data && data.error) ? data.error : `HTTP ${res.status}`;
    const err = new Error(msg);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

function toast(msg) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.classList.remove("hidden");
  setTimeout(() => el.classList.add("hidden"), 3500);
}

function showModal(id) { document.getElementById(id).classList.remove("hidden"); }
function hideModal(id) { document.getElementById(id).classList.add("hidden"); }

function formatBytes(bytes) {
  if (!bytes || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = bytes;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

function formatTime(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

function statusBadge(st) {
  switch (st) {
    case "updated":
    case "cloned":
    case "up_to_date":
    case "succeeded":
      return `<span class="badge badge-ok">${st}</span>`;
    case "missing":
    case "partial_failure":
    case "running":
    case "queued":
      return `<span class="badge badge-warn">${st}</span>`;
    case "failed":
      return `<span class="badge badge-danger">${st}</span>`;
    default:
      return `<span class="badge badge-muted">${st || "pending"}</span>`;
  }
}

async function checkSession() {
  const s = await api("/api/v1/session");
  state.authRequired = s.auth_required;
  state.authenticated = s.authenticated;
  const badge = document.getElementById("authBadge");
  const logoutBtn = document.getElementById("btnLogout");
  if (!s.auth_required) {
    badge.textContent = "回环免密模式";
    badge.className = "badge badge-warn";
    logoutBtn.classList.add("hidden");
    return true;
  }
  if (s.authenticated) {
    badge.textContent = "Token 已认证";
    badge.className = "badge badge-ok";
    logoutBtn.classList.remove("hidden");
    hideModal("loginModal");
    return true;
  }
  badge.textContent = "未认证";
  badge.className = "badge badge-danger";
  showModal("loginModal");
  return false;
}

async function loadStatusAndDashboard() {
  const data = await api("/api/v1/status");
  const s = data.summary || { counts: {} };
  const c = s.counts || {};
  const healthy = (c.cloned || 0) + (c.updated || 0) + (c.up_to_date || 0);

  document.getElementById("kpiGrid").innerHTML = `
    <div class="kpi"><div class="kpi-label">纳管总数</div><div class="kpi-value">${c.managed_total ?? state.targets.length}</div></div>
    <div class="kpi"><div class="kpi-label">健康仓库</div><div class="kpi-value" style="color:var(--success)">${healthy}</div></div>
    <div class="kpi"><div class="kpi-label">同步失败</div><div class="kpi-value" style="color:var(--danger)">${c.failed ?? 0}</div></div>
    <div class="kpi"><div class="kpi-label">远端缺失 (已保留)</div><div class="kpi-value" style="color:var(--warn)">${c.missing ?? 0}</div></div>
    <div class="kpi"><div class="kpi-label">已失管</div><div class="kpi-value">${c.unmanaged ?? 0}</div></div>
    <div class="kpi"><div class="kpi-label">总存储占用</div><div class="kpi-value">${formatBytes(data.total_storage_bytes)}</div></div>
  `;

  const disk = data.disk || {};
  const usedPct = disk.total_bytes ? (100 - (disk.avail_percent || 0)).toFixed(1) : "0";
  const fillClass = disk.avail_percent < 10 ? "danger" : (disk.avail_percent < 20 ? "warn" : "");
  document.getElementById("diskPanel").innerHTML = `
    <div class="kv"><span>备份根目录</span><span class="mono">${data.root_dir}</span></div>
    <div class="kv"><span>可用 / 总容量</span><span>${formatBytes(disk.avail_bytes)} / ${formatBytes(disk.total_bytes)} (${(disk.avail_percent || 0).toFixed(1)}% 可用)</span></div>
    <div class="progress-track"><div class="progress-fill ${fillClass}" style="width:${usedPct}%"></div></div>
  `;

  document.getElementById("schedulerPanel").innerHTML = `
    <div class="kv"><span>上次运行 ID</span><span class="mono">${s.run_id || "-"}</span></div>
    <div class="kv"><span>完成时间 / 耗时</span><span>${formatTime(s.finished_at)} (${((s.duration_ms || 0) / 1000).toFixed(1)}s)</span></div>
    <div class="kv"><span>退出码</span><span>${s.exit_code ?? "-"}</span></div>
    <div class="kv"><span>下次定时同步</span><span>${formatTime(data.next_sync_at)}</span></div>
  `;

  const abnormal = s.abnormal_repos || [];
  if (abnormal.length === 0) {
    document.getElementById("abnormalPanel").innerHTML = `<span class="muted">所有白名单仓库状态健康，无异常项。</span>`;
  } else {
    document.getElementById("abnormalPanel").innerHTML = `
      <table class="table">
        <thead><tr><th>仓库</th><th>状态</th><th>连续失败</th><th>详情</th><th>操作</th></tr></thead>
        <tbody>
          ${abnormal.map(r => `
            <tr>
              <td class="mono">${r.repo}</td>
              <td>${statusBadge(r.state)}</td>
              <td>${r.consecutive_failures}</td>
              <td style="white-space:normal;max-width:460px">${escapeHtml(r.last_error)}</td>
              <td><button class="btn btn-sm btn-primary" onclick="window.__syncRepo('${r.repo}')">重试同步</button></td>
            </tr>
          `).join("")}
        </tbody>
      </table>
    `;
  }

  document.getElementById("configPre").textContent = JSON.stringify(data.config_summary || {}, null, 2);
}

async function loadTargets() {
  const res = await api("/api/v1/targets");
  state.targets = res.targets || [];
  renderTargets();
}

function renderTargets() {
  const q = state.search.trim().toLowerCase();
  const rows = state.targets.filter(item => {
    if (q && !item.repo.toLowerCase().includes(q)) return false;
    const st = item.state || "pending";
    if (state.filter === "healthy") return ["cloned", "updated", "up_to_date"].includes(st);
    if (state.filter !== "all") return st === state.filter;
    return true;
  });

  const body = document.getElementById("targetsBody");
  if (rows.length === 0) {
    body.innerHTML = `<tr><td colspan="7" class="muted">暂无匹配项目</td></tr>`;
    return;
  }

  body.innerHTML = rows.map(item => {
    const isUnmanaged = item.state === "unmanaged";
    const sha = item.head_commit ? (item.head_commit === "EMPTY" ? "EMPTY" : item.head_commit.slice(0, 7)) : "-";
    const cfg = item.effective || {};
    return `
      <tr>
        <td><button class="link-btn mono" onclick="window.__showRepo('${item.repo}')">${item.repo}</button></td>
        <td>${statusBadge(item.state)}</td>
        <td class="mono">${sha}</td>
        <td>${formatTime(item.last_git_sync_at)}</td>
        <td>${formatBytes(item.disk_size_bytes)}</td>
        <td class="muted">${cfg.enable_lfs || "auto"} / ${cfg.mirror_pr_refs ? "PR" : "no-PR"} / ${cfg.skip_unchanged ? "skip" : "force"}</td>
        <td>
          ${!isUnmanaged ? `<button class="btn btn-sm btn-primary" onclick="window.__syncRepo('${item.repo}')">Sync</button>` : ""}
          ${!isUnmanaged ? `<button class="btn btn-sm btn-secondary" onclick="window.__verifyRepo('${item.repo}')">Verify</button>` : ""}
          ${!isUnmanaged ? `<button class="btn btn-sm btn-secondary" onclick="window.__openEdit('${item.repo}')">Edit</button>` : ""}
          ${!isUnmanaged ? `<button class="btn btn-sm btn-danger" onclick="window.__removeTarget('${item.repo}')">Remove</button>` : ""}
        </td>
      </tr>
    `;
  }).join("");
}

async function loadJobs() {
  const res = await api("/api/v1/jobs");
  state.jobs = res.jobs || [];

  const running = state.jobs.find(j => j.status === "running" || j.status === "queued");
  const banner = document.getElementById("activeJobBanner");
  if (running) {
    banner.classList.remove("hidden");
    banner.textContent = `正在执行 ${running.kind.toUpperCase()} (${running.id})：已完成 ${running.completed}/${running.total} ${running.current_repo ? "· " + running.current_repo : ""}`;
    scheduleFastPoll();
  } else {
    banner.classList.add("hidden");
  }

  const body = document.getElementById("jobsBody");
  if (state.jobs.length === 0) {
    body.innerHTML = `<tr><td colspan="8" class="muted">暂无任务记录</td></tr>`;
    return;
  }
  body.innerHTML = state.jobs.map(j => {
    const dur = j.finished_at ? `${((new Date(j.finished_at) - new Date(j.started_at)) / 1000).toFixed(1)}s` : "运行中";
    return `
      <tr>
        <td class="mono">${j.id}</td>
        <td>${j.kind}</td>
        <td>${j.trigger}</td>
        <td>${statusBadge(j.status)}</td>
        <td>${j.completed}/${j.total}</td>
        <td>${formatTime(j.started_at)}</td>
        <td>${dur}</td>
        <td><button class="btn btn-sm btn-secondary" onclick="window.__showJob('${j.id}')">查看</button></td>
      </tr>
    `;
  }).join("");
}

function scheduleFastPoll() {
  if (state.pollTimer) return;
  state.pollTimer = setTimeout(async () => {
    state.pollTimer = null;
    try {
      await refreshAll();
    } catch {}
  }, 2000);
}

async function refreshAll() {
  await Promise.all([loadStatusAndDashboard(), loadTargets(), loadJobs()]);
}

function escapeHtml(s) {
  return String(s || "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

window.__syncRepo = async (repo) => {
  try {
    const res = await api("/api/v1/sync", { method: "POST", body: JSON.stringify({ repos: [repo] }) });
    toast(`已触发 ${repo} 同步 (${res.job.id})`);
    await refreshAll();
  } catch (e) { toast(e.message); }
};

window.__verifyRepo = async (repo) => {
  try {
    const res = await api("/api/v1/verify", { method: "POST", body: JSON.stringify({ repos: [repo], fsck: true }) });
    toast(`已触发 ${repo} 校验 (${res.job.id})`);
    await refreshAll();
  } catch (e) { toast(e.message); }
};

window.__showRepo = async (repo) => {
  try {
    const data = await api(`/api/v1/repos/${repo}`);
    document.getElementById("repoDetailTitle").textContent = `仓库详情：${repo}`;
    document.getElementById("repoDetailPre").textContent = JSON.stringify(data, null, 2);
    showModal("repoDetailModal");
  } catch (e) { toast(e.message); }
};

window.__showJob = async (id) => {
  try {
    const job = await api(`/api/v1/jobs/${id}`);
    document.getElementById("jobDetailTitle").textContent = `任务详情：${id}`;
    document.getElementById("jobDetailPre").textContent = JSON.stringify(job, null, 2);
    document.getElementById("jobDetailCard").classList.remove("hidden");
  } catch (e) { toast(e.message); }
};

window.__openEdit = (repo) => {
  const item = state.targets.find(t => t.repo === repo);
  if (!item) return;
  const ov = item.overrides || {};
  document.getElementById("editModalTitle").textContent = `编辑策略：${repo}`;
  document.getElementById("editRepoName").value = repo;
  document.getElementById("editLFS").value = ov.enable_lfs || "default";
  document.getElementById("editMirrorPR").value = ov.mirror_pr_refs === undefined ? "default" : String(ov.mirror_pr_refs);
  document.getElementById("editSkipUnchanged").value = ov.skip_unchanged === undefined ? "default" : String(ov.skip_unchanged);
  document.getElementById("editUpdateTimeout").value = ov.update_timeout || "";
  document.getElementById("editTargetError").classList.add("hidden");
  showModal("editTargetModal");
};

window.__removeTarget = async (repo) => {
  if (!confirm(`确认将 ${repo} 移出白名单？\n本地已有备份目录将按 unmanaged_policy 保留，不会被直接物理删除。`)) return;
  try {
    await api(`/api/v1/targets/${repo}`, { method: "DELETE" });
    toast(`已将 ${repo} 移出白名单`);
    await refreshAll();
  } catch (e) { toast(e.message); }
};

function bindUI() {
  document.querySelectorAll(".tab").forEach(btn => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".tab").forEach(b => b.classList.remove("active"));
      document.querySelectorAll(".tab-panel").forEach(p => p.classList.remove("active"));
      btn.classList.add("active");
      document.getElementById(`tab-${btn.dataset.tab}`).classList.add("active");
    });
  });

  document.querySelectorAll(".modal-close").forEach(btn => {
    btn.addEventListener("click", () => hideModal(btn.dataset.close));
  });

  document.getElementById("targetSearch").addEventListener("input", (e) => {
    state.search = e.target.value;
    renderTargets();
  });

  document.querySelectorAll("#stateFilterGroup .pill").forEach(btn => {
    btn.addEventListener("click", () => {
      document.querySelectorAll("#stateFilterGroup .pill").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
      state.filter = btn.dataset.filter;
      renderTargets();
    });
  });

  document.getElementById("btnOpenAddTarget").addEventListener("click", () => {
    document.getElementById("addTargetForm").reset();
    document.getElementById("addCheckRemote").checked = true;
    document.getElementById("addSyncNow").checked = true;
    document.getElementById("addTargetError").classList.add("hidden");
    showModal("addTargetModal");
  });

  document.getElementById("addTargetForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const errEl = document.getElementById("addTargetError");
    errEl.classList.add("hidden");
    const payload = { repo: document.getElementById("addRepoInput").value.trim() };
    const lfs = document.getElementById("addLFS").value;
    if (lfs) payload.enable_lfs = lfs;
    const pr = document.getElementById("addMirrorPR").value;
    if (pr) payload.mirror_pr_refs = (pr === "true");
    const ct = document.getElementById("addCloneTimeout").value.trim();
    if (ct) payload.clone_timeout = ct;
    const ut = document.getElementById("addUpdateTimeout").value.trim();
    if (ut) payload.update_timeout = ut;

    const check = document.getElementById("addCheckRemote").checked;
    const syncNow = document.getElementById("addSyncNow").checked;
    try {
      await api(`/api/v1/targets?check=${check}&sync_now=${syncNow}`, {
        method: "POST",
        body: JSON.stringify(payload),
      });
      hideModal("addTargetModal");
      toast(`已添加 ${payload.repo}`);
      await refreshAll();
    } catch (err) {
      errEl.textContent = err.message;
      errEl.classList.remove("hidden");
    }
  });

  document.getElementById("editTargetForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const errEl = document.getElementById("editTargetError");
    errEl.classList.add("hidden");
    const repo = document.getElementById("editRepoName").value;
    const patch = {};
    const lfs = document.getElementById("editLFS").value;
    if (lfs === "default") patch.clear_enable_lfs = true; else patch.enable_lfs = lfs;
    const pr = document.getElementById("editMirrorPR").value;
    if (pr === "default") patch.clear_mirror_pr_refs = true; else patch.mirror_pr_refs = (pr === "true");
    const sk = document.getElementById("editSkipUnchanged").value;
    if (sk === "default") patch.clear_skip_unchanged = true; else patch.skip_unchanged = (sk === "true");
    const ut = document.getElementById("editUpdateTimeout").value.trim();
    if (ut) patch.update_timeout = ut;

    try {
      await api(`/api/v1/targets/${repo}`, { method: "PATCH", body: JSON.stringify(patch) });
      hideModal("editTargetModal");
      toast(`已更新 ${repo} 策略`);
      await refreshAll();
    } catch (err) {
      errEl.textContent = err.message;
      errEl.classList.remove("hidden");
    }
  });

  document.getElementById("btnSyncAll").addEventListener("click", async () => {
    try {
      const res = await api("/api/v1/sync", { method: "POST", body: "{}" });
      toast(`已启动全量同步 (${res.job.id})`);
      await refreshAll();
    } catch (e) { toast(e.message); }
  });

  document.getElementById("btnVerifyAll").addEventListener("click", async () => {
    try {
      const res = await api("/api/v1/verify", { method: "POST", body: JSON.stringify({ fsck: false }) });
      toast(`已启动全量校验 (${res.job.id})`);
      await refreshAll();
    } catch (e) { toast(e.message); }
  });

  document.getElementById("btnPreflightAll").addEventListener("click", async () => {
    try {
      const rep = await api("/api/v1/targets/check", { method: "POST" });
      document.getElementById("repoDetailTitle").textContent = "白名单全量预检结果";
      document.getElementById("repoDetailPre").textContent = JSON.stringify(rep, null, 2);
      showModal("repoDetailModal");
    } catch (e) { toast(e.message); }
  });

  document.getElementById("btnRefreshJobs").addEventListener("click", loadJobs);
  document.getElementById("btnCloseJobDetail").addEventListener("click", () => {
    document.getElementById("jobDetailCard").classList.add("hidden");
  });

  document.getElementById("loginForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const errEl = document.getElementById("loginError");
    errEl.classList.add("hidden");
    try {
      await api("/api/v1/session", {
        method: "POST",
        body: JSON.stringify({ token: document.getElementById("loginTokenInput").value }),
      });
      document.getElementById("loginTokenInput").value = "";
      await checkSession();
      await refreshAll();
    } catch (err) {
      errEl.textContent = err.message;
      errEl.classList.remove("hidden");
    }
  });

  document.getElementById("btnLogout").addEventListener("click", async () => {
    await api("/api/v1/session", { method: "DELETE" });
    await checkSession();
  });
}

(async function init() {
  bindUI();
  try {
    const ok = await checkSession();
    if (ok) await refreshAll();
  } catch (e) {
    toast(e.message);
  }
})();
