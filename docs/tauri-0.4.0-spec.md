# ChemSSH Launcher 0.4.0 开发规范与执行手册（Tauri v2 + Rust + Vue + pnpm）

> **用途说明**：本文档用于指导 AI 编程助手（如 DeepSeek）在 `tauri` 分支上开发 `chemssh-launcher` `0.4.0` 版本。
> **核心架构**：**Tauri v2 + Rust 后端（Cargo Workspace）+ 复用现有 Vue 3 前端 + pnpm 包管理**。

---

## 0. 给执行者（AI 助手）的铁律（必须严格遵守）

1. **严格按阶段执行，禁止跨阶段大改**：
   - 本文档分为 **Phase 0 ~ Phase 5**。每次只执行用户指定的单个 Phase。
   - 未经用户确认，**不得**跳过当前 Phase 的验收命令直接进入下一个 Phase。
2. **严格使用 Tauri v2（`2.x`）API，禁止混用 Tauri v1 写法**：
   - Rust 依赖必须使用 `tauri = "2"`、`tauri-build = "2"` 及官方 v2 插件（`tauri-plugin-dialog`、`tauri-plugin-opener` 等）。
   - 前端必须使用 `@tauri-apps/api/core`（`invoke`、`Channel`）和 `@tauri-apps/api/window`，禁止使用已废弃的 `@tauri-apps/api/tauri`。
3. **前端包管理统一使用 `pnpm`，禁止再使用 `npm`**：
   - 所有脚本、文档、`tauri.conf.json`、CI 命令一律使用 `pnpm`。
   - 禁止提交或重新生成 `package-lock.json`。
4. **安全与隐私红线（绝对不可违反）**：
   - 绝不将明文密码、私钥口令、Security Token 写入任何 JSON 配置文件、日志或错误信息中。
   - 绝不在编辑接口中把已保存的密码原文返回给前端（只返回 `has_password` / `has_private_key_passphrase` / `has_security_token` 布尔值）。
   - 绝不默认绑定 `0.0.0.0`；本地转发和远程反向代理监听均必须默认且优先绑定回环地址 `127.0.0.1`。
   - 绝不在文档、测试、注释、提交信息中写入真实用户名、真实主机名、真实内网路径；统一使用占位符（`user`、`example.com`、`/home/user/project`、`C:\Users\user\Desktop`）。
5. **渐进替换，不要一开始就删除现有 Go 代码**：
   - Phase 0 ~ Phase 4 期间，保留原 `cmd/`、`internal/` 作为行为对照参考。
   - 仅在 Phase 5 全部功能与测试验收通过后，再由用户决定是否归档或移除旧 Go 代码。
6. **禁止臆造接口与静默吞错**：
   - 每个对外命令、配置字段、行为边界必须与现有 Go 版本保持 100% 语义兼容（尤其是老用户的配置文件与系统凭据库）。
   - 反向代理通道或代理桥接文件失败时必须**降级并告警**，绝不能导致整个 SSH 会话启动失败。

---

## 1. 目标目录结构

在 `tauri` 分支上采用 Cargo Workspace 组织后端，确保模块边界清晰、便于独立单测：

```text
chemssh-launcher/
├── Cargo.toml                  # Rust Workspace 根配置
├── frontend/                   # 现有 Vue 3 + Vite + TypeScript + Element Plus + xterm.js（改用 pnpm）
│   ├── package.json            # 切换 pnpm，新增 @tauri-apps/api
│   ├── pnpm-lock.yaml          # 替代 package-lock.json
│   └── src/
│       ├── api.ts              # 改造为 Tauri invoke 适配层
│       ├── appEvents.ts        # 改造为 Tauri Channel / Event 流式适配层
│       ├── nativeDrag.ts       # 改造为调用 Rust Windows OLE 拖出命令
│       └── views/              # 复用现有 5 个视图，尽量少改业务逻辑
├── crates/
│   ├── core-config/            # Profile / LocalProxy / X11Config / Settings / JSON 存取
│   ├── core-secret/            # OS Keyring（兼容现有键名）+ Argon2id/AES-GCM 回退 Vault
│   ├── core-ssh/               # russh：认证、host-key、PTY、正向隧道、反向代理隧道、X11、SFTP 代理桥接
│   ├── core-transfer/          # russh-sftp：SFTP 文件操作、断点续传、跨服流式传输、压缩/解压、后台缓存队列
│   ├── core-session/           # 会话状态机、端口检测、健康检查、日志缓冲
│   └── win-native/             # #[cfg(windows)] Windows OLE 异步拖出、后台缓存交付、系统文件图标
├── src-tauri/                  # Tauri v2 桌面壳（仅做薄适配层，不堆砌核心业务逻辑）
│   ├── Cargo.toml
│   ├── tauri.conf.json         # 窗口默认 visible: false，前端就绪后 show()，彻底消除 1x1 窗口
│   ├── capabilities/           # Tauri v2 权限配置
│   └── src/
│       ├── main.rs
│       ├── lib.rs
│       ├── state.rs            # AppState（管理各 core-* 服务实例）
│       └── commands/           # 按领域拆分的 #[tauri::command] 模块
│           ├── profiles.rs
│           ├── sessions.rs
│           ├── terminal.rs
│           ├── sftp.rs
│           ├── local_fs.rs
│           ├── transfer.rs
│           ├── dragout.rs
│           └── backend.rs
├── cmd/                        # [旧 Go 参考代码，Phase 5 前保留]
└── internal/                   # [旧 Go 参考代码，Phase 5 前保留]
```

---

## 2. 必须 100% 兼容的数据契约

### 2.1 Profile JSON 格式（对照 `internal/config/profile.go` 与 `local_proxy.go`）

Rust `serde` 结构体字段名必须与现有 JSON 完全一致（使用 `#[serde(default)]` 兼容旧配置文件缺失字段）：

- `id`, `kind` (`"remote"` | `"local"`), `name`
- `ssh_host`, `ssh_port` (默认 `22`), `ssh_user`, `auth_method` (`"password"` | `"private_key"`)
- `has_password`, `private_key_path`, `has_private_key_passphrase`
- `remote_host` (默认 `"127.0.0.1"`), `remote_port` (默认 `8888`)
- `local_host` (默认 `"127.0.0.1"`), `local_port` (默认 `8888`), `local_url_path` (默认 `"/"`)
- `pre_start_commands`, `start_command`, `health_check_url`, `open_browser` (默认 `true`)
- `has_security_token`
- `local_proxy`: `{ enabled: bool, host: "127.0.0.1", port: 7890, no_proxy: "localhost,127.0.0.1,::1" }`（需实现 `normalized()` 方法）
- `x11`: 对照 `internal/config/x11.go` 完整字段

### 2.2 凭据存储契约（对照 `internal/secret/`）

- **OS Keyring**：使用 `keyring` crate，必须沿用现有命名确保老用户升级 `0.4.0` 后无需重填密码：
  - `service`: `"chemssh-launcher"`
  - `username`:
    - `<profile-id>:password`
    - `<profile-id>:key-passphrase`
    - `<profile-id>:security-token`
- **回退 Vault**：Keyring 不可用时使用 Argon2id + AES-256-GCM 加密文件，绝不持久化 Vault 主密码。

### 2.3 本地网络代理与 ChemSSH 桥接契约（对照 `AGENTS.md` 与 `internal/sshclient/proxy_*.go`）

- 反向监听只能绑定远端回环 `127.0.0.1:0`（由 SSH 服务端分配端口）。
- 注入环境变量：`http_proxy`, `https_proxy`, `all_proxy`, `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`，`no_proxy` 非空时同时注入 `no_proxy` / `NO_PROXY`。
- 远程命令中 `export` 必须放在 `set -e` 之后、`pre_start_commands` 之前。
- 桥接文件路径：通过 SFTP `realpath(".")` 获取远端 `$HOME`，写入 `$HOME/.chemssh-launcher/terminal-proxy-<effective_chemssh_port>.json`（权限 `0600`），JSON 结构：
  ```json
  {
    "version": 1,
    "proxy_url": "http://127.0.0.1:<assigned_port>",
    "no_proxy": "localhost,127.0.0.1,::1",
    "env": { "...": "..." }
  }
  ```
- 暂停转发时保留反向通道和桥接文件；完全停止或异常退出时关闭通道并删除桥接文件（文件不存在不报错）。

---

## 3. 前后端通信改造规范（Tauri v2 最佳实践）

1. **普通请求（替代原 `/api/*` REST 接口）**：
   - 使用 `#[tauri::command] pub async fn ...`，前端通过 `invoke(...)` 调用。
   - 所有 I/O 或网络操作必须 `async` 执行在 Tokio 运行时中，**严禁阻塞 Tauri 主线程 / UI 线程**。
   - **禁止**在普通 `invoke` 中做长时间阻塞等待（例如把长轮询伪装成 `invoke`），避免在 Windows WebView2 上占用并发槽位。
2. **高频流式数据（替代原 `/api/events` WebSocket 与 `/api/logs/stream` SSE）**：
   - 使用 **`tauri::ipc::Channel<T>`**（原生消息桥推流，完全不受 HTTP 6 连接限制）：
     - **SSH 终端输出**：每个终端会话打开时传入独立 `Channel<TerminalOutputEvent>`；对高频输出做微批合并（如 16ms 或 8–16KB 合并一次发送），并保留序号与流控提示语义。
     - **SFTP 传输进度**：后端维护进度状态表，**按 50ms–100ms 节流合并快照**通过 `Channel<TransferProgressSnapshot>` 推送，绝不对每 4KB 读写直接发事件。
     - **会话/启动器日志**：使用 `Channel<LogBatchEvent>` 批量推送。
3. **ChemSSH 远程 Web 页面承载**：
   - 远程 ChemSSH 本身是运行在远端 `127.0.0.1:<remote_port>` 的 Web 应用。
   - 本地 SSH 正向隧道依然转发到 `127.0.0.1:<local_port>`；前端 `ShellView` 可继续通过本地代理/iframe 或独立 Tauri 子窗口加载 `http://127.0.0.1:<local_port><local_url_path>`。
4. **启动无 1×1 窗口白屏方案**：
   - 在 `src-tauri/tauri.conf.json` 中设置主窗口 `"visible": false`（并设置合理默认宽高，如 `1200x800`）。
   - 前端 Vue 根组件 `App.vue` 完成首屏挂载后，调用 Tauri 窗口 API（如 `getCurrentWindow().maximize()` + `show()`），同时在 Rust 侧保留 2 秒兜底定时器，防止前端异常导致窗口永不显示。

---

## 4. 分阶段开发与验收清单（逐阶段下发给 DeepSeek）

> **使用方式**：每次只复制一个 Phase 的任务给 DeepSeek，要求其完成后汇报改动文件和验收输出，确认通过后再进入下一 Phase。

### Phase 0：分支初始化、pnpm 切换与 Tauri v2 骨架验证
- **前置操作**：从当前 `master` 创建并切换到分支 `tauri`：`git checkout -b tauri`。
- **任务清单**：
  1. **前端切换 `pnpm`**：
     - 删除 `frontend/package-lock.json`（如存在）。
     - 在 `frontend/package.json` 中添加 `"packageManager": "pnpm@11.7.0"`（按本机实际版本），安装 `@tauri-apps/api`（`^2`）及 `@tauri-apps/cli`（`^2`，devDependency），并在 `frontend/pnpm-workspace.yaml` 中配置 `allowBuilds: { esbuild: true }`。
     - 运行 `pnpm --dir frontend install` 生成 `frontend/pnpm-lock.yaml`。
     - 验证现有前端脚本全部通过 `pnpm --dir frontend run build` 及 `pnpm --dir frontend test`。
  2. **初始化 Cargo Workspace 与 Tauri v2 骨架**：
     - 创建根目录 `Cargo.toml`（workspace 包含 `crates/*` 与 `src-tauri`）。
     - 初始化 `src-tauri/`（版本号设为 `0.4.0`），配置 `tauri.conf.json`：
       - `beforeDevCommand`: `"pnpm --dir frontend dev"`
       - `beforeBuildCommand`: `"pnpm --dir frontend build"`
       - `devUrl`: `"http://127.0.0.1:5174"`
       - `frontendDist`: `"../frontend/dist"`
       - 主窗口默认 `"visible": false`，实现前端就绪回调 `show()` + Rust 2s 兜底显示。
     - 添加最小健康测试命令 `ping`（返回 `"0.4.0"`）与一个 `Channel` 推流示例命令。
- **验收标准**：
  - `pnpm --dir frontend run build` 通过；
  - `cargo check --workspace` 与 `cargo test --workspace` 通过；
  - （Windows 实机 / CI）`pnpm tauri dev` 能启动应用、无 1×1 闪窗、前端成功调用 `ping` 与 `Channel`。

---

### Phase 1：`core-config`、`core-secret` 与 `core-session` 基础层
- **参考旧代码**：`internal/config/`、`internal/secret/`、`internal/netcheck/`、`internal/health/`。
- **任务清单**：
  1. 实现 `core-config`：`Profile`、`LocalProxy`、`X11Config`、`UISettings`、配置目录路径定位、原子写入 JSON、导入/导出（绝不包含密钥）、`validate()` 与 `normalized()`。
  2. 实现 `core-secret`：`SecretStore` trait、`KeyringStore`（兼容现有 `chemssh-launcher` service/key）、`VaultStore`（Argon2id + AES-256-GCM）、`MemoryStore`（用于单测）。
  3. 实现本地端口占用检测、空闲端口推荐、HTTP 健康检查（`200..=499` 视为可达）。
  4. 在 `src-tauri/src/commands/profiles.rs` 和 `backend.rs` 中暴露 Profile CRUD、排序、复制、导入导出、UI 设置、版本号等 `#[tauri::command]`。
- **验收标准**：
  - `cargo test -p core-config -p core-secret -p core-session` 全部通过（含 JSON 往返兼容测试、编辑绝不回传明文密码测试、端口冲突与健康检查测试）。

---

### Phase 2：`core-ssh` 核心引擎与原生流式终端
- **参考旧代码**：`internal/sshclient/`、`internal/app/session.go`、`internal/gui/terminal.go`、`internal/gui/events_ws.go`。
- **任务清单**：
  1. 基于 `russh` 实现 SSH 连接、密码认证、私钥认证（含加密私钥 passphrase）、Host Key 校验与信任保存。
  2. 实现远程命令拼装与执行：严格保持 `set -e` → Proxy `export`（如启用）→ `pre_start_commands` → `start_command` 顺序，实时输出 stdout/stderr。
  3. 实现本地 TCP 端口转发（`127.0.0.1:<local_port> -> <remote_host>:<remote_port>`）。
  4. 实现反向代理通道（`tcpip-forward` 仅绑定 `127.0.0.1:0`）及 SFTP 代理桥接文件写入/删除（失败必须降级告警，不阻断会话）。
  5. 实现 X11 转发对接与独立交互式 PTY 终端会话（resize、cwd 追踪、输入发送结果回传、流控状态提示）。
  6. 在 `src-tauri/src/commands/sessions.rs` 与 `terminal.rs` 中暴露会话控制和基于 `tauri::ipc::Channel` 的终端推流。
- **验收标准**：
  - `cargo test -p core-ssh` 通过（含命令拼装顺序测试、Proxy 环境变量大小写及空 `no_proxy` 测试、降级容错测试）；
  - 实测能连接测试 SSH 服务器，打开原生终端流式输出流畅、resize 正常、ChemSSH 隧道可达。

---

### Phase 3：`core-transfer`（SFTP、本地文件、断点续传、跨服传输与压缩）
- **参考旧代码**：`internal/sftpclient/`、`internal/archive/`、`internal/gui/sftp.go`、`internal/gui/local.go`、`internal/gui/transfer_*.go`、`internal/gui/archive.go`。
- **任务清单**：
  1. 基于 `russh-sftp` 实现目录列表、`realpath`、新建文件/目录、重命名、删除（含回收站支持）、远端编辑打开与变更回传监听（`notify` crate 替代 `fsnotify`）。
  2. 实现本地文件浏览与基础操作命令（`local_fs.rs`）。
  3. 实现统一传输管理器：上传、下载、断点续传、跨服务器流式复制、服务端同机复制、暂停/恢复/取消控制。
  4. 实现 50–100ms 节流合并的进度快照，通过 `tauri::ipc::Channel` 推送给前端底部任务栏。
  5. 实现远端/本地压缩与解压（对齐现有支持格式）。
- **验收标准**：
  - `cargo test -p core-transfer` 通过（含断点续传、取消清理、进度节流单测）；
  - 前端 SFTP 双栏浏览、大文件上传下载、暂停/续传、取消全部可用。

---

### Phase 4：`win-native`（Windows OLE 异步拖出、后台缓存与系统图标）
- **参考旧代码**：`internal/webview/dragout_*.go`、`internal/dragtransfer/`、`internal/fileicon/`。
- **任务清单**：
  1. 在 `crates/win-native`（`#[cfg(windows)]`）中使用官方 `windows` crate 实现：
     - 获取 Tauri 主窗口 `HWND`；
     - 异步虚拟文件拖出（`IDataObject` + `IDropSource` + 后台缓存下载，拖放到资源管理器时前台不阻塞，缓存完毕后交付）；
     - 将拖出缓存任务接入 `core-transfer` 的底部传输任务栏；
     - 系统文件图标获取；
     - 关闭窗口前检查活跃会话/传输任务的拦截确认。
  2. 非 Windows 平台提供空实现 / 友好降级，保证 `cargo check --workspace` 在 Linux/macOS 下无报错。
- **验收标准**：
  - Linux 下 `cargo check --workspace` 通过；
  - Windows 下 `cargo test -p win-native` 通过；
  - Windows 实机验证：从 SFTP 列表拖出多文件/大文件夹到桌面或资源管理器，前台不卡死，底部任务栏实时显示进度，完成后文件正确落地。

---

### Phase 5：前端适配层切换、端到端回归与旧代码清理
- **参考旧代码**：`frontend/src/api.ts`、`frontend/src/appEvents.ts`、`frontend/src/nativeDrag.ts`、`frontend/src/views/*`。
- **任务清单**：
  1. 将 `frontend/src/api.ts` 中的 `fetch('/api/...')` 统一替换为 Tauri `invoke(...)`（保留清晰的错误类型转换）。
  2. 将 `frontend/src/appEvents.ts` 中的 WebSocket 客户端替换为 `tauri::ipc::Channel` 订阅管理器，保持对外方法签名尽量不变，减少 `views/` 改动量。
  3. 将 `frontend/src/nativeDrag.ts` 接入 `dragout` Tauri 命令。
  4. 配置 GitHub Actions（Windows + Linux）构建工作流：使用 `pnpm` + `cargo test` + `pnpm tauri build`。
  5. 全量人工冒烟测试（对照 `AGENTS.md` 冒烟清单），全部通过后由用户确认再清理旧 Go 代码与更新 `README.md` / `README.zh-CN.md`。
- **验收标准**：
  - `pnpm --dir frontend run build` 与全部前端单测通过；
  - `cargo test --workspace` 通过；
  - `pnpm tauri build` 成功产出 `0.4.0` Windows 安装包/单文件，冒烟测试 100% 通过。

---

## 5. 给 DeepSeek 的标准任务派发模板（每次复制使用）

```text
请严格按照《ChemSSH Launcher 0.4.0 开发规范与执行手册》执行 **Phase X**。
要求：
1. 先阅读本 Phase 涉及的现有参考文件，列出你的修改计划，不要擅自扩大修改范围。
2. 严格遵守第 0 节铁律（Tauri v2 API、pnpm、安全与隐私红线、兼容现有配置与凭据格式）。
3. 编码完成后，运行本 Phase 的全部验收命令，并贴出真实命令输出。
4. 完成后仅汇报：改动文件清单、验收结果、存在的风险或待确认项，不要自动进入下一个 Phase。
```
