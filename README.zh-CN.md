# ChemSSH Launcher 中文说明

[English](README.md) | 中文

ChemSSH Launcher 是 ChemSSH 的桌面启动器和文件传输辅助工具。它可以通过 SSH 在远程服务器上启动 ChemSSH，建立本地端口转发，在浏览器或 Windows WebView2 内嵌窗口里打开 ChemSSH，并提供独立的 SFTP 文件管理页面。

运行时它是一个 Go 单文件程序。除非你使用本地调试 profile，否则不需要本机安装 Python、Electron、系统 `ssh` 命令或 ChemSSH。

## 安装

可以直接使用已经构建好的可执行文件，也可以从源码构建。

Windows 常见可执行文件：

- `chemssh-launcher.exe`：使用系统浏览器的普通版本。
- `chemssh-launcher-webview2.exe`：可选 WebView2 版本，会用内嵌窗口打开 Launcher 和 ChemSSH。

WebView2 版本要求运行机器安装 Microsoft Edge WebView2 Runtime。Windows 10/11 通常已经预装。

<details>
<summary>源码构建（开发者）</summary>

源码构建需要：

- Go 1.23 或更新版本。
- Node.js 和 npm，用于 Vue 前端构建。
- 可选：`go-winres`，用于稳定的离线 Windows 图标和版本资源生成。

```bash
go install github.com/tc-hib/go-winres@v0.3.3
```

</details>

## 启动

双击可执行文件，或运行：

```bash
chemssh-launcher
```

程序会在 `127.0.0.1` 的随机可用端口启动本地 GUI 服务，并打开 Launcher 页面。页面中可以创建 profile、保存 SSH secret、测试连接、启动 ChemSSH、停止转发或远程服务、打开 SFTP、查看日志、导入/导出 profile、管理配置和缓存目录。

WebView2 版本：

```bash
chemssh-launcher-webview2.exe
```

常用 GUI 参数：

```bash
chemssh-launcher --browser
chemssh-launcher --webview
chemssh-launcher --devtools
chemssh-launcher --version
```

环境变量：

- `CHEMSSH_LAUNCHER_WEBVIEW=1` 或 `0`：在支持 WebView 的构建中启用或关闭内嵌模式。
- `CHEMSSH_LAUNCHER_DEVTOOLS=1`：调试 WebView2 页面时启用 DevTools。
- `CHEMSSH_LAUNCHER_VAULT_PASSWORD=...`：系统凭证管理器不可用时，改用本地加密 vault。

## 第一次使用

1. 打开 Launcher。
2. 新建远程 profile，填写 SSH host、port、user 和认证方式。
3. 按需要调整 ChemSSH 的远程监听地址，默认是 `127.0.0.1:8888`。
4. 本地 tunnel 建议继续绑定 `127.0.0.1`，除非你明确想暴露给其他机器。
5. 点击 Test SSH。首次连接未知主机时，Launcher 会要求确认 host key 指纹。
6. 点击 Start。Launcher 会在需要时启动远程 ChemSSH，建立 SSH tunnel，等待 health URL 可访问，然后按 profile 设置打开 ChemSSH 页面。

如果远程 ChemSSH 端口上已经有可复用的 ChemSSH 服务，Launcher 可以只建立转发，而不重复启动一个服务。

## 核心功能

- 通过 SSH 启动远程 ChemSSH，支持密码和私钥认证。
- 在同一个远程 shell session 中执行多行 pre-start/start 命令。
- SSH 本地端口转发、本地/远程端口检查、本地可用端口建议。
- 系统浏览器模式和可选 Windows WebView2 模式。
- 面向内嵌 ChemSSH 的同源 `/chemssh` 代理，包括 WebView2 友好的文件拖拽。
- 远程 profile 和本地调试 profile。
- 按 profile 记录日志，同时提供 launcher/backend 日志。
- 系统凭证管理器保存 secret，加密 vault 兜底，OpenSSH 兼容 `known_hosts`。
- 独立 SFTP 页面 `/sftp`。
- 类似 Xftp 的左右双窗格文件管理，每个窗格可打开本地或远程多标签。
- 文件操作：列目录、打开、用文本编辑器打开、新建文件、新建目录、重命名、删除、复制路径。
- 本地/远程、远程/远程、本地/本地之间传输文件。
- 传输队列、进度、速度/ETA、文件夹分组、暂停、继续、取消。
- 远程文件打开到本地缓存，本地编辑后自动同步回远程。
- 支持系统文件图标，无法获取时使用 fallback 图标。
- Backend 页面可查看配置/缓存路径、导入/导出 profile、清理缓存。

## Profile

Profile 保存在本机，分为远程 profile 和本地 profile。

### 远程 Profile

远程 profile 通过 SSH 连接服务器，并可以在服务器上启动 ChemSSH。

重要字段：

- `SSH Host`、`SSH Port`、`SSH User`：SSH 目标。
- `Auth Method`：`Password` 或 `Private Key`。
- `Remote Host`、`Remote Port`：ChemSSH 在远程机器上的监听地址。
- `Local Host`、`Local Port`：本地 tunnel 的监听地址。
- `Local URL Path`：本地浏览器打开的路径。
- `Pre-start Commands`：启动前执行的可选命令。
- `Start Command`：启动 ChemSSH 的命令。
- `Health Check URL`：可选覆盖值；留空时检查本地浏览器 URL。
- `Open ChemSSH tab after start`：启动成功后是否自动打开 ChemSSH。
- `Security Token`：ChemSSH 安全令牌，用于启用了 token 认证的 ChemSSH 实例。配置后，Launcher 会在代理的 API 请求、终端 WebSocket 连接和 health/identity 检查中自动注入令牌，无需每次手动输入。

已保存的 secret 只显示为 `*******`。编辑 secret 可替换；编辑后留空并保存会清除对应 secret。

### 本地 Profile

本地 profile 用于本机 ChemSSH 调试。它不使用 SSH，不创建 SFTP session，也不创建 SSH tunnel。

本地 profile 中，`Local Host`、`Local Port`、`Local URL Path`、`Pre-start Commands` 和 `Start Command` 描述本机 ChemSSH 进程。如果 health URL 已经可访问，Launcher 会直接复用；否则会用 Windows PowerShell 或 Unix-like 系统的 `sh -c` 执行配置的本地启动命令。

## 默认启动命令

默认远程 `Start Command` 假设服务器有 `bash`、项目 `.venv` 和可用的 `chemssh` 命令：

```bash
source .venv/bin/activate
__chemssh_conda_prompt=""
if [ -n "${CONDA_DEFAULT_ENV:-}" ]; then
  __chemssh_conda_prompt="(${CONDA_DEFAULT_ENV}) "
fi

__chemssh_venv_prompt=""
if [ -n "${VIRTUAL_ENV:-}" ]; then
  __chemssh_venv_prompt="($(basename "$VIRTUAL_ENV")) "
fi

__chemssh_prompt_char="$"
if [ "$(id -u)" = "0" ]; then
  __chemssh_prompt_char="#"
fi

export PS1="${__chemssh_conda_prompt}${__chemssh_venv_prompt}[\u@\h \W]${__chemssh_prompt_char} "
chemssh --config config.yaml
```

如果命令中还没有 `--host` 或 `--port`，Launcher 会自动追加：

```bash
--host <Remote Host> --port <Remote Port>
```

## SFTP 与本地文件

SFTP 页面与 ChemSSH 启动流程解耦，可以从 Launcher 打开，也可以访问：

```text
/sftp
```

页面左右两侧各自维护多个标签页。每个标签页可以浏览本地文件系统，或通过已保存远程 profile 建立 SSH/SFTP 连接。远程 SFTP 标签页需要远程 profile 和 host key 校验，但不会启动 ChemSSH、执行 start command、创建 tunnel，也不依赖 ChemSSH health check。

支持的传输方向：

- 本地到远程。
- 远程到本地。
- 远程到远程，通过 Launcher 中继。
- 本地到本地，通过 Launcher 复制。
- 从系统文件管理器拖拽文件/文件夹上传到远程标签页。

通过 `Open` 或 `Open in text editor` 打开的远程文件会先下载到本地缓存目录，再使用系统文件关联或文本编辑器打开。只要 SFTP session 仍保持连接，Launcher 会监控缓存文件变化并同步上传回原远程路径。缓存是临时的，Launcher 退出时会尽量清理。

目录传输通过前端展开为文件任务实现。部分后端路径仍只支持单文件复制，因此本地到本地的目录传输可能会报 `directory transfer is not supported yet`。

## ChemSSH Bridge

Launcher 为 ChemSSH 暴露 bridge 接口：

```text
/api/chemssh-bridge/*
```

这些接口让通过代理打开的 ChemSSH 页面可以发现 Launcher 能力，请求系统文件图标，用本地应用打开 workspace 文件，用文本编辑器打开文件，并轮询本地编辑后的同步事件。

Bridge 能力有条件：

- ChemSSH 必须通过当前 Launcher 的代理打开，通常是 `/chemssh`。
- 当前 session 必须仍在 forwarding。
- Launcher 必须能读取 ChemSSH identity，并且 identity 中要包含 workspace root。
- 远程 bridge open 会为当前远程 profile 建立专用 SFTP session。
- 本地 profile 只能打开本地 ChemSSH workspace 内的文件，不提供 SFTP bridge open。

Bridge 会限制在当前 ChemSSH workspace 内；workspace 外路径会被拒绝。

## WebView2 与浏览器模式

浏览器模式会用系统浏览器打开 Launcher 本地 URL。

WebView2 模式会在内嵌窗口中打开 `/shell`。普通 WebView2 模式会禁用 DevTools 快捷键并拦截 `Ctrl+Shift+C`；只有调试时才应使用 `--devtools`。如果仍有传输任务运行，WebView2 关闭拦截可以提示用户。

WebView2 模式下，ChemSSH 通过 Launcher 同源代理打开：

```text
http://127.0.0.1:<launcher-port>/chemssh
```

而不是直接打开：

```text
http://127.0.0.1:8888
```

这样可以避开 WebView2 跨源 iframe 的拖拽限制，也让 Launcher 代理参与 ChemSSH 文件操作。

## 存储位置

非 secret 的 profile 配置保存为 JSON：

- Windows: `%AppData%\ChemSSHLauncher\profiles.json`
- macOS: `~/Library/Application Support/ChemSSHLauncher/profiles.json`
- Linux: `~/.config/chemssh-launcher/profiles.json`

密码、私钥 passphrase 和安全令牌不会写入 `profiles.json`。默认保存到系统凭证管理器：

- service: `chemssh-launcher`
- username: `<profile-id>:password`
- username: `<profile-id>:key-passphrase`
- username: `<profile-id>:security-token`

如果系统凭证管理器不可用，可以设置 `CHEMSSH_LAUNCHER_VAULT_PASSWORD`，改用本地加密 vault：

- Windows: `%AppData%\ChemSSHLauncher\vault.json`
- macOS: `~/Library/Application Support/ChemSSHLauncher/vault.json`
- Linux: `~/.config/chemssh-launcher/vault.json`

vault 密码不会被程序保存。

SSH host key 保存为 OpenSSH 兼容格式：

- Windows: `%AppData%\ChemSSHLauncher\known_hosts`
- macOS: `~/Library/Application Support/ChemSSHLauncher/known_hosts`
- Linux: `~/.config/chemssh-launcher/known_hosts`

WebView2 数据目录和临时 SFTP 打开缓存目录可以在 Backend 页面查看。Windows 上缓存清理可能被文件锁阻塞；Launcher 会记录 pending marker，并在下次启动时继续尝试。

## 安全说明

ChemSSH Launcher 依赖 SSH 完成认证、加密、host key 校验和本地端口转发。Launcher 自身负责 profile 存储、secret 查询、远程/本地进程启动、SFTP session 和本地 HTTP 代理。

首次连接服务器时，GUI/CLI 会显示 host key 类型和 SHA256 指纹。请只在你已经通过可信来源确认后再信任，例如服务器管理员或一台已经信任该服务器的 SSH 客户端。如果已保存的 host key 后续变化，Launcher 会阻止连接。

如果把 `Local Host` 绑定到非 loopback 地址，例如 `0.0.0.0`，可能会把 tunnel 或本地 ChemSSH 目标暴露给其他机器。Launcher 会记录警告，但这个设置仍由使用者负责。

---

<details>
<summary>架构概览（开发者）</summary>

项目由 Go 后端、嵌入式 Vue 前端、SSH/SFTP 客户端和可选 WebView2 宿主组成：

```text
chemssh-launcher executable
  Go GUI server on 127.0.0.1:<random>
    Vue app: /, /shell, /sftp, /launcher-logs
    Profile/session APIs: /api/profiles, /api/session/*
    SFTP/local file APIs: /api/sftp/*, /api/local/*
    ChemSSH proxy: /chemssh and related proxied paths
    ChemSSH bridge: /api/chemssh-bridge/*
  SSH client
    remote command startup
    local port forwarding
    host-key verification
  SFTP client manager
    reusable SFTP tabs
    dedicated bridge SFTP session
    open-cache sync watcher
  Browser or WebView2 host
```

前端使用 Vue 3、TypeScript、Vite 和 Element Plus。生产构建输出到 `internal/gui/static/vue/`，再嵌入到 Go 可执行文件中。

</details>

<details>
<summary>目录结构（开发者）</summary>

```text
cmd/chemssh-launcher/      CLI 入口和 Windows resource object
frontend/                 Vue 3 前端、Vite 配置、package 配置
frontend/src/views/       Launcher、SFTP、Shell、Backend 页面
internal/app/             应用/session 辅助逻辑
internal/browser/         浏览器和 OS 文件打开辅助
internal/chemssh/         ChemSSH identity 读取
internal/config/          Profile、默认值、路径和应用目录
internal/fileicon/        系统/fallback 文件图标服务
internal/gui/             本地 GUI HTTP server、API、proxy、bridge、SFTP/本地文件处理
internal/netcheck/        端口和 health check
internal/secret/          系统 keyring、加密 vault、测试用 memory store
internal/sftpclient/      SFTP session 管理和文件操作
internal/sshclient/       SSH 认证、host key、tunnel、远程命令
internal/version/         版本源文件和测试
internal/webview/         可选 WebView2 集成
tools/build/              前端、资源和 Go binary 的构建编排
winres/                   Windows 图标和版本资源元数据
idea/                     开发任务说明和设计记录
```

</details>

<details>
<summary>开发指南（开发者）</summary>

首次安装前端依赖：

```bash
cd frontend
npm install
```

前端类型检查和构建：

```bash
npm run build
```

运行 Go 测试：

```bash
go test ./...
```

在仓库根目录构建 Launcher：

```bash
go run ./tools/build --optimize 
```

构建工具会读取 `internal/version/VERSION`，同步 `winres/winres.json`，构建 Vue 前端，在需要时重新生成 Windows 资源，然后执行 `go build ./cmd/chemssh-launcher`。

构建可选 Windows WebView2 版本：

```bash
go run ./tools/build --webview2                   # WebView2 带控制台（调试用）
go run ./tools/build --webview2 --windowsgui      # WebView2 无控制台（推荐发布版）
```

构建工具默认启用体积和启动速度优化（使用 `-trimpath -ldflags="-s -w"`）。调试时可使用 `--no-optimize` 禁用优化。

**构建版本说明**：
- **标准版（浏览器模式）**：在系统浏览器中打开，显示控制台便于调试
- **标准版 + `--windowsgui`**：在系统浏览器中打开，无控制台窗口（用户友好）
- **WebView2 版**：内嵌浏览器窗口，带控制台（用于调试启动问题）
- **WebView2 + `--windowsgui`**：内嵌浏览器窗口，无控制台（最佳用户体验）

本地前端迭代可以运行 Vite：

```bash
cd frontend
npm run dev
```

生产可执行文件使用嵌入资源，因此测试打包后的 Go server 前，需要先运行 `npm run build` 或 `go run ./tools/build`。

</details>

<details>
<summary>CLI 辅助命令（开发者）</summary>

GUI 是主要入口，CLI 保留用于调试和自动化：

```bash
chemssh-launcher profile list
chemssh-launcher profile add
chemssh-launcher profile edit <name-or-id>
chemssh-launcher profile delete <name-or-id>
chemssh-launcher profile test <name-or-id>
chemssh-launcher start <name-or-id>
```

</details>

<details>
<summary>版本管理（开发者）</summary>

版本号只需要修改：

```text
internal/version/VERSION
```

`go run ./tools/build` 会把版本同步到 Windows 资源信息中。版本号会显示在 GUI 侧边栏，也可以用命令查看：

```bash
chemssh-launcher --version
```

</details>

<details>
<summary>已知限制和注意事项</summary>

- 运行时是单文件程序，但源码构建需要 Node/npm，因为当前前端是 Vue/Vite 应用。
- SFTP 只适用于远程 profile。本地 profile 用于本地 ChemSSH 调试，不提供 SSH/SFTP。
- Bridge 文件打开依赖当前 active proxied ChemSSH session，并且 ChemSSH identity 中需要有 `workspace_root`。
- Launcher 只能停止自己启动的 ChemSSH，或能通过 ChemSSH identity 识别 PID 的服务。如果 PID 读取失败，停止服务时可能留下远程 ChemSSH 进程。
- 远程和本地递归删除由后端支持，删除确认需要仔细阅读。
- 临时 SFTP open cache 清理可能被 Windows 文件锁阻塞；这属于环境清理问题，不一定表示测试或代码失败。
- `go.sum` 应提交到仓库。它用于锁定 Go 依赖校验和，保证构建可复现，并降低依赖被篡改的风险。

</details>
