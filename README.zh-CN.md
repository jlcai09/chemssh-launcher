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

[源码构建（开发者）](#development-guide)：环境准备、依赖安装、编译命令及常见错误请直接查看后面的开发指南。

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
- `CHEMSSH_LAUNCHER_VAULT_PASSWORD=...`：设置后强制改用本地加密 vault（即使系统凭证管理器可用；此时不会读取 keyring 中已保存的 secret）。

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
- 可选反向通道：让远程机器使用本机运行的网络代理，包括 ChemSSH 自己的终端标签页（见 [本机网络代理](#本机网络代理)）。
- 系统浏览器模式和可选 Windows WebView2 模式。
- 多标签 `/shell` 工作区，第一个固定标签页即启动器页面；点击 `+` 打开空白标签页，并提供启动器、终端、SFTP、后台四个入口。
- 面向内嵌 ChemSSH 的同源代理（`/<profile-name>/chemssh`，裸 `/chemssh` 亦可用），包括 WebView2 友好的文件拖拽。
- 远程 profile 和本地调试 profile。
- 服务器配置侧栏支持拖拽排序；右键可复制、删除配置，或将远程服务器直接用 SFTP 打开。
- 按 profile 记录日志，同时提供 launcher/backend 日志，支持关键字过滤、复制全部与导出。
- 系统凭证管理器保存 secret，加密 vault 兜底，OpenSSH 兼容 `known_hosts`。
- 独立 SSH 调试终端 `/terminal`，无需先安装 ChemSSH；采用类似 Xshell 的布局，左侧服务器栏可直接连接。
- 从系统文件管理器拖拽文件/文件夹到 SSH 终端，或使用底部状态栏的上传按钮，通过 SFTP 上传并显示进度环。
- 独立 SFTP 页面 `/sftp`。
- 类似 Xftp 的左右双窗格文件管理，每个窗格可打开本地或远程多标签，支持把多选的文件和文件夹直接拖到系统。
- 文件操作：列目录、打开、用文本编辑器打开、新建文件、新建目录、重命名、删除、复制路径。
- 本地/远程、远程/远程、本地/本地之间传输文件。
- 传输队列、进度、速度/ETA、文件夹分组、暂停、继续、取消、断点续传与自动重试。
- 远程文件打开到本地缓存，本地编辑后自动同步回远程。
- 支持系统文件图标，无法获取时使用 fallback 图标。
- Backend 页面可查看界面语言和版本、配置/缓存路径，导入/导出 profile、清理缓存。
- 界面语言默认跟随系统，也可以固定为中文或 English；设置会在下次启动时保留。

## Profile

Profile 保存在本机，分为远程 profile 和本地 profile。保存后的配置以卡片形式显示在启动器左侧：拖拽卡片可以调整顺序，新顺序会随 profile 存储一起持久化。右键卡片可以复制配置（包括已保存的 secret）、删除配置，或将该服务器用 SFTP 打开；用 SFTP 打开会复用已经显示 SFTP 页面的 `/shell` 标签页，把该服务器作为窗格标签页追加进去，只有当前没有打开的 SFTP 标签页时才会新建一个。本地 profile 不支持该操作。

`复制配置` 会在不暴露明文的前提下复制连接设置以及已保存的密码、私钥口令和安全令牌；副本会追加到列表末尾并自动选中以便编辑。

### 远程 Profile

远程 profile 通过 SSH 连接服务器，并可以在服务器上启动 ChemSSH。

重要字段：

- `SSH Host`、`SSH Port`、`SSH User`：SSH 目标。
- `Auth Method`：`Password` 或 `Private Key`。选择 `Password` 时，Launcher 会同时发送 RFC 4252 `password` 和 RFC 4256 `keyboard-interactive` 两种认证方式，因此仅允许 `keyboard-interactive` 的服务器（常见于 HPC 登录节点）无需额外配置即可认证。
- `Remote Host`、`Remote Port`：ChemSSH 在远程机器上的监听地址。
- `Local Host`、`Local Port`：本地 tunnel 的监听地址。
- `Local URL Path`：本地浏览器打开的路径。
- `Local Network Proxy`：把本机运行的网络代理反向转发到远端回环，供远端进程使用（见 [本机网络代理](#本机网络代理)）。
- `Pre-start Commands`：启动前执行的可选命令。
- `Start Command`：启动 ChemSSH 的命令。
- `Health Check URL`：可选覆盖值；留空时检查本地浏览器 URL。
- `Open ChemSSH tab after start`：启动成功后是否自动打开 ChemSSH。
- `Security Token`：ChemSSH 安全令牌，用于启用了 token 认证的 ChemSSH 实例。配置后，Launcher 会在代理的 API 请求、终端 WebSocket 连接和 health/identity 检查中自动注入令牌，无需每次手动输入。

已保存的 secret 只显示为 `*******`。每个 secret 都有 `保持` / `替换` / `清除` 三个动作：留空并选择 `保持` 时不变，输入新值会自动切到 `替换`，只有选择 `清除` 并保存才会删除已保存的 secret（留空 + `替换` 会写入一个空值）。

### 本地 Profile

本地 profile 用于本机 ChemSSH 调试。它不使用 SSH，不创建 SFTP session，也不创建 SSH tunnel。

本地 profile 中，`Local Host`、`Local Port`、`Local URL Path`、`Pre-start Commands` 和 `Start Command` 描述本机 ChemSSH 进程。如果 health URL 已经可访问，Launcher 会直接复用；否则会用 Windows PowerShell 或 Unix-like 系统的 `sh -c` 执行配置的本地启动命令。

## 默认启动命令

默认远程 `Start Command` 假设服务器有 `bash`、项目 `.venv` 和可用的 `chemssh` 命令：

```bash
source .venv/bin/activate
unset PS1
export CONDA_CHANGEPS1=false
export VIRTUAL_ENV_DISABLE_PROMPT=1
chemssh --config config.yaml
```

Launcher 会检查启动命令的**最后一行**：如果该行还没有 `--host` 或 `--port`，就把参数追加到该行：

```bash
--host <Remote Host> --port <Remote Port>
```

## SSH 调试终端

终端页面为远程 profile 提供不依赖 ChemSSH 的基础交互式 SSH shell：

```text
/terminal
```

可以在安装 ChemSSH 之前用它查看服务器、检查路径、加载模块或执行简单命令。Launcher 会建立独立的 SSH 连接并申请远程 PTY（`xterm-256color`）；认证和 host key 校验沿用 profile 与 `known_hosts` 策略。终端尺寸变化会同步到远程，断开连接、删除 profile 或退出 Launcher 时会关闭会话。本地 profile 不提供 SSH 终端。

终端页面采用类似 Xshell 的布局：左侧服务器栏列出已保存的远程 profile，单击选中，双击连接；栏目标题右侧的刷新按钮可重新拉取 profile 列表，因此在启动器页面新增、改名或删除的配置无需刷新整个终端页即可生效。双击总是新建标签页，即使该服务器已有连接的会话也如此；只有当前标签页尚未连接过时才会直接在其中连接，避免占位标签页留在新会话旁边。窗口较窄时服务器栏会收成小图标列。清屏、上传按钮以及 `ssh://user@host:port` 地址和连接状态位于终端底部的状态栏。

左侧栏分为上下两个可折叠区块：服务器列表，以及当前终端标签页的 SFTP 浏览器，点击各自的标题即可折叠或展开。拖动侧栏与终端之间的分隔条可以调整侧栏宽度；宽度、服务器列表的折叠状态和 SFTP 同步偏好都会记住。每个新建的终端标签页默认收起 SFTP 浏览器，不会自行额外建立 SFTP 通道：展开时才为该标签页建立（或复用）SFTP 会话，收起时若没有正在进行的上传就释放该通道。浏览器标题栏的同步按钮会在两个方向建立联动：在终端里切换目录会刷新浏览器，而在浏览器里进入目录（双击、上一级按钮或手动输入路径）会把对应的 `cd` 发送给终端。双击文件的行为与 SFTP 页面的“打开”一致：Launcher 会把文件缓存到本地并用系统关联程序打开，每次保存都会自动回传到远程路径；即使收起浏览器，这条通道也会保留到终端标签页关闭为止；超过 100 MB 的文件与 SFTP 页面一样不支持这种打开方式。路径左侧的箭头按访问历史返回上一个目录，列表顶部的 `..` 一行双击进入上一级。把条目拖到资源管理器或桌面会通过系统拖拽目标把该文件保存到本地（Chromium 只接受一个 `DownloadURL`，因此一次只带一个文件）；把系统里的文件或文件夹拖到面板上，会直接上传到当前显示的目录。窗口较窄或进入纵向布局时，侧栏只保留服务器列。

每个标签页各自维护独立的 SSH shell 与传输会话。点击标签栏中的 `+` 新建标签页，右键标签页可以复制、打开 SFTP、关闭或全部关闭，双击标签页也会直接复制。复制标签页会用同一个 profile 建立新连接，并进入原标签页所在的目录，方便在同一位置并行操作。**打开 SFTP**（或 `Ctrl+Alt+F`）会为该 profile 打开 SFTP 页面并直接进入当前 shell 所在目录，而不是服务器栏和启动器入口使用的远程主目录；按快捷键时总是新建标签页，即使同一目录已经打开着也照常新建。向左或向右拖拽标签页可以调整它们的顺序。

可以从系统文件管理器将文件或文件夹拖到终端上，通过 SFTP 上传，也可以点击底部状态栏左侧的上传按钮手动选择文件；上传时按钮外圈会显示进度环和百分比数字。Launcher 会从提示符标记跟踪 shell 当前工作目录（无法获取时回退到远程主目录），因此上传会跟随当前所在目录；缺少的相对子目录会自动创建，遇到同名项目会先询问。终端使用的 SFTP 会话与当前连接绑定，断开终端时释放。

Launcher 还会在远程 shell 的 `PATH` 中安装临时的 `sz`/`rz` shim，与 ChemSSH 本体的终端集成逻辑一致：执行 `rz` 会打开本地文件选择器并上传到当前目录，执行 `sz <文件>` 会通过 SFTP 下载这些文件。shim 发出传输标记后立即退出，因此不会让终端停在等待状态。如果无法安装 shell 集成，则会拦截原生 ZMODEM 特征并发送 Ctrl-C，保证终端仍可使用。临时的 `vi`/`vim` wrapper 会绕开 shim 目录找到真实的编辑器，并关闭 Vim 的终端能力查询，与 ChemSSH 本体一致，避免编辑器在浏览器终端中卡住。拖拽和 SFTP 页面仍是推荐的传输方式。

### 转发 X11 到本机 X Server

远程 profile 可为新开的 Launcher SSH 终端启用 **X11 转发**。Go SSH 后端在启动 shell 前申请转发，校验传入的 X11 认证，并把图形通信送到本机 X Server。远端 SSH 服务负责设置 `DISPLAY`，无需在预启动命令中硬编码。图形窗口由 VcXsrv 或其他本机 X Server 显示，不会嵌在终端画布内。

请先自行启动 VcXsrv 或其他本机 X Server，再打开 Launcher SSH 终端。Launcher 只向已运行的 X Server 转发，不启动或管理 X Server。勾选 **X11 转发**，并将本机 TCP Display 设为服务器实际使用的显示地址（默认 `127.0.0.1:0`）。X Server 需在该地址接受来自 Launcher 的 TCP 连接。

**Xauthority 文件**为可选项。留空时尝试 `XAUTHORITY` 或默认主目录认证文件；若服务器使用 `MIT-MAGIC-COOKIE-1`，也可指定匹配的认证文件。如果 X Server 的现有访问策略允许本机无 cookie 连接，同样可以转发。无需输入 cookie，profile 只保存认证文件路径。请按本机访问需要维持现有服务器访问控制和防火墙限制。这是受信任的 X11 转发，仅对信任的远程机器和图形程序启用。

远端需要安装 `xauth`，SSH 服务需允许 `X11Forwarding`。保存 profile 后断开并新开 Launcher 终端，已存在的 shell 不会自动获得转发。终端会显示 X11 是否就绪或启动失败原因。可以执行 `echo "$DISPLAY"`，然后运行 `ase gui example.traj` 或 `xclock`。如果 SSH 服务接受请求但登录脚本清除了 `DISPLAY`，应修改脚本，不要手工猜显示号。若程序运行在另一台计算节点或容器内，还需为该环境配置显示与认证的路由。

X11 启动失败时文本终端仍可使用；关闭终端会关闭其 X11 通道。本设置作用于 Launcher 自身的 SSH 终端，ChemSSH 另外创建的终端进程不会自动继承此转发。

## SFTP 与本地文件

SFTP 页面与 ChemSSH 启动流程解耦，可以从 Launcher 打开，也可以访问：

```text
/sftp
```

在远程标签页先用 Ctrl/Shift 或框选选中文件与文件夹，再在选中的行上按住鼠标左键约半秒后拖动。Windows WebView2 桌面版通过 Windows 原生 OLE 异步拖放，将单个文件、多个文件和文件夹交给支持异步提取的系统目标。目标接收后，松开鼠标即可继续操作 Launcher；后台作业通过 SFTP 递归缓存，再由系统复制到它接收拖放的目标位置。目标位置由系统拖放协议决定。

拖拽任务统一显示在 SFTP 页面下方的传输任务栏，不再弹出右上角提示；列表显示实际缓存字节、进度、速度和耗时。文件夹总大小随遍历发现文件逐步增加。右键选中任务可暂停、继续或取消后台缓存，也可与普通传输一起多选控制；暂停在读取检查点生效，已在途的一块数据可能仍会完成。回到 SFTP 页面按 Esc 可取消最新的缓存任务、中断 SFTP 并清理未完成副本。缓存完成后任务显示“系统交付中”，由系统负责复制，此时 Launcher 不再提供暂停或取消；只有系统报告交付结束才标为完成。目标路径显示为“系统拖放目标位置”，不会把临时缓存目录冒充为最终位置。已结束任务通过任务栏现有清理按钮移除。关闭 Launcher 会中断尚未完成的后台缓存。目标不支持异步提取时会报告失败；不会同步等待大文件缓存。

拖到另一侧窗格或文件夹则在 Launcher 内部传输。普通浏览器模式的单文件拖出沿用浏览器下载，多文件/文件夹拖出到系统需要 Windows WebView2 桌面版。快速按住行移动用于范围选择，长按后移动用于拖动当前选区。

页面左右两侧各自维护多个标签页。每个标签页可以浏览本地文件系统，或通过已保存远程 profile 建立 SSH/SFTP 连接。远程 SFTP 标签页需要远程 profile 和 host key 校验，但不会启动 ChemSSH、执行 start command、创建 tunnel，也不依赖 ChemSSH health check。右键标签页可以复制、关闭或全部关闭，双击标签页也会直接复制；复制远程标签页会重新连接同一个 profile 并打开相同目录。向左或向右拖拽标签页可以调整顺序，把标签页拖到另一侧窗口的标签栏或文件区域即可移动到另一侧。全部关闭后可通过 `+` 重新打开。`+` 菜单是一个扁平列表，包含本地文件系统与所有已保存的远程服务器；点击任意一项即在当前侧打开对应标签页，本地从用户主目录开始、远程从远程主目录开始，不需要输入路径或二次确认。启动器中的“用 SFTP 打开”会复用已打开的 `/sftp` shell 标签页，在其右侧窗格追加该服务器的远程标签页并自动连接远程主目录；若该服务器已有标签页则直接切到前台，没有打开的 SFTP 标签页时才访问 `/sftp?profile=<id>&name=<name>` 新建一个。终端标签页右键的“打开 SFTP”（快捷键 `Ctrl+Alt+F`）会额外带上 `&path=<目录>`（或通过消息传递同一个 `path`），让标签页直接进入该 shell 所在目录；右键菜单在该服务器已有标签页正好停在这个目录时直接切到前台，否则在右侧窗格追加一个新标签页，而 `Ctrl+Alt+F` 总是新建右侧窗格标签页，即使同一目录已经打开也不会切过去，原标签页位置保持不变。页面尚未就绪时请求会排队，就绪后再发送。

选中文件或目录后按 `Delete` 可以直接删除：本地标签页会把选中项移入系统回收站且不弹确认框，远程标签页的行为与右键删除一致，会先弹出确认。

路径栏始终显示绝对路径：输入 `.`、`user/data` 这类相对路径时，会先通过服务器解析为绝对路径再列目录；右键复制文件路径同样得到绝对路径。

列目录时会解析符号链接：指向目录的链接显示为带链接角标的文件夹图标，可双击进入，鼠标悬停会在提示中显示链接目标。删除链接只会删除链接本身，不会影响其指向的目标。

支持的传输方向：

- 本地到远程。
- 远程到本地。
- 远程到远程，通过 Launcher 中继。
- 本地到本地，通过 Launcher 复制。
- 从系统文件管理器拖拽文件/文件夹上传到远程标签页。

单个文件的复制（本地↔远程、远程↔远程、本地↔本地）按字节断点续传：Launcher 只记录已经确认写入的偏移，传输中断时会自动重连 SSH 并按指数退避重试（最多 5 次，单次等待不超过 15 秒），每次重试都从上次确认的偏移继续，而不是从头再拷。重试期间传输行显示“重试 n/5”，速度和 ETA 暂置为等待数据；权限不足、目标不存在、磁盘写满、源文件被改动等错误不会重试，重试用尽后该行变为失败，可以在传输面板右键（或多选后右键）选择“重试”再次从断点续传。传输过程中源文件大小或修改时间发生变化时，Launcher 会停止并报错，避免拼出损坏的文件；手动重试时若源已改变，则自动从头重传。取消过的任务再次重试会从头开始。浏览器“选择文件”上传和浏览器下载这两条路径不经过该队列（请求体和响应流无法重放），因此不支持断点续传与自动重试，中断后需要重新发起。

右键菜单的 `Open` 作用于整个选中范围：多选后打开会逐个打开所有选中的文件，而不是只打开右键点击的那一个，菜单项会显示待打开的数量。

通过 `Open` 或 `Open in text editor` 打开的远程文件会先下载到本地缓存目录，再使用系统文件关联打开；`Open in text editor` 在 Windows 使用记事本、macOS 使用 TextEdit、Linux 使用系统默认应用（xdg-open）。只要 SFTP session 仍保持连接，Launcher 会监控缓存文件变化并同步上传回原远程路径。缓存是临时的，Launcher 退出时会尽量清理。

目录传输通过前端展开为文件任务实现。部分后端路径仍只支持单文件复制，因此这类传输会直接报"不支持目录传输"的错误，而不是自动展开。

## 本机网络代理

ChemSSH 在远端只监听 `127.0.0.1`，因此只存在于本机的代理在远端无法直接访问。在远程 profile 中勾选 `Local Network Proxy`，Launcher 会用反向端口转发沿已建立的 SSH 连接把它送过去：

```text
远端 127.0.0.1:<服务器分配端口>  ->  ssh  ->  本机 <Proxy Host>:<Proxy Port>
```

Launcher 向 SSH 服务器申请一个只绑定服务器回环的监听端口（端口传 `0`，由服务器分配），两端都不会绑定 `0.0.0.0`。得到的 `http://127.0.0.1:<服务器分配端口>` 就是远端程序使用的代理地址。

字段：

- `Local Network Proxy`：是否启用反向通道。仅远程 profile 可用；本地 profile 没有可承载它的 SSH 连接。
- `Proxy Host`、`Proxy Port`：本机代理地址。默认 `127.0.0.1` 和 `7890`。
- `No Proxy`：`no_proxy` 与 `NO_PROXY` 的值。默认 `localhost,127.0.0.1,::1`；清空后不再输出这两个变量。

通道建立后，以下变量指向反向监听端口：`http_proxy`、`https_proxy`、`all_proxy`、`HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`，以及 `no_proxy` / `NO_PROXY`。远端已有的同名变量会被覆盖，而不是追加。

谁继承这些变量：

- ChemSSH 端口探测命令和启动命令：`export` 写在 `set -e` 之后、你的 pre-start 命令之前，因此 `cd`、`source`、`conda activate`、`module load` 都会保留它们。
- Launcher 的 SSH 调试终端：每个 shell 单独建立一条反向监听。`bash` 走 `--rcfile`，`export` 追加在 `/etc/profile` 和 `~/.bash_profile` 之后执行，因此优先级高于登录 profile。非 bash 登录 shell 通过启动前的 `env` 获得变量，其登录 profile 仍可能覆盖。
- ChemSSH 的终端标签页（包括复用已有 ChemSSH 后再新开的标签页）：通过下面的桥接文件。

### ChemSSH 终端桥接文件

每个 session 会通过 SFTP 写一个文件，权限 `0600`：

```text
$HOME/.chemssh-launcher/terminal-proxy-<chemssh 端口>.json
```

内容为 v1 JSON：

```json
{
  "version": 1,
  "proxy_url": "http://127.0.0.1:<服务器分配端口>",
  "no_proxy": "localhost,127.0.0.1,::1",
  "env": {
    "http_proxy": "http://127.0.0.1:<服务器分配端口>",
    "https_proxy": "http://127.0.0.1:<服务器分配端口>",
    "all_proxy": "http://127.0.0.1:<服务器分配端口>",
    "HTTP_PROXY": "http://127.0.0.1:<服务器分配端口>",
    "HTTPS_PROXY": "http://127.0.0.1:<服务器分配端口>",
    "ALL_PROXY": "http://127.0.0.1:<服务器分配端口>",
    "no_proxy": "localhost,127.0.0.1,::1",
    "NO_PROXY": "localhost,127.0.0.1,::1"
  }
}
```

文件名用的是 ChemSSH 实际监听的端口，可能来自 `--port` 而不是配置文件。ChemSSH 每次创建终端 session 时读取并校验：版本必须为 `1`，`proxy_url` 必须是 http 且主机为回环地址、端口在 `1-65535`，端点必须能建立 TCP 连接，env 键只允许上面这些变量，值不得含 NUL 字符。任何一项不通过都按"没有代理"处理——终端照常启动，不报错。

生命周期：

- 暂停转发（远端 ChemSSH 仍在运行）时保留反向监听和桥接文件。
- 停止 session、停止 Launcher 资源、远端进程异常退出时，关闭监听、删除桥接文件（`.chemssh-launcher` 目录本身保留）、关闭 SSH 连接。
- 桥接文件写失败只记 warning，session 仍会变为就绪：远程命令和 Launcher 终端拿到代理，但 ChemSSH 自己的终端不会同步。
- 反向通道完全建不起来时，session 仍然正常启动（不做反向转发），警告会出现在 profile 日志、会话卡片和终端页面。

## ChemSSH Bridge

Launcher 为 ChemSSH 暴露 bridge 接口：

```text
/api/chemssh-bridge/*
```

这些接口让通过代理打开的 ChemSSH 页面可以发现 Launcher 能力，请求系统文件图标，用本地应用打开 workspace 文件，用文本编辑器打开文件，并轮询本地编辑后的同步事件。

Bridge 能力有条件：

- ChemSSH 必须通过当前 Launcher 的代理打开（自动打开的是 `/<profile-name>/chemssh`，裸 `/chemssh` 也可用）。
- 当前 session 必须仍在 forwarding。
- Launcher 必须能读取 ChemSSH identity，并且 identity 中要包含 workspace root。
- 远程 bridge open 会为当前远程 profile 建立专用 SFTP session。
- 本地 profile 只能打开本地 ChemSSH workspace 内的文件，不提供 SFTP bridge open。

Bridge 会限制在当前 ChemSSH workspace 内；workspace 外路径会被拒绝。

## WebView2 与浏览器模式

浏览器模式会用系统浏览器打开 Launcher 本地 URL。

WebView2 模式会在内嵌窗口中打开 `/shell`。普通 WebView2 模式会禁用 DevTools 快捷键并拦截 `Ctrl+Shift+C`；只有调试时才应使用 `--devtools`。如果仍有传输任务运行，WebView2 关闭拦截可以提示用户。

`/shell` 是标签页工作区，而不是另一个应用页面：第一个固定标签页加载 `/` 启动器，`/shell` 不会在标签页内再次打开。点击 `+` 会打开空白标签页，并提供启动器、终端、SFTP、后台四个入口，也可以在地址栏输入应用路径或外部地址。界面语言在 Backend → 界面中设置。

WebView2 模式下，ChemSSH 通过 Launcher 同源代理打开：

```text
http://127.0.0.1:<launcher-port>/<profile-name>/chemssh
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

如果系统凭证管理器不可用，可以设置 `CHEMSSH_LAUNCHER_VAULT_PASSWORD` 改用本地加密 vault（只要设置了该变量就会强制使用 vault，不存在自动回退；keyring 中已保存的 secret 不会迁移过来）：

- Windows: `%AppData%\ChemSSHLauncher\vault.json`
- macOS: `~/Library/Application Support/ChemSSHLauncher/vault.json`
- Linux: `~/.config/chemssh-launcher/vault.json`

vault 密码不会被程序保存。

SSH host key 保存为 OpenSSH 兼容格式：

- Windows: `%AppData%\ChemSSHLauncher\known_hosts`
- macOS: `~/Library/Application Support/ChemSSHLauncher/known_hosts`
- Linux: `~/.config/chemssh-launcher/known_hosts`

配置目录中还保存 `client_identity.json`：Launcher 的稳定 client id，代理 ChemSSH 时通过 `X-ChemSSH-Client-Id` 头发送（终端 WebSocket 走 `client_id` 查询参数），让 ChemSSH 按客户端隔离会话缓存。`settings.json` 保存界面偏好，例如界面语言（`system`、`zh` 或 `en`）。

WebView2 数据目录和临时 SFTP 打开缓存目录可以在 Backend 页面查看。Windows 上缓存清理可能被文件锁阻塞；Launcher 会记录 pending marker，并在下次启动时继续尝试。

## 安全说明

ChemSSH Launcher 依赖 SSH 完成认证、加密、host key 校验和本地端口转发。Launcher 自身负责 profile 存储、secret 查询、远程/本地进程启动、SFTP session 和本地 HTTP 代理。

首次连接服务器时，GUI/CLI 会显示 host key 类型和 SHA256 指纹。请只在你已经通过可信来源确认后再信任，例如服务器管理员或一台已经信任该服务器的 SSH 客户端。如果已保存的 host key 后续变化，GUI 会阻止连接并同时显示旧指纹和新指纹；只有在你核对新指纹并明确确认后，才会替换 `known_hosts` 中的旧密钥。CLI 仍会拒绝指纹变化的连接。

如果把 `Local Host` 绑定到非 loopback 地址，例如 `0.0.0.0`，可能会把 tunnel 或本地 ChemSSH 目标暴露给其他机器。Launcher 会在 tunnel 表单里给出提示，在会话启动时写入 profile 日志，CLI 保存配置时也会打印警告；但这个设置仍由使用者负责。

反向代理通道与 `Local Host` 无关：监听建在远端回环上、端口由服务器分配，不会暴露给其他机器，也不会静默绑定 `0.0.0.0`。注入的环境里只有不带凭据的 `http://127.0.0.1:<port>`，Launcher 不保存任何代理密码。建议保留 `No Proxy`，让回环和内部地址继续直连。

Launcher 的本地 GUI API 对写操作（非 GET 请求）有额外保护：每次启动生成随机 token 写入 `chemssh_launcher_token` cookie，写请求必须同时携带匹配的 `X-ChemSSH-Launcher-Token` 头，并校验 Origin/Sec-Fetch-Site。Launcher 自带前端会自行带上该头；通过代理打开的 ChemSSH 页面由 Launcher 注入的脚本从 cookie 读取并补头，因此 bridge 写接口（例如本地打开文件）无需额外配置。

---

<details>
<summary>架构概览（开发者）</summary>

项目由 Go 后端、嵌入式 Vue 前端、SSH/SFTP 客户端和可选 WebView2 宿主组成：

```text
chemssh-launcher executable
  Go GUI server on 127.0.0.1:<random>
    Vue app: /, /shell, /terminal, /sftp, /launcher-logs
    Profile/session APIs: /api/profiles, /api/profiles/reorder, /api/profiles/duplicate, /api/session/*, /api/session/shell/*
    UI settings API: /api/ui-settings
    SFTP/local file APIs: /api/sftp/*, /api/local/*
    ChemSSH proxy: /<profile-name>/chemssh (bare /chemssh also accepted)
    ChemSSH bridge: /api/chemssh-bridge/*
  SSH client
    remote command startup
    interactive PTY shell
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
frontend/src/views/       Launcher、SFTP、Shell、Terminal、Backend 页面
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
idea/                     本地开发笔记（已在 .gitignore 中，不随仓库分发）
```

</details>

<a id="development-guide"></a>

## 开发指南（开发者）

### 构建前准备

- 安装 Go 1.23 或更新版本，以及 Node.js（建议 22 或更新版本）和随附的 npm，并将它们加入 `PATH`。安装后重新打开终端。
- 获取完整仓库，在包含 `go.mod`、`frontend/` 和 `tools/` 的仓库根目录执行下列命令。
- 首次安装依赖需要能访问 Go 模块代理和 npm registry。构建需要 Node.js/npm，但生成的可执行文件运行时不需要它们。
- WebView2 是 Windows 专用版本，运行它需要 Microsoft Edge WebView2 Runtime；普通浏览器版不需要该 Runtime。

### Windows / PowerShell：首次完整构建

以下命令均从**仓库根目录**执行。使用 `npm.cmd` 可以避免 PowerShell 因执行策略拦截 `npm.ps1`。

```powershell
# 1. 确认工具可用
go version
node --version
npm.cmd --version

# 2. 安装锁定版本的前端依赖（必须包含开发依赖）
npm.cmd --prefix frontend ci --include=dev

# 3. 下载 Go 依赖
go mod download

# 4. 可选：预装 Windows 图标和版本资源生成工具
go install github.com/tc-hib/go-winres@v0.3.3

# 5. 构建 WebView2 无控制台版本
go run ./tools/build --webview2 --windowsgui

# 6. 启动生成的程序
.\chemssh-launcher-webview2.exe
```

每一步成功后再执行下一步，尤其是前端依赖安装失败时，不要继续构建。`npm ci` 按 `frontend/package-lock.json` 安装依赖，并重建 `frontend/node_modules/`；首次获取源码、锁文件更新或依赖缺失时都应执行。`vue-tsc`、TypeScript 和 Vite 都是构建所需的开发依赖，不能使用 `--omit=dev`。`--include=dev` 也可避免 `NODE_ENV=production` 等配置导致这些依赖被省略。无需全局安装 `vue-tsc`。

构建工具**不会自动安装前端依赖**。它会读取 `internal/version/VERSION`、同步 `winres/winres.json`、执行前端类型检查和构建、在 Windows 上按需生成资源，最后编译 Go 程序。前端产物位于 `internal/gui/static/vue/`，会嵌入可执行文件。

`go-winres` 是可选预装工具，不能代替 npm 依赖安装。如果它不在 `PATH` 中，构建工具会自动回退到 `go run github.com/tc-hib/go-winres@v0.3.3`，需要时会联网下载。需要预先准备资源工具时，可检查 `go env GOBIN`；未设置时检查 `go env GOPATH` 下的 `bin` 目录，并将实际安装目录加入 `PATH`。这不代表首次构建可以完全离线，其他 Go/npm 依赖也必须事先准备好。

### 选择构建版本与输出位置

安装依赖后，在仓库根目录选择一条构建命令即可：

```powershell
# 浏览器版，带控制台（便于调试）
go run ./tools/build

# 浏览器版，无控制台
go run ./tools/build --windowsgui

# WebView2 版，带控制台（便于调试启动问题）
go run ./tools/build --webview2

# WebView2 版，无控制台（推荐日常使用）
go run ./tools/build --webview2 --windowsgui
```

Windows 下默认产物在仓库根目录：普通版为 `chemssh-launcher.exe`，WebView2 版为 `chemssh-launcher-webview2.exe`。构建不会自动启动程序。可以用 `-o` 指定输出路径：

```powershell
go run ./tools/build --webview2 --windowsgui -o chemssh-launcher-custom.exe
```

构建工具默认启用优化（`-trimpath -ldflags="-s -w"`）；调试时可加 `--no-optimize`，并省略 `--windowsgui` 以保留控制台日志。修改前端源码后，重新执行构建命令即可；只在依赖变化或缺失时重新安装依赖。

### macOS / Linux：浏览器版

在仓库根目录执行；不要使用 Windows 专用的 `--webview2` 和 `--windowsgui`：

```bash
npm --prefix frontend ci --include=dev
go mod download
go run ./tools/build
./chemssh-launcher
```

### 常见构建错误

- **`'vue-tsc' 不是内部或外部命令` / `vue-tsc: not found`**：通常是前端依赖尚未安装，或安装时跳过了开发依赖。`go-winres` 只负责 Windows 资源，无法解决此错误。在仓库根目录执行以下命令，并确认第一条成功后再构建：

  ```powershell
  npm.cmd --prefix frontend ci --include=dev
  go run ./tools/build --webview2 --windowsgui
  ```

- **找不到 `go`、`node` 或 `npm.cmd`**：检查安装和 `PATH`，重新打开终端，再运行版本检查命令。若 PowerShell 提示 `npm.ps1` 无法加载，使用本文的 `npm.cmd`，无需修改系统执行策略。
- **`npm ci` 提示锁文件与 `package.json` 不一致**：先确认使用同一份源码中的两个文件。如果是主动修改了依赖，运行 `npm.cmd --prefix frontend install --include=dev` 更新锁文件并一并提交；正常源码构建应保留锁文件并使用 `npm ci`。
- **依赖下载超时或失败**：先解决 Go 模块代理、npm registry 或本机网络访问问题，重新完成安装后再构建；不要删除锁文件或全局安装 `vue-tsc` 来绕过失败。

### 开发与检查

以下命令也从仓库根目录执行：

```powershell
# 仅检查类型并构建前端（macOS/Linux 将 npm.cmd 换成 npm）
npm.cmd --prefix frontend run build

# Go 测试
go test ./...

# Windows WebView2 相关测试
go test -tags webview2 ./internal/webview

# 启动 Vite 开发服务
npm.cmd --prefix frontend run dev
```

Vite 开发服务不会自动更新 Go 程序内嵌的资源。测试打包后的程序前，请重新执行 `go run ./tools/build`（或对应的 WebView2 构建命令）。

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

`go run ./tools/build` 会把版本同步到 Windows 资源信息中。版本号会显示在 Backend 页面，也可以用命令查看：

```bash
chemssh-launcher --version
```

发布前请确认 `internal/version/VERSION` 已更新，并与 git tag、提交信息保持一致（构建工具只会同步 `winres/winres.json`，不会自动修改 VERSION）。

</details>

<details>
<summary>已知限制和注意事项</summary>

- 运行时是单文件程序，但源码构建需要 Node/npm，因为当前前端是 Vue/Vite 应用。
- SFTP 只适用于远程 profile。本地 profile 用于本地 ChemSSH 调试，不提供 SSH/SFTP。
- Bridge 文件打开依赖当前 active proxied ChemSSH session，并且 ChemSSH identity 中需要有 `workspace_root`。
- Launcher 只能停止自己启动的 ChemSSH，或能通过 ChemSSH identity 识别 PID 的服务。如果 PID 读取失败，停止服务时可能留下远程 ChemSSH 进程。
- 远程和本地递归删除由后端支持，删除确认需要仔细阅读。
- 临时 SFTP open cache 清理可能被 Windows 文件锁阻塞；这属于环境清理问题，不一定表示测试或代码失败。
- Launcher 依赖页面心跳判断前端是否还在运行：Launcher 页面关闭后（约 75 秒无心跳 + 10 秒宽限），会自动停止会话、关闭 tunnel/SFTP/bridge 并退出。
- `go.sum` 应提交到仓库。它用于锁定 Go 依赖校验和，保证构建可复现，并降低依赖被篡改的风险。

</details>
