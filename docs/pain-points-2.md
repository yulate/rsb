# rsb 实战痛点记录 (第二批次)

记录第二轮真实使用反馈的痛点。修复后标注 `[已修复]`。

- [x] [已修复] P8. `ensure` scp 部分失败后远端残留旧版，doctor 误报 ok → ensure 改为上传临时文件 + 原子 mv + 上传后强制 sha256 本地/远端校验，不一致明确失败
- [x] [已修复] P9. `doctor` 容器模式只读本地预期值 → 新增 `doctor --container=NAME` 执行真实 `docker exec <NAME> true` smoke test
- [x] [已修复] P10. 远端 agent 版本不可见 → agent 新增 `--version` 输出；新增 `rsb agent-version <host>` 命令；doctor 输出远端 agent 版本 + sha256 对比
- [x] [已修复] P11. `--container` 走旧 agent 的 nsenter 报 permission denied 误导权限问题 → client 检测到 nsenter permission denied 时提示"stale agent，运行 rsb ensure --force"
- [x] [已修复] P12. `ensure` 缺强制覆盖 → 新增 `--force`；默认模式 hash 匹配则跳过（幂等），`--force` 始终重传+校验
- [x] [已修复] P13. `RSB_CONTAINER_MODE` 设置后无生效反馈 → doctor 同时显示本地期望模式，并提示用 `--container` 做真实测试验证远端
- [x] [已修复] P14. 文档"永远用 --container"过强 → SKILL.md 新增"容器执行故障兜底"段落：--container 异常时先用 argv 手动 docker exec + 检查 agent hash
