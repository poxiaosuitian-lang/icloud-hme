# Windows 协助 Apple Account 新通道登录

服务器 Codex 的完整部署任务见 [SERVER_CODEX_APPLE_LOGIN_DEPLOY.md](SERVER_CODEX_APPLE_LOGIN_DEPLOY.md)。

创建别名时遇到 `login_required` 的按需本机恢复任务，见 [SERVER_CODEX_ON_DEMAND_LOGIN.md](SERVER_CODEX_ON_DEMAND_LOGIN.md)。Windows 接收端已实现，服务器队列与新接口仍需按该文档实现；不新增定时 Apple 会话检查。

`POST /api/integrations/appleservices/apple-login` 使用现有 `ICLOUD_HME_IMPORT_TOKEN` Bearer 鉴权；沿用导入接口的请求限流、并发限制和无凭据日志。与管理员 Session、服务 API token 隔离，未配置导入密钥时返回 404。请求最大 8 KiB，不允许跳转、代理参数或任意账号 ID。

请求公共字段：`version:1`、`source_id`（与已导入账号一致）、`apple_id`、`operation`。只对已通过导入验证并绑定该 source_id 和 Apple 身份的账号操作。

| operation | 额外字段 | 行为 |
|---|---|---|
| `status` | 无 | 实际验证/刷新 Apple Account 管理会话，返回 `ready` 或 `login_required` |
| `login` | `password` | SRP 登录，返回 `ready` 或 `otp_required`；已有未过期集成 challenge 时复用 |
| `verify` | `challenge`、`otp_code` | 按待验证会话原通道验证六位码，信任并持久化会话 |
| `verify_device` | `challenge`、`otp_code` | 明确按受信任设备通道验证本机或 iPhone/Mac 六位码，不调用短信验证端点 |

成功信封为 `{"success":true,"data":{"source_id":"appleservices-...","state":"ready"}}`。待验证码时 state 为 `otp_required`，同时返回 64 位十六进制 `challenge` 和 `verification`（`trusted_device` 或 `sms`）。返回 OTP 状态不等于认证完成。无效验证码保留原 challenge；过期、重启、账号/代理变化或验证会话被其他登录替换后必须重新开始。

错误返回静态 code，不透传 Apple 正文。认证步骤不创建邮箱别名、不订阅、不发送邮件，也不修改 Web/IMAP 凭据。主密码与验证码只在请求中短暂使用，不保存、不写日志；Apple Account 独立会话按既有受限数据文件持久化，既有维护任务负责续期。

响应新增 `device_code_supported:true`。Windows 客户端先检查 status，仅在需要时发主密码；支持此能力的服务器优先通过 `verify_device` 尝试一次本机六位码，即使 Apple 默认选中了手机号。失败时保留原短信验证选项及旋转后的会话头，面板可选择 iPhone/Mac 或手机短信，按来源提交。旧服务器未声明此能力时不将本机或 iPhone 验证码提交到短信端点。不会自动循环、请求短信或伪报就绪。

部署：保留数据卷、环境文件及 `ICLOUD_HME_IMPORT_TOKEN`，在 Linux 运行 `go test ./internal/account ./internal/server ./internal/icloudmail/apple` 后重建当前项目镜像/程序（Compose build 部署使用 `docker compose up -d --build`）。确认新路由未鉴权为 401，再从 Windows 面板确认同步。不需要更换密钥或服务器 URL。

### 身份绑定兼容性

Apple ID 与 iCloud 收件地址可以不同。集成登录按导入时 Apple validate 已验证的
`verified_apple_id`、DSID 和 `source_id` 绑定，SRP 使用该 Apple ID，不使用收件地址
替代。旧的非导入账号保留原登录规则。待验证 challenge 还绑定导入凭据版本、
收件地址及代理/区域配置；重新导入或配置变化后，旧 challenge 和在途登录结果
不会覆盖新账号状态。主密码和验证码仍不落盘。
