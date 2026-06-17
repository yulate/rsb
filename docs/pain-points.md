# rsb 实战痛点记录

每行一条痛点，修复后标注 `[已修复]`。

- [x] [已修复] P1. `ensure` 依赖 cwd 查找 agent → 改为基于 home 发现机制（RSB_HOME > 可执行文件路径反推 > cwd），见 `paths.AgentForPlatform`
- [x] [已修复] P2. daemon 二进制查找依赖 cwd → 同上，`findDaemonBinary` 改用 `paths.LocalPlatformDir()`
- [x] [已修复] P3. SSH 密码认证失败只报 "read hello: EOF" → daemon 端在握手期捕获 agent stderr，按 auth/refused/resolve 分类给出具体修复提示，通过 Error 帧回传 client
- [x] [已修复] P4. `--container` 默认走 nsenter 被权限挡住 → 默认改走 `docker exec`，nsenter 仅在 `RSB_CONTAINER_MODE=nsenter` 时用，失败自动 fallback docker exec
- [x] [已修复] P5. `--stdin` 套 `docker exec -i` 无输出 → 根因是 `pumpStdin` 的 select 竞态丢数据；改为 eofCh 触发后先排空 stdinCh 再关管道
- [x] [已修复] P6. 缺自检/安装命令 → 新增 `rsb doctor`（home/二进制/daemon/ssh/docker 自检）和 `rsb install-local`（建 bin/ symlink）
- [x] [已修复] P7. docker.sock 权限失败提示不具体 → `docker.BuildArgv` 检测到权限问题时返回 `SocketPermissionError`，提示 `usermod -aG docker <user>`
