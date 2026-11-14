# 双通道接入记录

双通道（auto）已对齐 `t508708/icloud-mail` 提交 `886eb52` 的 19+4 自动调度，而非上一版的一新一旧轮流、每通道五个一批。

- 一小时 23 个随机时隙，至少间隔 120 秒；旧通道在第 6、12、18、23 个时隙。
- 新通道掉线降级为旧通道每小时 5 个分散时隙；旧会话不可用时跳过所有时隙并保留任务。
- 每通道滚动预算、共享小时/日预算、冷却、计划和进度持久化；尝试发出前保存账本。
- 单通道限流不推迟另一通道；共享限流推迟整个计划；不补发积压时隙。
- 重启续跑等待中的双通道任务；在途创建状态不确定时暂停核对，不自动重发。
- 保留本应用有限 count、750 总数上限和现有收件功能，没有整体迁移参考项目的数据库/补库/邮箱池。
- 旧通道和单独新通道模式不变；手动 API 不纳入本次双通道自动计划账本。
- 新通道真实创建仍需本人先完成 Apple 登录和双重认证；测试不使用真实 Apple 凭据。

## 来源

协议客户端及原始许可/来源范围在 `internal/icloudmail/`。调度适配依据为参考仓库的 `internal/domain/apple_creation_policy.go`、`internal/autocreate/manager.go`、`internal/store/apple_creation_channel_budget.go` 和 `internal/store/apple_creation_budget.go`，JSON 存储替代其数据库适配器。

## 检查

- Go 全套 internal 包回归测试。
- 前端全套 Vitest、生产构建与本次修改文件的 ESLint。
- 原有全仓 ESLint 在未修改的 `MailboxDialog.tsx:25` 有 `react-hooks/set-state-in-effect` 错误，另有既存 Fast Refresh 警告；本次未改动这些无关文件。
- 原有前端测试输出仍有 Dialog 的嵌套 button 警告及 Inbox mock 请求提示；本次不修改既有 Dialog 实现。
