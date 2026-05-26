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

const $ = (id) => document.getElementById(id);

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
    empty.textContent = "No profiles yet.";
    nav.appendChild(empty);
    return;
  }

  profiles.forEach((profile) => {
    const button = document.createElement("button");
    button.className = "profile-item" + (profile.id === current.id ? " active" : "");
    const running = profile.id === activeProfileId;
    button.innerHTML = `<span class="state-dot ${running ? "running" : "stopped"}" title="${running ? "Active" : "Stopped"}"></span><span class="profile-text"><strong>${escapeHtml(profile.name || "(unnamed)")}</strong><span>${escapeHtml(profile.ssh_user || "?")}@${escapeHtml(profile.ssh_host || "?")} - ${escapeHtml(profile.local_host)}:${profile.local_port}</span></span>`;
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
  $("passwordDisplay").placeholder = current.has_password ? "" : "not set";
  $("editPassword").textContent = "Edit";
  $("passphraseDisplay").readOnly = true;
  $("passphraseDisplay").value = current.has_private_key_passphrase ? "*******" : "";
  $("passphraseDisplay").placeholder = current.has_private_key_passphrase ? "" : "not set";
  $("editPassphrase").textContent = "Edit";
  $("heading").textContent = current.name || "New Profile";
  $("summary").textContent = current.id ? `${current.ssh_user || "user"}@${current.ssh_host || "host"} - ${current.local_host}:${current.local_port}` : "Create a remote server profile.";
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
  await runWithFeedback("Saving profile...", async () => {
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
    return "Profile saved.";
  }, rollback);
}

async function deleteProfile() {
  if (!current.id) return;
  if (!confirm(`Delete profile "${current.name}"?`)) return;
  await runWithFeedback("Deleting profile...", async () => {
    await api(`/api/profiles/${current.id}`, { method: "DELETE" });
    current = { ...defaults };
    await loadProfiles();
    return "Profile deleted.";
  });
}

async function startSession() {
  if (!current.id) {
    alert("Save the profile before starting.");
    return;
  }
  await runWithFeedback("Starting session...", async () => {
    await apiWithHostKeyPrompt("/api/session/start", { id: current.id });
    await refreshStatus();
    await refreshLogs();
    return "Session start requested. Watch logs for SSH, command, tunnel, and health check progress.";
  });
}

async function testProfile() {
  if (!current.id) {
    alert("Save the profile before testing.");
    return;
  }
  await runWithFeedback("Testing SSH...", async () => {
    await apiWithHostKeyPrompt("/api/profile-test", { id: current.id });
    await refreshLogs();
    return "SSH and port checks OK.";
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
      `First SSH connection to ${hostKey.address}.`,
      "",
      `Host key: ${hostKey.key_type}`,
      `Fingerprint: ${hostKey.fingerprint}`,
      "",
      "Only trust this key if it matches the server's SSH fingerprint from a reliable source.",
      `It will be saved to: ${hostKey.known_hosts_path}`
    ].join("\n"));
    if (!ok) throw err;
    return await api(path, {
      method: "POST",
      body: JSON.stringify({ ...payload, accept_host_key: true })
    });
  }
}

async function stopSession() {
  await runWithFeedback("Stopping session...", async () => {
    await api("/api/session/stop", { method: "POST" });
    await refreshStatus();
    await refreshLogs();
    return "Stop requested.";
  });
}

async function refreshStatus() {
  const status = await api("/api/session/status");
  activeProfileId = status.running ? status.id : "";
  $("status").textContent = status.running ? `Running: ${status.name}` : "Idle";
  renderProfiles();
}

async function refreshLogs() {
  const data = await api("/api/logs");
  const lines = Array.isArray(data.lines) ? data.lines : [];
  $("logs").textContent = lines.join("\n");
  $("logs").scrollTop = $("logs").scrollHeight;
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
    setNotice(doneMessage || "Done.", "ok");
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
  ["testProfile", "startSession", "stopSession", "deleteProfile", "newProfile", "refreshProfiles"].forEach((id) => {
    $(id).disabled = busy;
  });
  $("profileForm").querySelector("button[type=submit]").disabled = busy;
}

function editPassword() {
  const input = $("passwordDisplay");
  input.readOnly = false;
  input.value = "";
  input.placeholder = "enter new password; leave empty to clear";
  $("password").dataset.action = "clear";
  $("editPassword").textContent = "Editing";
  input.focus();
}

function editPassphrase() {
  const input = $("passphraseDisplay");
  input.readOnly = false;
  input.value = "";
  input.placeholder = "enter new passphrase; leave empty to clear";
  $("passphrase").dataset.action = "clear";
  $("editPassphrase").textContent = "Editing";
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
    $("passwordDisplay").placeholder = passwordValue ? "" : "not set";
    $("editPassword").textContent = "Edit";
  }
  if (passphraseWasEditing) {
    $("passphraseDisplay").readOnly = true;
    $("passphraseDisplay").value = passphraseValue ? "*******" : "";
    $("passphraseDisplay").placeholder = passphraseValue ? "" : "not set";
    $("editPassphrase").textContent = "Edit";
  }

  return () => {
    if (passwordWasEditing) {
      $("passwordDisplay").readOnly = false;
      $("passwordDisplay").value = passwordValue;
      $("passwordDisplay").placeholder = "enter new password; leave empty to clear";
      $("password").value = passwordValue;
      $("password").dataset.action = passwordAction;
      $("editPassword").textContent = "Editing";
    }
    if (passphraseWasEditing) {
      $("passphraseDisplay").readOnly = false;
      $("passphraseDisplay").value = passphraseValue;
      $("passphraseDisplay").placeholder = "enter new passphrase; leave empty to clear";
      $("passphrase").value = passphraseValue;
      $("passphrase").dataset.action = passphraseAction;
      $("editPassphrase").textContent = "Editing";
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
$("stopSession").addEventListener("click", stopSession);
$("refreshLogs").addEventListener("click", refreshLogs);
$("authMethod").addEventListener("change", toggleAuthFields);
$("editPassword").addEventListener("click", editPassword);
$("editPassphrase").addEventListener("click", editPassphrase);
$("passwordDisplay").addEventListener("input", syncPasswordEdit);
$("passphraseDisplay").addEventListener("input", syncPassphraseEdit);

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
