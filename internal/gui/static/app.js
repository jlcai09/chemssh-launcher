const legacyDefaultStartCommand = "chemweb --config config.yaml";

let defaults = {
  id: "",
  name: "",
  ssh_host: "",
  ssh_port: 22,
  ssh_user: "",
  auth_method: "password",
  has_password: false,
  private_key_path: "",
  has_private_key_passphrase: false,
  remote_host: "127.0.0.1",
  remote_port: 8888,
  local_host: "127.0.0.1",
  local_port: 8888,
  local_url_path: "/",
  pre_start_commands: "",
  start_command: legacyDefaultStartCommand,
  health_check_url: "",
  open_browser: true
};

let profiles = [];
let current = { ...defaults };
let activeProfileId = "";
let activeForwarding = false;
let lang = localStorage.getItem("chemweb-launcher-language") || "zh-CN";

const messages = {
  "zh-CN": {
    tagline: "远程 Chemweb 启动器",
    language: "语言",
    newProfile: "新建配置",
    refresh: "刷新",
    testSsh: "测试 SSH",
    start: "启动",
    stopForwarding: "停止转发",
    stopService: "停止服务+转发",
    more: "更多",
    server: "服务器",
    name: "名称",
    sshHost: "SSH 主机",
    sshPort: "SSH 端口",
    sshUser: "SSH 用户",
    authMethod: "认证方式",
    passwordAuth: "密码",
    privateKeyAuth: "私钥",
    privateKeyPath: "私钥路径",
    secrets: "密钥",
    password: "密码",
    keyPassphrase: "私钥口令",
    edit: "编辑",
    editing: "编辑中",
    tunnel: "隧道",
    remoteHost: "远端主机",
    remotePort: "远端端口",
    localHost: "本地主机",
    localPort: "本地端口",
    localUrlPath: "本地 URL 路径",
    healthCheckUrl: "健康检查 URL",
    openBrowser: "启动后打开 Chemweb 标签页",
    remoteCommands: "远端命令",
    preStartCommands: "预启动命令",
    startCommand: "启动命令",
    startHint: "如果命令里没有 --host 或 --port，会自动追加远端主机和端口。",
    saveProfile: "保存配置",
    delete: "删除",
    sessionLogs: "会话日志",
    noProfiles: "还没有配置。",
    active: "运行中",
    stopped: "已停止",
    unnamed: "未命名",
    user: "用户",
    host: "主机",
    newProfileHeading: "新配置",
    newProfileSummary: "创建一个远程服务器配置。",
    idle: "空闲",
    running: "运行中：{name}",
    forwardingStopped: "服务运行中，转发已停止：{name}",
    saveBeforeStart: "请先保存配置再启动。",
    saveBeforeTest: "请先保存配置再测试。",
    savingProfile: "正在保存配置...",
    profileSaved: "配置已保存。",
    deletingProfile: "正在删除配置...",
    profileDeleted: "配置已删除。",
    deleteConfirm: "删除配置“{name}”？",
    startingSession: "正在启动会话...",
    startRequested: "已请求启动。请查看日志中的 SSH、Chemweb 检测、命令、隧道和健康检查进度。",
    testingSsh: "正在测试 SSH...",
    testOk: "SSH 和 Chemweb 端口检测通过。",
    stoppingForwarding: "正在停止转发...",
    forwardingStopRequested: "已请求停止转发，远端服务不会被停止。",
    confirmStopService: "这会停止远端 Chemweb 服务并关闭转发。如果其他用户正在访问该服务，他们会断开。确定继续吗？",
    stoppingService: "正在停止服务和转发...",
    serviceStopRequested: "已请求停止远端服务和转发。",
    done: "完成。",
    passwordNotSet: "未设置",
    passwordPlaceholder: "输入新密码；留空则清除",
    passphrasePlaceholder: "输入新口令；留空则清除",
    firstSsh: "首次 SSH 连接到 {address}。",
    hostKey: "主机密钥：{type}",
    fingerprint: "指纹：{fingerprint}",
    trustHostKey: "只有当它与可信来源提供的服务器 SSH 指纹一致时才信任。",
    savedTo: "将保存到：{path}"
  },
  en: {
    tagline: "Remote Chemweb launcher",
    language: "Language",
    newProfile: "New Profile",
    refresh: "Refresh",
    testSsh: "Test SSH",
    start: "Start",
    stopForwarding: "Stop Forwarding",
    stopService: "Stop Service + Forwarding",
    more: "More",
    server: "Server",
    name: "Name",
    sshHost: "SSH Host",
    sshPort: "SSH Port",
    sshUser: "SSH User",
    authMethod: "Auth Method",
    passwordAuth: "Password",
    privateKeyAuth: "Private Key",
    privateKeyPath: "Private Key Path",
    secrets: "Secrets",
    password: "Password",
    keyPassphrase: "Key Passphrase",
    edit: "Edit",
    editing: "Editing",
    tunnel: "Tunnel",
    remoteHost: "Remote Host",
    remotePort: "Remote Port",
    localHost: "Local Host",
    localPort: "Local Port",
    localUrlPath: "Local URL Path",
    healthCheckUrl: "Health Check URL",
    openBrowser: "Open Chemweb tab after startup",
    remoteCommands: "Remote Commands",
    preStartCommands: "Pre-start Commands",
    startCommand: "Start Command",
    startHint: "Remote Host and Remote Port are appended automatically when this command does not already include --host or --port.",
    saveProfile: "Save Profile",
    delete: "Delete",
    sessionLogs: "Session Logs",
    noProfiles: "No profiles yet.",
    active: "Active",
    stopped: "Stopped",
    unnamed: "unnamed",
    user: "user",
    host: "host",
    newProfileHeading: "New Profile",
    newProfileSummary: "Create a remote server profile.",
    idle: "Idle",
    running: "Running: {name}",
    forwardingStopped: "Service running, forwarding stopped: {name}",
    saveBeforeStart: "Save the profile before starting.",
    saveBeforeTest: "Save the profile before testing.",
    savingProfile: "Saving profile...",
    profileSaved: "Profile saved.",
    deletingProfile: "Deleting profile...",
    profileDeleted: "Profile deleted.",
    deleteConfirm: "Delete profile \"{name}\"?",
    startingSession: "Starting session...",
    startRequested: "Session start requested. Watch logs for SSH, Chemweb check, command, tunnel, and health check progress.",
    testingSsh: "Testing SSH...",
    testOk: "SSH and Chemweb port checks OK.",
    stoppingForwarding: "Stopping forwarding...",
    forwardingStopRequested: "Forwarding stop requested. The remote service will keep running.",
    confirmStopService: "This will stop the remote Chemweb service and close forwarding. Other users may lose access. Continue?",
    stoppingService: "Stopping service and forwarding...",
    serviceStopRequested: "Remote service and forwarding stop requested.",
    done: "Done.",
    passwordNotSet: "not set",
    passwordPlaceholder: "enter new password; leave empty to clear",
    passphrasePlaceholder: "enter new passphrase; leave empty to clear",
    firstSsh: "First SSH connection to {address}.",
    hostKey: "Host key: {type}",
    fingerprint: "Fingerprint: {fingerprint}",
    trustHostKey: "Only trust this key if it matches the server's SSH fingerprint from a reliable source.",
    savedTo: "It will be saved to: {path}"
  }
};

const $ = (id) => document.getElementById(id);

function t(key, params = {}) {
  const template = (messages[lang] && messages[lang][key]) || messages.en[key] || key;
  return template.replace(/\{(\w+)\}/g, (_, name) => params[name] ?? "");
}

function applyTranslations() {
  document.documentElement.lang = lang;
  $("language").value = lang;
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    el.textContent = t(el.dataset.i18n);
  });
  fillForm();
  renderProfiles();
  refreshStatus().catch(() => {});
}

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options
  });
  const data = await res.json();
  if (!res.ok) {
    const err = new Error(data.error || "Request failed");
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

function renderProfiles() {
  const nav = $("profiles");
  nav.innerHTML = "";
  if (!profiles.length) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = t("noProfiles");
    nav.appendChild(empty);
    return;
  }

  profiles.forEach((profile) => {
    const button = document.createElement("button");
    button.className = "profile-item" + (profile.id === current.id ? " active" : "");
    const running = profile.id === activeProfileId;
    button.innerHTML = `<span class="state-dot ${running ? "running" : "stopped"}" title="${running ? t("active") : t("stopped")}"></span><span class="profile-text"><strong>${escapeHtml(profile.name || `(${t("unnamed")})`)}</strong><span>${escapeHtml(profile.ssh_user || "?")}@${escapeHtml(profile.ssh_host || "?")} - ${escapeHtml(profile.local_host)}:${profile.local_port}</span></span>`;
    button.addEventListener("click", () => setCurrent(profile));
    nav.appendChild(button);
  });
}

async function loadProfiles() {
  profiles = await api("/api/profiles") || [];
  if (!Array.isArray(profiles)) profiles = [];

  if (!profiles.length) {
    current = { ...defaults };
  } else if (!current.id || !profiles.some((profile) => profile.id === current.id)) {
    current = normalizeProfile({ ...defaults, ...profiles[0] });
  } else {
    const selected = profiles.find((profile) => profile.id === current.id);
    current = normalizeProfile({ ...defaults, ...selected });
  }

  renderProfiles();
  fillForm();
}

function setCurrent(profile) {
  current = normalizeProfile({ ...defaults, ...profile });
  fillForm();
  renderProfiles();
}

function newProfile() {
  current = { ...defaults };
  fillForm();
  renderProfiles();
}

function fillForm() {
  $("profileId").value = current.id || "";
  $("name").value = current.name || "";
  $("sshHost").value = current.ssh_host || "";
  $("sshPort").value = current.ssh_port || 22;
  $("sshUser").value = current.ssh_user || "";
  $("authMethod").value = current.auth_method || "password";
  $("privateKeyPath").value = current.private_key_path || "";
  $("remoteHost").value = current.remote_host || "127.0.0.1";
  $("remotePort").value = current.remote_port || 8888;
  $("localHost").value = current.local_host || "127.0.0.1";
  $("localPort").value = current.local_port || 8888;
  $("localUrlPath").value = current.local_url_path || "/";
  $("healthCheckUrl").value = current.health_check_url || "";
  $("openBrowser").checked = current.open_browser !== false;
  $("preStartCommands").value = current.pre_start_commands || "";
  $("startCommand").value = current.start_command || defaults.start_command;
  $("password").value = "";
  $("passphrase").value = "";
  $("password").dataset.action = "keep";
  $("passphrase").dataset.action = "keep";
  $("passwordDisplay").readOnly = true;
  $("passwordDisplay").value = current.has_password ? "*******" : "";
  $("passwordDisplay").placeholder = current.has_password ? "" : t("passwordNotSet");
  $("editPassword").textContent = t("edit");
  $("passphraseDisplay").readOnly = true;
  $("passphraseDisplay").value = current.has_private_key_passphrase ? "*******" : "";
  $("passphraseDisplay").placeholder = current.has_private_key_passphrase ? "" : t("passwordNotSet");
  $("editPassphrase").textContent = t("edit");
  $("heading").textContent = current.name || t("newProfileHeading");
  $("summary").textContent = current.id ? `${current.ssh_user || t("user")}@${current.ssh_host || t("host")} - ${current.local_host}:${current.local_port}` : t("newProfileSummary");
  toggleAuthFields();
}

function readForm() {
  return {
    id: $("profileId").value,
    name: $("name").value,
    ssh_host: $("sshHost").value,
    ssh_port: numberValue("sshPort", 22),
    ssh_user: $("sshUser").value,
    auth_method: $("authMethod").value,
    has_password: current.has_password,
    private_key_path: $("privateKeyPath").value,
    has_private_key_passphrase: current.has_private_key_passphrase,
    remote_host: $("remoteHost").value || "127.0.0.1",
    remote_port: numberValue("remotePort", 8888),
    local_host: $("localHost").value || "127.0.0.1",
    local_port: numberValue("localPort", 8888),
    local_url_path: $("localUrlPath").value || "/",
    pre_start_commands: $("preStartCommands").value,
    start_command: $("startCommand").value,
    health_check_url: $("healthCheckUrl").value,
    open_browser: $("openBrowser").checked
  };
}

function numberValue(id, fallback) {
  const value = parseInt($(id).value, 10);
  return Number.isFinite(value) ? value : fallback;
}

async function saveProfile(event) {
  event.preventDefault();
  const rollback = maskEditedSecretsForSave();
  await runWithFeedback(t("savingProfile"), async () => {
    const profile = readForm();
    const payload = {
      profile,
      secrets: {
        password_action: $("password").dataset.action || "keep",
        password: $("password").value,
        passphrase_action: $("passphrase").dataset.action || "keep",
        passphrase: $("passphrase").value
      }
    };
    const method = profile.id ? "PUT" : "POST";
    const url = profile.id ? `/api/profiles/${profile.id}` : "/api/profiles";
    current = await api(url, { method, body: JSON.stringify(payload) });
    await loadProfiles();
    return t("profileSaved");
  }, rollback);
}

async function deleteProfile() {
  if (!current.id) return;
  if (!confirm(t("deleteConfirm", { name: current.name }))) return;
  await runWithFeedback(t("deletingProfile"), async () => {
    await api(`/api/profiles/${current.id}`, { method: "DELETE" });
    current = { ...defaults };
    await loadProfiles();
    return t("profileDeleted");
  });
}

async function startSession() {
  if (!current.id) {
    alert(t("saveBeforeStart"));
    return;
  }
  await runWithFeedback(t("startingSession"), async () => {
    await apiWithHostKeyPrompt("/api/session/start", { id: current.id });
    await refreshStatus();
    await refreshLogs();
    return t("startRequested");
  });
}

async function testProfile() {
  if (!current.id) {
    alert(t("saveBeforeTest"));
    return;
  }
  await runWithFeedback(t("testingSsh"), async () => {
    await apiWithHostKeyPrompt("/api/profile-test", { id: current.id });
    await refreshLogs();
    return t("testOk");
  });
}

async function apiWithHostKeyPrompt(path, payload) {
  try {
    return await api(path, { method: "POST", body: JSON.stringify(payload) });
  } catch (err) {
    const hostKey = err.data && err.data.host_key;
    if (!hostKey) throw err;
    if (hostKey.mismatch) throw err;

    const ok = confirm([
      t("firstSsh", { address: hostKey.address }),
      "",
      t("hostKey", { type: hostKey.key_type }),
      t("fingerprint", { fingerprint: hostKey.fingerprint }),
      "",
      t("trustHostKey"),
      t("savedTo", { path: hostKey.known_hosts_path })
    ].join("\n"));
    if (!ok) throw err;
    return await api(path, {
      method: "POST",
      body: JSON.stringify({ ...payload, accept_host_key: true })
    });
  }
}

async function stopForwarding() {
  await runWithFeedback(t("stoppingForwarding"), async () => {
    await api("/api/session/stop", { method: "POST" });
    await refreshStatus();
    await refreshLogs();
    return t("forwardingStopRequested");
  });
}

async function stopService() {
  if (!confirm(t("confirmStopService"))) return;
  await runWithFeedback(t("stoppingService"), async () => {
    await api("/api/session/stop-service", { method: "POST" });
    await refreshStatus();
    await refreshLogs();
    return t("serviceStopRequested");
  });
}

async function refreshStatus() {
  const status = await api("/api/session/status");
  activeProfileId = status.running ? status.id : "";
  activeForwarding = Boolean(status.forwarding);
  if (!status.running) {
    $("status").textContent = t("idle");
  } else if (activeForwarding) {
    $("status").textContent = t("running", { name: status.name });
  } else {
    $("status").textContent = t("forwardingStopped", { name: status.name });
  }
  renderProfiles();
}

async function refreshLogs() {
  const data = await api("/api/logs");
  const lines = Array.isArray(data.lines) ? data.lines : [];
  const text = lines.join("\n");
  const logs = $("logs");
  if (logs) {
    logs.textContent = text;
    logs.scrollTop = logs.scrollHeight;
  }
}

async function loadVersion() {
  const data = await api("/api/version");
  $("appVersion").textContent = `v${data.version}`;
}

async function loadDefaults() {
  defaults = { ...defaults, ...(await api("/api/defaults")) };
  if (!current.id) current = { ...defaults };
}

function normalizeProfile(profile) {
  const normalized = { ...profile };
  if ((normalized.start_command || "").trim() === legacyDefaultStartCommand) {
    normalized.start_command = defaults.start_command;
  }
  return normalized;
}

async function runWithFeedback(message, fn, rollback) {
  setNotice(message, "");
  setBusy(true);
  try {
    const doneMessage = await fn();
    setNotice(doneMessage || t("done"), "ok");
  } catch (err) {
    if (rollback) rollback();
    setNotice(err.message, "error");
    await refreshLogs().catch(() => {});
    alert(err.message);
  } finally {
    setBusy(false);
  }
}

function setNotice(message, kind) {
  const notice = $("notice");
  notice.textContent = message || "";
  notice.className = "notice" + (kind ? ` ${kind}` : "");
}

function setBusy(busy) {
  ["testProfile", "startSession", "stopForwarding", "stopService", "deleteProfile", "newProfile", "refreshProfiles"].forEach((id) => {
    $(id).disabled = busy;
  });
  $("profileForm").querySelector("button[type=submit]").disabled = busy;
}

function editPassword() {
  const input = $("passwordDisplay");
  input.readOnly = false;
  input.value = "";
  input.placeholder = t("passwordPlaceholder");
  $("password").dataset.action = "clear";
  $("editPassword").textContent = t("editing");
  input.focus();
}

function editPassphrase() {
  const input = $("passphraseDisplay");
  input.readOnly = false;
  input.value = "";
  input.placeholder = t("passphrasePlaceholder");
  $("passphrase").dataset.action = "clear";
  $("editPassphrase").textContent = t("editing");
  input.focus();
}

function syncPasswordEdit() {
  const value = $("passwordDisplay").readOnly ? "" : $("passwordDisplay").value;
  $("password").value = value;
  $("password").dataset.action = $("passwordDisplay").readOnly ? "keep" : (value ? "replace" : "clear");
}

function syncPassphraseEdit() {
  const value = $("passphraseDisplay").readOnly ? "" : $("passphraseDisplay").value;
  $("passphrase").value = value;
  $("passphrase").dataset.action = $("passphraseDisplay").readOnly ? "keep" : (value ? "replace" : "clear");
}

function maskEditedSecretsForSave() {
  const passwordWasEditing = !$("passwordDisplay").readOnly;
  const passphraseWasEditing = !$("passphraseDisplay").readOnly;
  const passwordValue = $("password").value;
  const passphraseValue = $("passphrase").value;
  const passwordAction = $("password").dataset.action || "keep";
  const passphraseAction = $("passphrase").dataset.action || "keep";

  if (passwordWasEditing) {
    $("passwordDisplay").readOnly = true;
    $("passwordDisplay").value = passwordValue ? "*******" : "";
    $("passwordDisplay").placeholder = passwordValue ? "" : t("passwordNotSet");
    $("editPassword").textContent = t("edit");
  }
  if (passphraseWasEditing) {
    $("passphraseDisplay").readOnly = true;
    $("passphraseDisplay").value = passphraseValue ? "*******" : "";
    $("passphraseDisplay").placeholder = passphraseValue ? "" : t("passwordNotSet");
    $("editPassphrase").textContent = t("edit");
  }

  return () => {
    if (passwordWasEditing) {
      $("passwordDisplay").readOnly = false;
      $("passwordDisplay").value = passwordValue;
      $("passwordDisplay").placeholder = t("passwordPlaceholder");
      $("password").value = passwordValue;
      $("password").dataset.action = passwordAction;
      $("editPassword").textContent = t("editing");
    }
    if (passphraseWasEditing) {
      $("passphraseDisplay").readOnly = false;
      $("passphraseDisplay").value = passphraseValue;
      $("passphraseDisplay").placeholder = t("passphrasePlaceholder");
      $("passphrase").value = passphraseValue;
      $("passphrase").dataset.action = passphraseAction;
      $("editPassphrase").textContent = t("editing");
    }
  };
}

function toggleAuthFields() {
  const isKey = $("authMethod").value === "private_key";
  document.querySelectorAll(".key-field").forEach((el) => {
    el.style.display = isKey ? "" : "none";
  });
  document.querySelectorAll(".password-field").forEach((el) => {
    el.style.display = isKey ? "none" : "";
  });
}

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;"
  })[ch]);
}

$("newProfile").addEventListener("click", newProfile);
$("refreshProfiles").addEventListener("click", loadProfiles);
$("profileForm").addEventListener("submit", saveProfile);
$("deleteProfile").addEventListener("click", deleteProfile);
$("startSession").addEventListener("click", startSession);
$("testProfile").addEventListener("click", testProfile);
$("stopForwarding").addEventListener("click", stopForwarding);
$("stopService").addEventListener("click", stopService);
if ($("refreshLogs")) $("refreshLogs").addEventListener("click", refreshLogs);
$("authMethod").addEventListener("change", toggleAuthFields);
$("editPassword").addEventListener("click", editPassword);
$("editPassphrase").addEventListener("click", editPassphrase);
$("passwordDisplay").addEventListener("input", syncPasswordEdit);
$("passphraseDisplay").addEventListener("input", syncPassphraseEdit);
$("language").addEventListener("change", () => {
  lang = $("language").value;
  localStorage.setItem("chemweb-launcher-language", lang);
  applyTranslations();
});

applyTranslations();
loadVersion().catch(() => {
  $("appVersion").textContent = "vunknown";
});
loadDefaults()
  .then(loadProfiles)
  .catch((err) => alert(err.message));
refreshStatus();
refreshLogs();
setInterval(refreshStatus, 2000);
setInterval(refreshLogs, 2000);
