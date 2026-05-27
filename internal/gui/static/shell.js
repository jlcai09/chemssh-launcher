let nextID = 1;
let activeID = "";
let lastSessionURL = "";
let menuTabID = "";

const tabs = new Map();
const $ = (id) => document.getElementById(id);

function absoluteURL(value) {
  const text = normalizeAddressInput(value);
  if (!text) return "about:blank";
  if (text === "about:blank") return text;
  if (/^[a-zA-Z][a-zA-Z\d+.-]*:/.test(text)) return text;
  if (text.startsWith("/")) return new URL(text, window.location.origin).href;
  if (text.includes(".") || text.includes(":")) return `http://${text}`;
  return new URL(text, window.location.origin).href;
}

function normalizeAddressInput(value) {
  return String(value || "")
    .trim()
    .replace(/[\u3002\uff0e\uff61]/g, ".");
}

function shortTitle(url) {
  if (url === "about:blank") return "新标签页";
  try {
    const parsed = new URL(url);
    if (parsed.origin === window.location.origin) {
      if (parsed.pathname === "/") return "启动器";
      if (parsed.pathname === "/launcher-logs") return "启动器后台";
    }
    return parsed.hostname || url;
  } catch (_) {
    return url;
  }
}

function tabList() {
  return Array.from(document.querySelectorAll(".tab[data-id]"))
    .map((tab) => tabs.get(tab.dataset.id))
    .filter(Boolean);
}

function createTab(title, url, pinned = false, focus = true, afterID = "") {
  const id = `tab-${nextID++}`;
  const finalURL = absoluteURL(url);

  const tab = document.createElement("button");
  tab.className = "tab" + (pinned ? " pinned" : "");
  tab.type = "button";
  tab.dataset.id = id;
  tab.setAttribute("role", "tab");

  const label = document.createElement("span");
  label.className = "tab-title";
  label.textContent = title || shortTitle(finalURL);
  tab.appendChild(label);

  if (!pinned) {
    const close = document.createElement("button");
    close.className = "tab-close";
    close.type = "button";
    close.textContent = "×";
    close.title = "关闭标签页";
    close.addEventListener("click", (event) => {
      event.stopPropagation();
      closeTab(id);
    });
    tab.appendChild(close);
  }

  const page = document.createElement("section");
  page.className = "page";
  page.dataset.id = id;
  const iframe = document.createElement("iframe");
  iframe.src = finalURL;
  iframe.title = title || shortTitle(finalURL);
  iframe.addEventListener("load", () => syncLoadedURL(id));
  page.appendChild(iframe);

  tab.addEventListener("click", () => activateTab(id));
  tab.addEventListener("contextmenu", (event) => openTabMenu(event, id));

  const after = afterID ? tabs.get(afterID) : null;
  if (after) {
    after.tab.after(tab);
  } else {
    $("tabs").insertBefore(tab, $("newTab"));
  }
  $("pages").appendChild(page);
  tabs.set(id, {
    id,
    tab,
    page,
    iframe,
    label,
    pinned,
    url: finalURL,
    history: [finalURL],
    historyIndex: 0
  });
  if (focus) activateTab(id);
  updateNavButtons();
  return id;
}

function activateTab(id) {
  const item = tabs.get(id);
  if (!item) return;
  activeID = id;
  for (const tab of tabs.values()) {
    const active = tab.id === id;
    tab.tab.classList.toggle("active", active);
    tab.tab.setAttribute("aria-selected", active ? "true" : "false");
    tab.page.classList.toggle("active", active);
  }
  $("address").value = item.url;
  updateNavButtons();
}

function closeTab(id) {
  const item = tabs.get(id);
  if (!item || item.pinned) return;
  const wasActive = activeID === id;
  const ordered = tabList();
  const index = ordered.findIndex((tab) => tab.id === id);
  item.tab.remove();
  item.page.remove();
  tabs.delete(id);
  if (wasActive) {
    const fallback = ordered[index - 1] || ordered[index + 1] || tabList().at(-1);
    if (fallback && tabs.has(fallback.id)) activateTab(fallback.id);
  }
  updateNavButtons();
}

function navigateTab(id, url, addHistory = true) {
  const item = tabs.get(id);
  if (!item) return;
  const finalURL = absoluteURL(url);
  item.url = finalURL;
  item.iframe.src = finalURL;
  item.label.textContent = shortTitle(finalURL);
  if (addHistory) {
    item.history = item.history.slice(0, item.historyIndex + 1);
    item.history.push(finalURL);
    item.historyIndex = item.history.length - 1;
  }
  if (id === activeID) {
    $("address").value = item.url;
    updateNavButtons();
  }
}

function navigateActive(url) {
  navigateTab(activeID, url, true);
}

function reloadTab(id) {
  const item = tabs.get(id);
  if (item) item.iframe.src = item.url;
}

function goBack() {
  const item = tabs.get(activeID);
  if (!item || item.historyIndex <= 0) return;
  item.historyIndex -= 1;
  navigateTab(activeID, item.history[item.historyIndex], false);
}

function goForward() {
  const item = tabs.get(activeID);
  if (!item || item.historyIndex >= item.history.length - 1) return;
  item.historyIndex += 1;
  navigateTab(activeID, item.history[item.historyIndex], false);
}

function updateNavButtons() {
  const item = tabs.get(activeID);
  $("back").disabled = !item || item.historyIndex <= 0;
  $("forward").disabled = !item || item.historyIndex >= item.history.length - 1;
}

function syncLoadedURL(id) {
  const item = tabs.get(id);
  if (!item) return;
  try {
    const loaded = item.iframe.contentWindow.location.href;
    if (loaded && loaded !== "about:blank" && loaded !== item.url) {
      item.url = loaded;
      item.label.textContent = shortTitle(loaded);
      item.history = item.history.slice(0, item.historyIndex + 1);
      item.history.push(loaded);
      item.historyIndex = item.history.length - 1;
      if (id === activeID) $("address").value = loaded;
    }
  } catch (_) {
  }
  updateNavButtons();
}

function findTabByURL(url) {
  const finalURL = absoluteURL(url);
  return tabList().find((tab) => tab.url === finalURL);
}

function openOrFocus(title, url) {
  const existing = findTabByURL(url);
  if (existing) {
    existing.label.textContent = title || existing.label.textContent;
    activateTab(existing.id);
    return existing.id;
  }
  return createTab(title, url, false, true);
}

function openTabMenu(event, id) {
  event.preventDefault();
  hideEditMenu();
  menuTabID = id;
  const item = tabs.get(id);
  const menu = $("tabMenu");
  menu.querySelector('[data-action="close"]').disabled = !item || item.pinned;
  menu.querySelector('[data-action="closeRight"]').disabled = !closableRightTabs(id).length;
  menu.querySelector('[data-action="closeOthers"]').disabled = !closableOtherTabs(id).length;
  menu.hidden = false;
  const left = Math.min(event.clientX, window.innerWidth - menu.offsetWidth - 8);
  const top = Math.min(event.clientY, window.innerHeight - menu.offsetHeight - 8);
  menu.style.left = `${Math.max(8, left)}px`;
  menu.style.top = `${Math.max(8, top)}px`;
}

function hideTabMenu() {
  $("tabMenu").hidden = true;
}

function openEditMenu(event) {
  if (event.defaultPrevented) {
    return;
  }
  if (event.target.closest(".context-menu")) {
    return;
  }
  if (event.target.closest(".browser-chrome") && event.target !== $("address")) {
    return;
  }
  event.preventDefault();
  hideTabMenu();
  const menu = $("editMenu");
  menu.hidden = false;
  const left = Math.min(event.clientX, window.innerWidth - menu.offsetWidth - 8);
  const top = Math.min(event.clientY, window.innerHeight - menu.offsetHeight - 8);
  menu.style.left = `${Math.max(8, left)}px`;
  menu.style.top = `${Math.max(8, top)}px`;
}

function hideEditMenu() {
  $("editMenu").hidden = true;
}

function closableRightTabs(id) {
  const ordered = tabList();
  const index = ordered.findIndex((tab) => tab.id === id);
  return ordered.slice(index + 1).filter((tab) => !tab.pinned);
}

function closableOtherTabs(id) {
  return tabList().filter((tab) => tab.id !== id && !tab.pinned);
}

function duplicateTab(id) {
	const item = tabs.get(id);
	if (!item) return;
	createTab(item.label.textContent || shortTitle(item.url), item.url, false, true, id);
}

function handleMenuAction(action) {
  const id = menuTabID;
	const item = tabs.get(id);
	if (!item) return;
	if (action === "newRight") createTab("新标签页", "about:blank", false, true, id);
	if (action === "reload") reloadTab(id);
	if (action === "copy") duplicateTab(id);
  if (action === "close") closeTab(id);
  if (action === "closeRight") closableRightTabs(id).forEach((tab) => closeTab(tab.id));
  if (action === "closeOthers") closableOtherTabs(id).forEach((tab) => closeTab(tab.id));
}

async function handleEditAction(action) {
  if (document.activeElement === $("address")) {
    await handleAddressEditAction(action);
    return;
  }
  const item = tabs.get(activeID);
  if (!item) return;
  item.iframe.focus();
  try {
    if (action === "copy") item.iframe.contentWindow.document.execCommand("copy");
    if (action === "paste") {
      const text = await navigator.clipboard.readText();
      item.iframe.contentWindow.document.execCommand("insertText", false, text);
    }
    if (action === "selectAll") item.iframe.contentWindow.document.execCommand("selectAll");
  } catch (_) {
    if (action === "copy") document.execCommand("copy");
    if (action === "paste") document.execCommand("paste");
    if (action === "selectAll") document.execCommand("selectAll");
  }
}

async function handleAddressEditAction(action) {
  const input = $("address");
  input.focus();
  if (action === "copy") {
    const selected = input.value.slice(input.selectionStart, input.selectionEnd) || input.value;
    try {
      await navigator.clipboard.writeText(selected);
    } catch (_) {
      document.execCommand("copy");
    }
  }
  if (action === "paste") {
    try {
      const text = await navigator.clipboard.readText();
      const start = input.selectionStart ?? input.value.length;
      const end = input.selectionEnd ?? input.value.length;
      input.value = input.value.slice(0, start) + text + input.value.slice(end);
      input.selectionStart = input.selectionEnd = start + text.length;
    } catch (_) {
      document.execCommand("paste");
    }
  }
  if (action === "selectAll") {
    input.select();
  }
}

async function pollSession() {
  try {
    const res = await fetch("/api/session/status");
    const status = await res.json();
    const url = status.forwarding && status.url ? status.url : "";
    if (url && status.open_browser !== false && url !== lastSessionURL) {
      lastSessionURL = url;
      openOrFocus(status.name || "Chemweb", url);
      return;
    }
    if (!url) lastSessionURL = "";
  } catch (_) {
  }
}

$("addressForm").addEventListener("submit", (event) => {
  event.preventDefault();
  $("address").value = normalizeAddressInput($("address").value);
  navigateActive($("address").value);
});
$("reload").addEventListener("click", () => reloadTab(activeID));
$("newTab").addEventListener("click", () => createTab("新标签页", "about:blank", false, true));
$("back").addEventListener("click", goBack);
$("forward").addEventListener("click", goForward);
$("tabMenu").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button || button.disabled) return;
  handleMenuAction(button.dataset.action);
  hideTabMenu();
});
$("editMenu").addEventListener("click", (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button || button.disabled) return;
  handleEditAction(button.dataset.action);
  hideEditMenu();
});
document.addEventListener("contextmenu", openEditMenu);
document.addEventListener("click", () => {
  hideTabMenu();
  hideEditMenu();
});
window.addEventListener("blur", () => {
  hideTabMenu();
  hideEditMenu();
});
window.addEventListener("keydown", (event) => {
  if (event.key === "Escape") {
    hideTabMenu();
    hideEditMenu();
  }
});
window.addEventListener("message", (event) => {
  const data = event.data || {};
  if (data.type === "chemweb-launcher:new-tab" && data.url) {
    openOrFocus(shortTitle(data.url), data.url);
  }
});

createTab("启动器后台", "/launcher-logs", true, false);
createTab("启动器", "/", true, true);
pollSession();
setInterval(pollSession, 1500);
