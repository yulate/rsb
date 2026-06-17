# rsb 实战示例

这些例子展示 agent 在真实场景里如何用 rsb。核心原则不变：
**命令永远是 argv JSON 数组，永不拼字符串走 ssh。**

## 1. 探查远程主机

```bash
RSB="$(pwd)/bin/$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64')/rsb"

# 系统信息
"$RSB" exec prod --argv '["uname","-a"]'
"$RSB" exec prod --argv '["df","-h","/"]'
"$RSB" exec prod --argv '["free","-m"]'

# 用 session 保持位置，连续探查
"$RSB" exec prod --session explore --argv '["cd","/var/log"]'
"$RSB" exec prod --session explore --argv '["ls","-la"]'
"$RSB" exec prod --session explore --argv '["tail","-n","50","syslog"]'
```

## 2. 操作配置文件（复杂参数安全）

```bash
# 用 jq 读取嵌套字段 —— 引号在 JSON 里写 \"，不会出错
"$RSB" exec prod --argv '["jq",".\"database\".\"host\"","/opt/app/config.json"]'

# 写文件，内容含大量特殊字符
"$RSB" exec prod --argv '["sh","-c","cat > /tmp/test.conf <<EOF\nKEY=\"value with spaces\"\nREGEX=^a.*b$\nEOF"]'

# grep 复杂正则
"$RSB" exec prod --argv '["grep","-rEn","(TODO|FIXME|XXX)","src/"]'
```

## 3. Docker 容器调试

```bash
# 列出容器（在主机上跑 docker，不是进容器）
"$RSB" exec prod --argv '["docker","ps","--format","{{.Names}}\t{{.Status}}"]'

# 进入指定容器执行命令 —— argv 原样到达容器内进程
"$RSB" exec prod --container api-server --argv '["env"]'
"$RSB" exec prod --container api-server --argv '["cat","/app/config.json"]'
"$RSB" exec prod --container api-server --argv '["ps","aux"]'

# 容器内看日志（带引号的路径安全）
"$RSB" exec prod --container api-server --argv '["tail","-n","100","/var/log/app.log"]'
```

## 4. 部署 / 重启服务

```bash
# 用 session 模拟完整部署流程
"$RSB" exec prod --session deploy --argv '["cd","/opt/myapp"]'
"$RSB" exec prod --session deploy --argv '["git","pull"]'
"$RSB" exec prod --session deploy --argv '["docker","compose","up","-d","--build"]'
"$RSB" exec prod --session deploy --argv '["docker","compose","ps"]'

# 验证服务起来了
"$RSB" exec prod --argv '["curl","-sf","http://localhost:8080/health"]'
```

## 5. 排查问题（退出码驱动条件判断）

```bash
# rsb 退出码 = 远端命令退出码，可直接判断
if "$RSB" exec prod --argv '["pgrep","-x","nginx"]' >/dev/null 2>&1; then
    echo "nginx 在运行"
else
    echo "nginx 没运行，尝试启动"
    "$RSB" exec prod --argv '["systemctl","start","nginx"]'
fi
```

## 6. 传本地输入给远端

```bash
# 把本地文件内容喂给远端命令
cat local_data.json | "$RSB" exec prod --stdin --argv '["python3","-m","json.tool"]'

# 交互式
"$RSB" exec prod --stdin --argv '["sh"]'
```

## 7. 交互式 repl（连续调试）

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
ssh prod "echo 'export KEY=\"value\"' >> ~/.bashrc"

# ✅ 对应的 rsb 写法（确定性正确）
"$RSB" exec prod --argv '["grep","-rEn","TODO|FIXME","src/"]'
"$RSB" exec prod --container api --argv '["cat","/app/config file.json"]'
"$RSB" exec prod --argv '["sh","-c","echo '\''export KEY=\"value\"'\'' >> ~/.bashrc"]'
```
