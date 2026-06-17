# rsb 更新日志

本文件记录 rsb 每个版本的变更。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.0.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

---

## [0.5.0] - 2026-06-17

第二批实战痛点修复：远端 agent 的可见性和强一致性。

### 新增
- **`ensure --force`**：强制重传 + 原子 mv + sha256 校验。默认模式幂等（hash 匹配则跳过上传）。
- **`rsb agent-version <host>`**：查询远端 agent 的版本/协议/构建时间，与本地 `rsb version` 对比。
- **`doctor --container=NAME`**：执行真实 `docker exec <NAME> true` smoke test，不再只读本地预期模式。
- doctor 新增远端 agent 版本输出和 sha256 本地/远端对比（catches stale installs）。
- rsb-agent 新增 `--version` 输出（version + protocol + build time，经 ldflags 注入）。
- SKILL.md 新增"容器执行故障兜底"段落。

### 修复
- **P8** ensure scp 部分失败后远端残留旧版、doctor 误报 ok：改为上传临时文件 + 原子 mv + 上传后强制 sha256 校验。
- **P9** doctor 容器模式假阳性：新增真实容器 smoke test。
- **P10** 远端 agent 版本不可见：新增 agent-version 命令 + doctor 远端版本/hash 对比。
- **P11** `--container` 走旧 agent nsenter 报 permission denied 误导权限问题：client 检测到时提示 stale agent + `ensure --force`。
- **P12** ensure 缺强制覆盖：新增 `--force`。
- **P13** `RSB_CONTAINER_MODE` 无生效反馈：doctor 显示本地期望模式并提示用 `--container` 做真实测试。

### 变更
- build.sh 按 binary 注入不同 ldflags：rsb→main.version，rsb-agent→main.agentVersion + main.buildTime。
- ensure 上传改为临时文件 + 原子 mv（POSIX rename），杜绝半安装状态。

---

## [0.4.0] - 2026-06-17

### 新增
- **help 体系**：`rsb -h` / `--help` / `help` 显示顶层帮助；每个子命令支持 `rsb <command> --help` 显示详细用法（USAGE / OPTIONS / EXAMPLES）。
- `rsb -v` / `--version` 作为版本号的别名。
- 顶层 usage 新增 QUICK START 段落，引导新用户四步上手。

### 变更
- help 输出走 stdout（主动请求），误用走 stderr 并 exit 2，符合 Unix 惯例。
- 未知命令现在给出 `unknown command "x"` 提示而非静默打印 usage。
- 顶层 usage 文本重写：命令列表更清晰，分 COMMANDS / GLOBAL OPTIONS / QUICK START。

### 修复
- **P1** `ensure` 不再依赖当前工作目录查找 agent，改用 home 发现机制（`RSB_HOME` > 可执行文件路径反推 > cwd）。
- **P2** daemon 二进制查找同样改用 home 发现，从任意目录调用都能自动找到并拉起 daemon。
- **P3** SSH 认证失败不再只报 `read hello: EOF`：daemon 在握手期捕获 agent stderr，按 auth denied / connection refused / host unresolvable 分类给出具体修复提示，通过 Error 帧回传给 client。
- **P4** `--container` 默认走 `docker exec`（普通用户友好），不再默认走需要 root 的 nsenter。nsenter 改为通过 `RSB_CONTAINER_MODE=nsenter` 显式启用，失败自动 fallback 到 docker exec。
- **P5** 修复 `--stdin` 套 `docker exec -i` 无输出的问题。根因是 `pumpStdin` 的 select 竞态导致 eofCh 抢占并丢弃缓冲的 stdin chunk；改为 eofCh 触发后先排空 stdinCh 再关闭管道。
- **P7** docker.sock 权限失败时返回 `SocketPermissionError`，提示具体的 `sudo usermod -aG docker <user>` 修复步骤。

### 新增命令
- `rsb doctor [host]`：自检 install home / 本平台二进制 / daemon / SSH 连通性 / 远端 agent / docker 访问，任一 FAIL 非零退出。
- `rsb install-local`：在 `<home>/bin/` 创建指向当前平台二进制的 symlink，方便加入 PATH。

### 架构
- 新增 `internal/paths` 的 home 发现机制（`Home()` / `LocalPlatformDir()` / `AgentForPlatform()`），作为所有二进制相互查找的统一基础。

---

## [0.3.0] - 2026-06-17

### 新增
- **Docker 容器执行**：`--container NAME` 直接在容器内执行命令，argv 原样到达容器内进程。
- **交互式 repl**：`rsb repl <host> [--session NAME]` 单连接多命令，cwd 跨命令持久，支持 `:session` / `:container` / `:quit` 元命令。
- **跨平台预编译**：`scripts/build.sh` 交叉编译三平台（linux-amd64 / linux-arm64 / darwin-arm64）× 三程序（rsb / rsb-agent / rsb-daemon），输出到 `skill/bin/`，附带 SHA256SUMS。
- **平台感知的 ensure**：`rsb ensure <host>` 探测远端 OS/arch，scp 匹配的二进制（不再误传本机平台 agent 给异构远端）。
- **SKILL.md 通用 skill 标准**：name / description frontmatter，agent 据此判断何时触发，附 examples.md 实战示例。

### 变更
- 版本号通过 ldflags 注入（`-X main.version=...`），不再写死。
- `ensure` 错误提示给出 `RSB_HOME` 和 `build.sh` 两条修复路径。

---

## [0.2.0] - 2026-06-17

P1：常驻 daemon + 连接复用 + session。

### 新增
- **本地常驻 daemon**（`rsb-daemon`）：监听 unix socket，为每个 host 维护一条 SSH 长连接，多请求复用。
- **session + cwd 持久化**：`--session NAME` 让多条命令共享 cwd/env；`cd` 作为内建命令直接更新 session.cwd，跨命令生效（绝对路径 / 相对路径 / `~` 均支持）。
- **流式 stdin**：协议新增 `Stdin` / `EndStdin` / `Cancel` 帧；客户端 `--stdin` 把本地输入流式喂给远端进程。
- **多请求并发 + cancel**：同 session 命令串行（保证 cd 语义），不同 session 并发。
- `rsb daemon status|stop` 子命令。

### 变更
- 协议升级到 v2：新增 `Stdin` / `EndStdin` / `Cancel` / `attach` 帧类型；`Request` 增加 `Session` 字段、`StdinClosed` 便捷字段。
- `rsb exec` 改走 daemon（自动拉起），不再每次命令新建 SSH 连接。
- stdout 帧写入用 mutex 序列化，支持多请求并发输出。

### 修复
- goroutine 时序竞态：Stdin 帧到达时 handleExec 未注册 runningReq → 改用 `sync.WaitGroup` + 同步注册。
- session 并发竞态：同 session 命令乱序 → 给 session 加 mutex 串行化。
- `eofCh` 重复关闭 panic → `sync.Once` 幂等。
- daemon EOF 时丢弃未完成请求 → `pending.Wait()` 等所有 in-flight 请求完成。

---

## [0.1.0] - 2026-06-17

P0：核心证明——argv 数组直达 execve，消灭引号地狱。

### 新增
- **协议层**（`internal/protocol`）：长度前缀 JSON 帧（`[4B BE 长度][JSON]`），定义 Request / Output / Result / Error / Hello 消息类型。
- **rsb-agent**（远端 daemon）：读帧 → `execve(argv, env)` → 流式回传 Output + Result。全程不经 shell。
- **rsb client**：`rsb exec <host> --argv '<json>'`，构造 Request 帧发到 agent，流式收响应打印到本地 stdout/stderr，退出码 = 远端退出码。
- **本地模式**：`rsb exec --local` 不走 SSH，直接 spawn 本地 rsb-agent，用于无 SSH 服务器的调试。
- `rsb ensure <host>`：scp rsb-agent 到远端 `~/.rsb/`。

### 核心价值验证
- 双引号 / 单引号 / `$VAR` / `${VAR}` 全部原样到达远端 execve，不展开、不转义错。
- 含空格的 argv 参数边界完整。
- 非零退出码正确传播。

---

## 版本号约定

- **主版本号**：协议不兼容变更。
- **次版本号**：向后兼容的新功能。
- **修订号**：bug 修复。

每个版本的二进制通过 `scripts/build.sh` 编译，版本号经 ldflags 注入，可用 `rsb version` 查看。
