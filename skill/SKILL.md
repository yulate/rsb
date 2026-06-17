---
name: rsb
description: |
  通过 rsb 在远程主机或容器内执行命令，彻底避免 SSH 引号地狱和状态丢失。
  命令以 argv 数组传输，远端 execve 直接执行，全程不经 shell，引号/$/空格原样保留。
  支持会话内 cwd 持久化（cd 跨命令生效）和容器执行（docker exec/容器内执行）。
  当需要在远程服务器、SSH 主机或其上的 Docker 容器里运行命令、查看文件、调试服务时使用。
---

# rsb — remote shell bridge

`rsb` 让你以 **argv 数组** 的形式在远程主机或容器内执行命令。它的核心价值：

- **没有引号地狱**：命令是 JSON 数组 `["ls","-la"]`，不是字符串，远端 `execve` 直接执行，**不经任何 shell**。引号、`$VAR`、空格、反引号全部原样到达目标进程。
- **状态持久**：用 `--session` 让多条命令共享 cwd/env。`cd /app` 之后下一条命令仍在 `/app`。
- **容器一等公民**：`--container NAME` 直接在容器内执行，argv 同样原样到达容器内进程。

> **绝对不要**用 `ssh host "cmd"` 或 `ssh host "docker exec c cmd"`——那是引号地狱的根源，你几乎必然写错转义。**永远用 rsb。**

## 何时使用

- 需要在远程主机/SSH 目标上运行任何命令
- 需要进入 Docker 容器执行命令或查看状态
- 命令参数包含引号、`$`、空格、反斜杠、JSON、正则等特殊字符
- 需要连续操作（`cd` 后 `ls`，配置后重启等）

## 二进制位置

预编译二进制按 `<os>-<arch>` 分目录存放在本 skill 的 `bin/` 下：

| 你的运行平台 | 路径 |
|---|---|
| Linux x86_64（多数云服务器）| `bin/linux-amd64/` |
| Linux ARM64（Graviton/树莓派）| `bin/linux-arm64/` |
| macOS Apple Silicon | `bin/darwin-arm64/` |

每个目录下有三个二进制：
- `rsb` — 客户端，**你在本机调用的就是这个**
- `rsb-daemon` — 本地常驻进程（`rsb` 自动拉起，无需手动管）
- `rsb-agent` — 远端执行器（`rsb ensure` 自动安装到目标主机）

**选择正确的二进制**：先确定当前 shell 运行在哪个平台：

```bash
uname -sm   # 输出如 "Darwin arm64" 或 "Linux x86_64"
```

然后在后续命令里把 `rsb` 替换为对应路径，例如 `./bin/darwin-arm64/rsb`。建议先设一个变量：

```bash
# 根据当前平台选择，一次性设置
RSB="$(pwd)/bin/$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64')/rsb"
"$RSB" version   # 验证可用
```

## 核心用法：rsb exec

### 基本执行

```bash
rsb exec <host> --argv '<JSON数组>'
```

`--argv` 接收一个 **JSON 字符串数组**，每个元素是一个 argv 参数。

```bash
# 简单命令
rsb exec prod --argv '["ls","-la","/var/log"]'

# 含双引号、单引号、$变量 —— 全部原样保留，不会展开/转义错
rsb exec prod --argv '["printf","%s\n","config has \"key\": \"val\" and $HOME"]'

# grep 带复杂正则 —— 正则里的特殊字符安全
rsb exec prod --argv '["grep","-rEn","TODO|FIXME|HACK","src/"]'
```

**对比传统 SSH 的灾难**（绝对不要这样做）：
```bash
# ❌ 这会让你几乎必然写错转义
ssh prod "grep -rEn 'TODO|FIXME' \"src/\" | head"
ssh prod "docker exec api sh -c 'echo \"$CONFIG\"'"
```

### 本地模式（调试/同机）

```bash
rsb exec --local --argv '["pwd"]'
```

### 在容器内执行

```bash
rsb exec <host> --container <容器名> --argv '<JSON数组>'
```

argv 原样到达容器内进程，**不经过 `docker exec` 的 shell**。

```bash
# 在 api 容器里看进程列表
rsb exec prod --container api --argv '["ps","aux"]'

# 容器内执行带引号的命令 —— 同样免疫
rsb exec prod --container api --argv '["sh","-c","echo \"PG ready at $PGHOST:$PGPORT\""]']
```

#### 容器执行故障兜底（重要）

如果 `--container` 报 `nsenter: Permission denied` 或行为异常，**几乎一定是远端 agent 是旧版**（旧版默认走 nsenter，新版默认走 docker exec）。排查步骤：

```bash
# 1. 检查远端 agent 版本（和本地 rsb version 对比）
rsb agent-version prod

# 2. 强制重装远端 agent（原子 mv + sha256 校验，确保真正更新）
rsb ensure prod --force

# 3. 用 doctor 做真实容器 smoke test
rsb doctor prod --container=api

# 4. 兜底：如果 --container 仍异常，直接用 argv 形式手动跑 docker exec
#    （仍然比 SSH 字符串安全得多，argv 原样到达）
rsb exec prod --argv '["docker","exec","api","ls","/app"]'
```

### 会话 + cwd 持久化

用 `--session NAME` 让多条命令共享状态。`cd` 会持久化：

```bash
rsb exec prod --session deploy --argv '["cd","/opt/app"]'
rsb exec prod --session deploy --argv '["ls"]'        # 仍是 /opt/app 的内容
rsb exec prod --session deploy --argv '["pwd"]'       # 输出 /opt/app
```

不同 session 互相隔离，可并发操作不同目录。

### 环境变量注入

```bash
rsb exec prod --env 'DEBUG=1' --env 'LOG_LEVEL=warn' --argv '["./run.sh"]'
```

env 值原样传递，**不展开 `$`**（因为是 execve 不是 shell）。

### 超时

```bash
rsb exec prod --timeout 30000 --argv '["curl","http://health"]'   # 30 秒
```

### 管道输入

```bash
echo "input data" | rsb exec prod --stdin --argv '["cat"]'
```

## 安装：rsb ensure

首次对一台主机操作前，把 `rsb-agent` 安装到远端 `~/.rsb/`：

```bash
rsb ensure <host>
```

这会自动检测远端的 OS/arch，从本地 rsb 安装目录（通过 home 发现定位，**不依赖当前工作目录**）找到匹配的二进制 scp 过去。

> 注意：远端 agent 必须匹配**远端**平台。如果远端是 linux-arm64 而你是 macOS，`ensure` 会传 linux-arm64 版的 agent。

## 自检：rsb doctor

第一次使用或遇到问题时，跑自检：

```bash
rsb doctor [host]
```

输出各组件状态：install home 定位、本平台三个二进制、daemon、SSH host 连通性、远端 agent 是否已装、docker 访问权限与容器执行模式。任一 FAIL 会非零退出。

## 本地快捷：rsb install-local

```bash
rsb install-local
```

在 `<home>/bin/` 下创建 `rsb`、`rsb-daemon`、`rsb-agent` 三个 symlink 指向当前平台的二进制。之后可直接 `bin/rsb` 调用，或加入 PATH：

```bash
export PATH=$(pwd)/bin:$PATH
```

## 完整选项速查

```
rsb exec <host> --argv '<json>' [flags]
  --argv '<json>'    必填，JSON 字符串数组
  --cwd DIR          工作目录
  --env K=V          环境变量（可重复）
  --timeout MS       超时毫秒
  --session NAME     会话名（cwd/env 跨命令持久）
  --container NAME   在容器内执行
  --stdin            从本地 stdin 读输入喂给远端
  --local            本机执行，不连 SSH

rsb repl <host> [--session NAME]    交互式多命令
rsb ensure <host>                   安装远端 agent
rsb doctor [host]                   自检（home/二进制/daemon/ssh/docker）
rsb install-local                   建 bin/ symlink，方便加入 PATH
rsb daemon status|stop              管理本地 daemon（通常自动）
rsb version
```

容器执行模式可选：默认 `docker exec`（普通用户友好）；设 `RSB_CONTAINER_MODE=nsenter` 用 nsenter（需 root，更快）。

## agent 操作清单（典型工作流）

```bash
# 1. 选对二进制
RSB="$(pwd)/bin/$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64')/rsb"

# 2. 首次安装 agent 到目标主机
"$RSB" ensure prod-host

# 3. 探查环境（用 session 保持位置）
"$RSB" exec prod-host --session work --argv '["uname","-a"]'
"$RSB" exec prod-host --session work --argv '["cd","/opt/myapp"]'
"$RSB" exec prod-host --session work --argv '["ls","-la"]'

# 4. 进容器调试
"$RSB" exec prod-host --container myapp-web --argv '["env"]'
"$RSB" exec prod-host --container myapp-web --argv '["cat","/app/config.json"]'

# 5. 安全地传复杂参数（这是 rsb 的强项）
"$RSB" exec prod-host --argv '["jq", ".\"database\".\"host\"", "config.json"]'
```

## 关键原则（务必遵守）

1. **永远用 `--argv` JSON 数组**，永远不要把命令拼成字符串去 `ssh`。
2. **复杂参数放心写**：双引号在 JSON 里写成 `\"`，单引号无需转义，`$` 无需转义。JSON 转义规则是确定性的，不像 shell。
3. **连续操作用 `--session`**：让 `cd` 生效，避免每次重新定位。
4. **容器用 `--container`**：不要手写 `docker exec ... sh -c "..."`。
5. **选对二进制平台**：客户端匹配本机，agent 匹配远端（`ensure` 自动处理）。

## 退出码

`rsb` 的退出码 = 远端命令的真实退出码。`exit 42` → rsb 也退出 42。可直接据此做条件判断。

## 它如何工作（原理，帮助理解）

```
你的 shell
  └─ rsb exec ──unix socket──► rsb-daemon ──ssh──► rsb-agent (远端)
                                (常驻,连接复用)        ├─ execve(argv) 无 shell
                                                       ├─ session 表 (cwd/env)
                                                       └─ docker adapter
```

- **零端口**：daemon 用 unix socket，远端复用 SSH 的 22。rsb-agent 不监听任何端口。
- **argv 直达 execve**：没有 shell 解析层，所以不存在"转义对不对"的问题。
- **协议**：长度前缀 JSON 帧，二进制安全（`[]byte` 走 base64）。
