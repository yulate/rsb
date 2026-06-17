# rsb — 面向 AI agent 的远程命令桥接

<p align="center">
  <strong>在远程主机和容器上执行命令，告别引号地狱。</strong><br>
  argv 以 JSON 数组的形式直达目标端的 <code>execve</code> —— 全程不经过 shell。
</p>

<p align="center">
  <a href="../README.md">English</a> · <strong>中文</strong>
</p>

---

[English](../README.md) | **中文**

`rsb` 让 LLM agent（Claude Code、Codex、Cursor 等）操作远程服务器就像操作本地
机器一样。它解决了让 `ssh host "cmd"` 对 agent 彻底不可用的两个问题：

1. **引号地狱**。一条命令字符串要穿越 *本地 shell → 远程 shell →（可选）容器
   shell*，每一层都要转义，复杂度层层叠加。没有任何 LLM 能可靠地写出
   `ssh prod "docker exec api sh -c 'echo \"$X\"'"` 这种三层嵌套。
2. **状态丢失**。每次 `ssh host "cmd"` 都是独立的登录 shell。`cd /app` 之后再
   `ssh host "ls"` 又回到了 `~` —— agent 没法表达"继续在这个目录里操作"。

**rsb 的解法**：命令是一个 `[]string` argv，以长度前缀 JSON 帧发送到远端一个
很小的 agent 进程，由它直接 `execve` 执行。没有任何 shell 解析它 —— 所以根本
不存在"转义对不对"这层。常驻 daemon + session 让 `cd` 和环境变量跨命令持久。

> 灵感来自 [Warp](https://www.warp.dev) 的 SSH extension（远端放一个 companion
> 进程、走结构化协议、一个 server 服务多个 session）—— 但 rsb 面向的是
> **agent 原生的远程执行**，不是终端 UI。

## 10 秒看懂

```bash
# 这条命令里有双引号、单引号、还有 $ 变量。
# 用 ssh 的话就是引号地狱。用 rsb 直接就对 —— argv 是数组，每个字符原样到达。
rsb exec prod --argv '["echo","他说了 \"你好\" 和 '"'"'再见'"'"', $HOME 原样保留"]'
# => 他说了 "你好" 和 '再见', $HOME 原样保留
```

容器里也一样 —— `--container` 让 argv 完整进入容器内进程，仍然没有 shell：

```bash
rsb exec prod --container api --argv '["env"]'
```

## 快速上手

```bash
# 1. 编译（或从 release 下载预编译二进制）
go build -o bin/rsb ./cmd/rsb
go build -o bin/rsb-agent ./cmd/rsb-agent
go build -o bin/rsb-daemon ./cmd/rsb-daemon

# 2. 一次性：把当前平台的二进制 symlink 到 bin/
rsb install-local

# 3. 验证环境
rsb doctor                        # 自检：home、二进制、daemon、docker
rsb doctor prod --container=api   # 同时探测远程主机 + 真实容器测试

# 4. 在远程主机上安装 agent（探测 OS/arch，scp 匹配的二进制，sha256 校验）
rsb ensure prod

# 5. 执行命令
rsb exec prod --argv '["kubectl","logs","deploy/api","--tail=50"]'
rsb exec prod --session work --argv '["cd","/opt/app"]'
rsb exec prod --session work --argv '["ls"]'    # 仍在 /opt/app
```

## 工作原理

```
你的 agent / shell
   │  rsb exec  (或 rsb repl)
   ▼
本地 daemon ──unix socket──►  rsb-daemon ──ssh──►  rsb-agent (远端, 常驻)
                              (每个 host 一条连接,    ├─ execve(argv)   ← 无 shell
                               连接复用)             ├─ session 表     ← cwd/env 持久
                                                     └─ docker adapter ← argv 进容器
```

- **零新增端口**。daemon 走 unix socket；远端 agent 是 `sshd` 的子进程，通过
  现有 SSH 通道的 stdin/stdout 通信。你能 `ssh` 到的主机，rsb 就能用 —— 不用动防火墙。
- **argv 直达 execve**。协议传输的是 `[]string`，不是命令字符串。agent 用 `PATH`
  解析 `argv[0]` 然后调 `execve`。引号、`$`、空格、反引号 —— 没有一个是"特殊字符"，
  因为根本没有任何东西在解析它们。
- **常驻 daemon + session**。每个 host 保持一条 SSH 连接并复用。同一 session 的
  命令串行执行（这样 `cd` 才有意义），不同 session 并发执行。

## 功能

| | 状态 |
|---|---|
| argv 数组 → execve（无 shell，零引号问题） | ✅ |
| 常驻 daemon + 连接复用 | ✅ |
| session：`cd` / cwd / env 跨命令持久 | ✅ |
| 流式 stdin（管道、交互） | ✅ |
| Docker 容器执行（`--container`） | ✅ |
| 多请求并发 + 取消 | ✅ |
| 交互式 REPL（`rsb repl`） | ✅ |
| `ensure` 原子安装 + sha256 校验 | ✅ |
| `doctor` 自检 + 真实容器 smoke test | ✅ |
| 远端 agent 版本 + hash 可见性 | ✅ |
| 跨平台预编译二进制（linux/macOS × amd64/arm64） | ✅ |
| Kubernetes（`kubectl exec`）adapter | 🔜 |
| Compose 服务名解析 | 🔜 |

## 为什么不用 ……？

| 工具 | 解决了什么 | 没解决什么 |
|---|---|---|
| `ssh host "cmd"` / ansible / fabric | 封装 ssh | 仍是命令*字符串* —— 引号地狱原样存在 |
| paramiko / asyncssh | SSH 库 | `exec_command(string)` —— shell 解析问题依旧 |
| tmux Control Mode（Warp 旧版） | 连接复用 | 面向终端 UI，不是 agent 执行接口 |
| Warp SSH extension | 结构化协议 + 复用 | 闭源，面向终端用户 |
| VS Code Remote-SSH | 结构化协议 | IDE 专用，不是 agent 接口 |
| **rsb** | **argv 到 execve + daemon + session + 容器** | — |

rsb 处在一个空白位：一个**面向 agent 的远程执行运行时**，开源、单二进制、把
argv 端到端当作一等公民对待。

## 安装

### 从源码

```bash
git clone https://github.com/<owner>/rsb.git
cd rsb
./scripts/build.sh          # 编译 3 个二进制 × 3 个平台，输出到 skill/bin/
```

### 预编译二进制

从 [Releases](../../releases) 下载你平台的压缩包。每个包里有对应平台的
`rsb`、`rsb-agent`、`rsb-daemon`。解压后跑 `rsb install-local`。

### 作为 agent skill

`skill/` 目录是一个自包含、可直接装给 agent 的包：`SKILL.md` 教 agent 何时、
如何使用 rsb；`bin/` 里有全部平台的预编译二进制。把 `skill/` 复制进你的 agent
skills 目录（比如 `~/.codex/skills/rsb/`）即可。

## 命令参考

```
rsb exec <host> --argv '<json>' [选项]
  --argv '<json>'     （必填）JSON 字符串数组，如 '["ls","-la"]'
  --cwd DIR           目标端的工作目录
  --env K=V           环境变量（可重复）；值不会被展开
  --timeout MS        N 毫秒后杀掉命令
  --session NAME      跨命令共享 cwd/env；"cd" 按 session 持久
  --container NAME    在 Docker 容器内执行（argv 原样到达容器进程）
  --stdin             把本地 stdin 管道给远程命令
  --local             在本机执行（不走 SSH）

rsb repl <host> [--session NAME]              交互式多命令会话
rsb ensure <host> [--force]                   安装/升级远端 agent（sha256 校验）
rsb agent-version <host>                      查看远端 agent 版本
rsb doctor [host] [--container=NAME]          自检（带 --container 时做真实容器测试）
rsb install-local                             把当前平台二进制 symlink 到 bin/
rsb daemon status|stop                        管理本地 daemon（通常自动）
rsb version

运行 `rsb <命令> --help` 查看每个命令的详细帮助。
```

**退出码**：`rsb exec` 的退出码 = 远程命令的退出码。

### 容器模式

默认 rsb 通过 `docker exec` 进入容器（对有 docker 组权限的普通用户友好）。对
root/特权主机，设置 `RSB_CONTAINER_MODE=nsenter` 直接用 `nsenter`（更快，跳过
docker daemon）。

如果 `--container` 报 `nsenter: Permission denied`，说明你的远端 agent 很可能
是**旧版（0.5.0 之前）**，那个版本默认走 nsenter。修复：

```bash
rsb ensure <host> --force          # 升级 + 校验
rsb agent-version <host>           # 确认版本一致
```

永远有效的兜底（仍是 argv，仍无 shell）：

```bash
rsb exec prod --argv '["docker","exec","api","ls","/app"]'
```

## argv 如何存活（核心不变量）

```
agent 代码 (Python/JS/shell)
   │  argv = ["echo", '有"双引号"和 $HOME']   ← 你写一个列表
   │  json.dumps(argv)                       ← JSON 转义是确定性的
   ▼
JSON 线上格式: ["echo","有\"双引号\"和 $HOME"]
   │  rsb 以长度前缀帧传输
   ▼
远端 execve(["echo", '有"双引号"和 $HOME'])   ← 精确还原，无 shell
   ▼
echo 收到: 有"双引号"和 $HOME                ← 正确
```

对比传统 SSH：同样的意图要穿越 N 个 shell 解析器，每个有自己的转义规则，
agent 要在脑子里模拟每一层的结果。rsb 把这些解析器整个去掉了。

## 项目结构

```
cmd/
  rsb/          客户端 CLI（exec、repl、ensure、doctor ……）
  rsb-agent/    远端 daemon（多请求、session、execve、docker）
  rsb-daemon/   本地 daemon（连接池、帧路由）
internal/
  protocol/     长度前缀 JSON 帧编解码 + 消息类型
  daemon/       host 连接池 + client 桥接
  client/       连 daemon / 自动拉起 / 流式 stdin / repl
  docker/       容器 adapter（默认 docker exec，nsenter 可选）
  paths/        安装目录发现（RSB_HOME > 可执行文件路径 > cwd）
skill/          自包含的 agent skill 包
docs/           更新日志 + 痛点记录
scripts/        交叉编译脚本（3 个平台）
```

## Roadmap

- [ ] Kubernetes adapter（`--container` → `kubectl exec`）
- [ ] Compose 服务名解析（`--container api` → `project-api-1`）
- [ ] `rsb scp`（复用同一条连接传文件）
- [ ] 按 host 的命令白名单 / 危险命令审计 hook

## License

MIT
