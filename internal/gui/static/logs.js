async function refreshLogs() {
  const res = await fetch("/api/launcher-logs");
  const data = await res.json();
  const lines = Array.isArray(data.lines) ? data.lines : [];
  const logs = document.getElementById("logs");
  logs.textContent = lines.join("\n");
  logs.scrollTop = logs.scrollHeight;
}

document.getElementById("refreshLogs").addEventListener("click", refreshLogs);
refreshLogs();
setInterval(refreshLogs, 1500);
