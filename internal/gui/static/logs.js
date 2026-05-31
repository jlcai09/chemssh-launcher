const $ = (id) => document.getElementById(id);

async function api(path, options = {}) {
  const res = await fetch(path, options);
  const data = await res.json();
  if (!res.ok) {
    const err = new Error(data.error || "Request failed");
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

function setNotice(message, kind = "") {
  const notice = $("backendNotice");
  notice.textContent = message || "";
  notice.className = "notice" + (kind ? ` ${kind}` : "");
}

function renderCacheEntries(entries) {
  const host = $("cacheEntries");
  host.innerHTML = "";
  const items = Array.isArray(entries) ? entries : [];
  if (!items.length) {
    const empty = document.createElement("p");
    empty.className = "backend-note";
    empty.textContent = "当前没有可展示的缓存目录。";
    host.appendChild(empty);
    return;
  }

  items.forEach((entry) => {
    const row = document.createElement("div");
    row.className = "cache-entry";

    const head = document.createElement("div");
    head.className = "cache-entry-head";
    const title = document.createElement("strong");
    title.textContent = entry.kind === "managed" ? "当前缓存目录" : "旧版缓存目录";
    const state = document.createElement("span");
    state.textContent = entry.exists ? "已存在" : "未发现";
    head.append(title, state);

    const pathLine = document.createElement("div");
    pathLine.className = "path-line";
    const code = document.createElement("code");
    code.textContent = entry.path || "-";
    code.title = "点击复制路径";
    code.addEventListener("click", () => {
      copyPath(entry.path || "", "缓存目录路径已复制。").catch((err) => setNotice(err.message, "error"));
    });
    const copy = document.createElement("button");
    copy.className = "copy-path";
    copy.type = "button";
    copy.textContent = "复制";
    copy.addEventListener("click", () => {
      copyPath(entry.path || "", "缓存目录路径已复制。").catch((err) => setNotice(err.message, "error"));
    });
    pathLine.append(code, copy);

    row.append(head, pathLine);
    host.appendChild(row);
  });
}

function renderBackendInfo(info) {
  $("configDir").textContent = info.config_dir || "-";
  $("clearPending").textContent = info.clear_pending ? "下次启动继续清理" : "状态正常";
  $("clearPending").className = "backend-badge" + (info.clear_pending ? " warn" : " ok");
  renderCacheEntries(info.cache_entries);
}

async function refreshBackendInfo() {
  const info = await api("/api/backend");
  renderBackendInfo(info);
}

async function refreshLogs() {
  const data = await api("/api/launcher-logs");
  const lines = Array.isArray(data.lines) ? data.lines : [];
  const logs = $("logs");
  logs.textContent = lines.join("\n");
  logs.scrollTop = logs.scrollHeight;
}

async function openConfigDir() {
  setNotice("正在打开配置目录...", "");
  await api("/api/backend/open-config-dir", { method: "POST" });
  setNotice("已打开配置目录。", "ok");
}

async function clearCache() {
  setNotice("正在清理旧版缓存...", "");
  const result = await api("/api/backend/clear-cache", { method: "POST" });
  renderBackendInfo(result.info || {});
  const messages = Array.isArray(result.messages) ? result.messages : [];
  setNotice(messages[0] || "旧版缓存清理完成。", "ok");
  await refreshLogs();
}

async function exportProfiles() {
  setNotice("正在导出配置...", "");
  const res = await fetch("/api/backend/export");
  if (!res.ok) {
    const data = await res.json();
    throw new Error(data.error || "Export failed");
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = "chemssh-launcher-profiles.json";
  document.body.appendChild(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(url);
  setNotice("配置已导出。", "ok");
}

async function importProfiles(file) {
  if (!file) return;
  setNotice("正在导入配置...", "");
  const text = await file.text();
  await api("/api/backend/import", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: text
  });
  setNotice("配置已导入。", "ok");
  await refreshLogs();
}

async function copyPath(path, message) {
  if (!path || !navigator.clipboard) {
    setNotice("当前环境不支持自动复制。", "error");
    return;
  }
  await navigator.clipboard.writeText(path);
  setNotice(message, "ok");
}

function bindEvents() {
  $("refreshLogs").addEventListener("click", () => {
    refreshLogs().catch((err) => setNotice(err.message, "error"));
  });
  $("openConfigDir").addEventListener("click", () => {
    openConfigDir().catch((err) => setNotice(err.message, "error"));
  });
  $("clearCache").addEventListener("click", () => {
    clearCache().catch((err) => setNotice(err.message, "error"));
  });
  $("copyConfigDir").addEventListener("click", () => {
    copyPath($("configDir").textContent || "", "配置目录路径已复制。").catch((err) => setNotice(err.message, "error"));
  });
  $("exportProfiles").addEventListener("click", () => {
    exportProfiles().catch((err) => setNotice(err.message, "error"));
  });
  $("importProfilesInput").addEventListener("change", async (event) => {
    const file = event.target.files && event.target.files[0];
    try {
      await importProfiles(file);
      await refreshBackendInfo();
    } catch (err) {
      setNotice(err.message, "error");
    } finally {
      event.target.value = "";
    }
  });
}

bindEvents();
refreshBackendInfo().catch((err) => setNotice(err.message, "error"));
refreshLogs().catch((err) => setNotice(err.message, "error"));
setInterval(() => {
  refreshLogs().catch(() => {});
  refreshBackendInfo().catch(() => {});
}, 2000);
