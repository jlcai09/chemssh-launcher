# Chemweb Launcher 中文说明

[English](README.md) | 中文

Chemweb Launcher 是一个用 Go 编写的小型 GUI 启动器，用来通过 SSH 在远程 Linux 服务器或 HPC 登录节点上启动 Chemweb，并在本机浏览器里访问。

程序不依赖 Python、Node、Electron 或系统 `ssh` 命令。直接运行后会启动一个只绑定在 `127.0.0.1` 的本地 Web GUI，并自动打开浏览器。

## 用法

双击或直接运行：

```bash
chemweb-launcher
```

页面里可以新建/编辑/删除服务器 profile，保存 SSH 密码或私钥 passphrase，测试 SSH，启动远程 Chemweb，建立本地 SSH tunnel，查看日志，并停止当前 session。

Windows 下如果使用带 WebView2 的构建，默认不依赖系统浏览器，而是用内嵌窗口打开启动器和 Chemweb 页面：

```bash
chemweb-launcher-webview2.exe
```

`--browser` 可以强制使用系统浏览器。`--webview` 可以在支持 WebView2 的构建中显式请求内嵌模式。`--devtools` 仅用于调试 WebView2 内嵌页面。普通 WebView2 模式会关闭 DevTools 快捷键，并拦截 `Ctrl+Shift+C`，避免它打开 DevTools。

WebView2 模式下，Chemweb 会通过启动器自己的同源代理打开，而不是直接打开 tunnel 地址。例如启动器日志显示 `GUI: http://127.0.0.1:63456` 时，Chemweb 页面会使用 `http://127.0.0.1:63456/chemweb`，不是 `http://127.0.0.1:8888`。这样可以避开 WebView2 跨源 iframe 的外拖限制，通过代理页面把 Chemweb 文件拖拽到桌面。

## 常用字段

`SSH Host`、`SSH Port`、`SSH User` 用于连接远程服务器。

`Auth Method` 支持 `Password` 和 `Private Key`。

`Secrets` 用于保存 SSH 密码或私钥 passphrase。已保存的 secret 只显示为 `*******`，不会明文展示。点击 `Edit` 可以替换；编辑后留空并保存会清除对应 secret。

`Remote Host`、`Remote Port` 是 Chemweb 在远程机器上的监听地址，默认 `127.0.0.1:8888`。

`Local Host`、`Local Port` 是本机 tunnel 的监听地址，默认 `127.0.0.1:8888`。本地浏览器访问的是这个地址。

`Local URL Path` 是本机浏览器打开的路径，默认 `/`。如果 Chemweb 入口在 `/chemweb/` 之类的子路径，就填对应路径。

`Health Check URL` 通常留空。留空时程序检查 `http://<Local Host>:<Local Port><Local URL Path>`。只有服务有专门的 `/health`、`/status` 等检查地址时才需要填写。

## 启动命令

默认 `Start Command` 是：

```bash
source .venv/bin/activate
__chemweb_conda_prompt=""
if [ -n "${CONDA_DEFAULT_ENV:-}" ]; then
  __chemweb_conda_prompt="(${CONDA_DEFAULT_ENV}) "
fi

__chemweb_venv_prompt=""
if [ -n "${VIRTUAL_ENV:-}" ]; then
  __chemweb_venv_prompt="($(basename "$VIRTUAL_ENV")) "
fi

__chemweb_prompt_char="$"
if [ "$(id -u)" = "0" ]; then
  __chemweb_prompt_char="#"
fi

export PS1="${__chemweb_conda_prompt}${__chemweb_venv_prompt}[\u@\h \W]${__chemweb_prompt_char} "
chemweb --config config.yaml
```

程序运行时会自动追加：

```bash
--host <Remote Host> --port <Remote Port>
```

如果你已经手动写了 `--host` 或 `--port`，程序不会重复追加。默认命令假设远程机器有 `bash`，并且当前目录存在 `.venv`。

## 端口检查

`Test SSH` 和 `Start` 会区分本地端口与远程端口：

- 本地端口占用会显示 `local port occupied`，并建议附近可用端口。
- 远程端口已经被服务监听会显示 `remote port occupied`。
- SSH 认证、网络、host key 校验失败会显示对应 SSH 错误。

## 安全性

连接安全性由 SSH 协议和 `golang.org/x/crypto/ssh` 实现保证，包括认证、加密传输和本地端口转发。Launcher 自身负责读取配置、登录、启动远程服务、停止远程服务和转发端口。

程序现在默认启用 SSH host key 校验，行为类似 OpenSSH/Paramiko 的安全模型：

- 首次连接未知主机时，GUI/CLI 会显示 host key 类型和 SHA256 指纹。
- 只有你确认该指纹来自可信来源后，程序才会把它写入 `known_hosts`。
- 后续连接必须匹配已保存的 host key。
- 如果 host key 变化，程序会阻止连接，因为这可能表示服务器重装、域名/IP 指向变化，或中间人攻击。

请不要随手信任首次连接弹出的指纹。推荐向服务器管理员确认，或和一台已经信任该服务器的 OpenSSH 客户端显示的指纹对比。

## 存储位置

非 secret 的 SSH/profile 配置保存为 JSON：

- Windows: `%AppData%\ChemwebLauncher\profiles.json`
- macOS: `~/Library/Application Support/ChemwebLauncher/profiles.json`
- Linux: `~/.config/chemweb-launcher/profiles.json`

SSH host key 保存为 OpenSSH 兼容格式：

- Windows: `%AppData%\ChemwebLauncher\known_hosts`
- macOS: `~/Library/Application Support/ChemwebLauncher/known_hosts`
- Linux: `~/.config/chemweb-launcher/known_hosts`

密码和私钥 passphrase 不会写入 `profiles.json`。默认保存到系统凭证管理器：

- service: `chemweb-launcher`
- username: `<profile-id>:password`
- username: `<profile-id>:key-passphrase`

这通常比自己在应用里写一套加密逻辑更安全，因为系统凭证管理器会使用操作系统的账户隔离、加密和访问控制。

如果系统凭证管理器不可用，可以设置环境变量 `CHEMWEB_LAUNCHER_VAULT_PASSWORD`，程序会改用本地加密 vault：

- Windows: `%AppData%\ChemwebLauncher\vault.json`
- macOS: `~/Library/Application Support/ChemwebLauncher/vault.json`
- Linux: `~/.config/chemweb-launcher/vault.json`

vault 密码不会被程序保存。

## CLI 辅助命令

GUI 是主要入口，CLI 保留用于调试和自动化：

```bash
chemweb-launcher profile list
chemweb-launcher profile add
chemweb-launcher profile edit <name-or-id>
chemweb-launcher profile delete <name-or-id>
chemweb-launcher profile test <name-or-id>
chemweb-launcher start <name-or-id>
```

## 开发依赖

开发和构建只需要安装 Go 1.22 或更新版本。

本项目不需要 Python、Node、Electron，也不需要系统 `ssh` 命令。Go 会在首次构建或测试时自动下载 `go.mod` 里的模块依赖。

Windows 图标和版本资源由构建工具自动生成。为了获得稳定的离线构建体验，建议先一次性安装 `go-winres`：

```bash
go install github.com/tc-hib/go-winres@v0.3.3
```

当需要重新生成 Windows 资源时，构建工具会优先使用已安装的 `go-winres`。如果没有安装，则回退到 `go run github.com/tc-hib/go-winres@v0.3.3`，这一步可能需要联网。如果构建 WebView2 版本，运行机器还需要 Microsoft Edge WebView2 Runtime；Windows 10/11 通常已经预装。

## 构建

```bash
go run ./tools/build
```

Windows 下会生成 `chemweb-launcher.exe`。构建工具会先读取 `internal/version/VERSION`，同步 `winres/winres.json`，并重新生成 Windows 资源文件。

默认构建不包含 WebView2。Windows 下可以单独构建 WebView2 版本：

```bash
go run ./tools/build --webview2
```

如果希望双击 WebView2 版本时不显示 PowerShell/控制台窗口，使用 Windows GUI 子系统构建：
```bash
go run ./tools/build --webview2 --windowsgui
```

不带 `-H=windowsgui` 的控制台版本仍然适合调试，因为启动错误会打印到终端。

这样可以同时发布普通版本和 WebView2 版本。

## 版本管理

版本号只需要修改：

```text
internal/version/VERSION
```

使用 `go run ./tools/build` 构建时，会自动把这个版本同步到 Windows 资源信息。版本号会显示在 GUI 左侧栏，也可以用命令查看：

```bash
chemweb-launcher --version
```

## 仓库说明

`go.sum` 应该提交到仓库。它用于锁定 Go 依赖校验和，保证构建可复现，也能降低依赖被篡改的风险。
