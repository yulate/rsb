# rsb 实战痛点记录 (第三批次)

记录第三轮真实使用反馈的痛点。修复后标注 `[已修复]`。

- [x] [已修复] P15. `rsb exec` 在 daemon 的 host 连接死亡时永久卡住，`--timeout` 无效 → 三层修复：(1) client 端 inactivity timeout 兜底（默认 60s 无任何帧则失败，可用 RSB_INACTIVITY_TIMEOUT 调）；(2) daemon 的 pumpFromAgent 退出时自动 close + 从 pool 标记死连接，下次请求自动重建；(3) send() 加 recover 防并发 close channel 的 panic
- [x] [已修复] P16. agent 端超时杀进程后退出码是 -1（os.Exit 后变 255，语义模糊）→ client 收到 Result 时规范化：TIMEOUT→124，其他信号→137
