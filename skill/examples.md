# rsb 实战示例

这些例子展示 agent 在真实场景里如何用 rsb。两条核心原则：
1. **命令优先用 `--` 简写**（`rsb exec host -- cmd args`），需要程序化构造时才用 `--argv '<json>'`
2. **文件操作走 rsb**（cp/sync/cat），不经 scp，无路径转义

## 0. 选对二进制（所有示例的前提）

```bash
RSB="$(pwd)/bin/$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64')/rsb"
```

## 1. 探查远程主机（-- 简写）

```bash
# -- 后面直接是命令，CLI 本地组装 argv，不经 shell
"$RSB" exec prod -- uname -a
"$RSB" exec prod -- df -h /
"$RSB" exec prod -- free -m

# 用 session 保持位置，连续探查
"$RSB" exec prod --session explore -- cd /var/log
"$RSB" exec prod --session explore -- ls -la
"$RSB" exec prod --session explore -- tail -n 50 syslog
```

## 2. 文件传输（cp / sync / cat）

### 传单个文件（替代 scp）

```bash
# 上传代码到远端
"$RSB" cp ./service.py prod:/opt/app/service.py

# 下载日志回来分析
"$RSB" cp prod:/var/log/app.log ./app.log
```

### 增量同步整个目录

```bash
# 只传变化的文件（mtime+size 判定）
"$RSB" sync ./orchestrator prod:/home/ubuntu/app/orchestrator

# 先看看会传哪些
"$RSB" sync ./orchestrator prod:/home/ubuntu/app/orchestrator --dry-run
```

### 读远端文件（不下载，直接看）

```bash
# 看大日志的前 100 行 —— 只传这 100 行的字节，不是整个文件
"$RSB" cat prod:/var/log/app.log --lines=1:100

# 看配置文件的某一行
"$RSB" cat prod:/etc/nginx/nginx.conf --lines=50

# 只读前 4KB（防止巨型文件 OOM）
"$RSB" cat prod:/var/log/app.log --max-bytes=4096
```

> `rsb cat` 是 agent 看远端配置/日志的首选——行范围在远端执行，省带宽和 token。

### 搜远端代码库（只传命中行）

```bash
# 搜代码里的 TODO/FIXME —— rg 在远端跑，只传命中行回来
"$RSB" grep prod 'TODO|FIXME' /opt/app/src

# 忽略大小写，只搜 Python 文件
"$RSB" grep prod -i --glob='*.py' 'def main' /opt/app

# 限制命中数，避免巨量结果
"$RSB" grep prod --max-matches=20 'database' /opt/app/config
```

> `rsb grep` 比 `rsb exec prod -- grep -rn ...` 更省带宽：ripgrep 的 --json 输出被解析成结构化的 file:line:content，且搜索在远端 CPU 上跑（用远端的 gitignore）。

## 3. 操作配置文件（复杂参数安全）

```bash
# jq 读嵌套字段 —— -- 简写里引号原样到达，不会出错
"$RSB" exec prod -- jq '.database.host' /opt/app/config.json

# grep 复杂正则 —— 正则里的特殊字符安全
"$RSB" exec prod -- grep -rEn 'TODO|FIXME|XXX' src/

# 需要程序化构造 argv 时，用 --argv JSON 形式
"$RSB" exec prod --argv '["jq",".\"database\".\"host\"","/opt/app/config.json"]'
```

## 4. Docker 容器调试（compose 服务名）

```bash
# 列容器
"$RSB" exec prod -- docker ps --format '{{.Names}}\t{{.Status}}'

# --container 支持 compose 服务名（api 自动找到 myproject-api-1）
"$RSB" exec prod --container api -- env
"$RSB" exec prod --container api -- cat /app/config.json
"$RSB" exec prod --container api -- ps aux

# 容器内看日志（-- 简写）
"$RSB" exec prod --container api -- tail -n 100 /var/log/app.log
```

## 5. 部署 / 重启服务

```bash
# 用 session 模拟完整部署流程
"$RSB" exec prod --session deploy -- cd /opt/myapp
"$RSB" exec prod --session deploy -- git pull
"$RSB" exec prod --session deploy -- docker compose up -d --build
"$RSB" exec prod --session deploy -- docker compose ps

# 同步代码后重启
"$RSB" sync ./src prod:/opt/myapp/src
"$RSB" exec prod --session deploy -- docker compose restart api

# 验证健康
"$RSB" exec prod -- curl -sf http://localhost:8080/health
```

## 6. 排查问题（退出码驱动条件判断）

```bash
# rsb 退出码 = 远端命令退出码，可直接判断
if "$RSB" exec prod -- pgrep -x nginx >/dev/null 2>&1; then
    echo "nginx 在运行"
else
    echo "nginx 没运行，启动它"
    "$RSB" exec prod -- systemctl start nginx
fi
```

## 7. 传本地输入给远端

```bash
# 把本地文件内容喂给远端命令
cat local_data.json | "$RSB" exec prod --stdin -- python3 -m json.tool

# 容器内管道
echo "query" | "$RSB" exec prod --container db --stdin -- psql -U postgres
```

## 8. 交互式 repl（连续调试）

```bash
"$RSB" repl prod --session debug
# 进入后：
#   [prod:debug] ["cd","/opt/app"]
#   [prod:debug] ["ls"]
#   [prod:debug] :container api       # 后续命令进容器
#   [prod:debug] ["env"]
#   [prod:debug] :quit
```

## 反面教材：绝对不要这样写

```bash
# ❌ 这些会让你几乎必然写错转义
ssh prod "grep -rEn 'TODO|FIXME' \"src/\" | head"
ssh prod "docker exec api sh -c 'cat /app/\"config file.json\"'"
scp ./service.py prod:/opt/app/service.py   # 路径含空格就炸

# ✅ 对应的 rsb 写法（确定性正确）
"$RSB" exec prod -- grep -rEn 'TODO|FIXME' src/
"$RSB" exec prod --container api -- cat '/app/config file.json'
"$RSB" cp ./service.py prod:/opt/app/service.py
```
