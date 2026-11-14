# iCloud Hide My Email API 文档

## 概述

HTTP JSON API，所有接口返回统一格式：

```json
{
  "success": true,
  "data": {}
}
```

**失败响应：**

```json
{
  "success": false,
  "code": "VALIDATION_ERROR",
  "message": "参数错误"
}
```

**稳定错误码：** `AUTH_REQUIRED`、`INVALID_CREDENTIALS`、`RATE_LIMITED`、`CSRF_INVALID`、`VALIDATION_ERROR`、`ACCOUNT_NOT_FOUND`、`OTP_REQUIRED`、`OTP_INVALID`、`UPSTREAM_UNAUTHORIZED`、`UPSTREAM_FORBIDDEN`、`UPSTREAM_RATE_LIMITED`、`UPSTREAM_FAILURE`、`INTERNAL_ERROR`

上游 HTTP 401 映射为 `401 UPSTREAM_UNAUTHORIZED`。普通上游 HTTP 403 映射为 `502 UPSTREAM_FORBIDDEN`，表示操作被拒绝，不直接断言 Cookie 过期，也不自动重试。明确识别到限流的响应仍优先映射为 `429 UPSTREAM_RATE_LIMITED`，由批量任务按冷却策略处理。服务端错误日志仅记录操作名、HTTP 状态、受限长度的数字错误码、响应类型与冷却元数据，不记录响应正文或凭据。

**安全约定：**

- `/api/v1/*` 使用独立 Bearer Token；以下旧版管理接口除登录/会话查询外需要管理员会话
- 旧版会话接口的非 GET/HEAD/OPTIONS 请求必须携带 `X-CSRF-Token`；Token API 不使用 Cookie/CSRF
- 会话 Cookie：`hme_session`，`Path=/`、`HttpOnly`、`SameSite=Strict`；TLS 部署时设置 `ICLOUD_HME_SECURE_COOKIE=true` 启用 `Secure`
- 任何账号响应**绝不包含** `cookies`、`app_password`、`proxy` 字段（代理只暴露 `has_proxy` 布尔值）
- 用户可见错误消息不拼接上游响应体或秘密

---

## 自动注册任务（管理员会话接口）

- `POST /api/alias-batch`：启动任务，需管理员会话及 `X-CSRF-Token`。
  单通道 JSON：`account_id`、`count`（1–750 的整数）、`interval_seconds`（1–86400 秒，支持小数）、`cooldown_minutes`（可选，1–1440 的整数，省略默认 60）、`label_prefix`（可选，最多 170 字符）。
  单通道校验已有别名后的剩余额度，数量超额返回 `400 ALIAS_QUOTA_EXCEEDED`，已有 750 个返回 `409 ALIAS_LIMIT_REACHED`。
  `channel:"auto"` 固定持续运行：`continuous:true,count:0`，数量与间隔设置由服务端规范化；满额可启动等待容量。
  同账号有运行中、冷却中或停止中任务返回 `409 BATCH_ALREADY_RUNNING`。成功返回 `202` 和任务状态。
- `GET /api/alias-batch?account_id=ACCOUNT_ID`：查询该账号最近任务，无任务时 `data: null`；未知账号返回 404。
- `POST /api/alias-batch/stop`：停止任务，JSON：`account_id`、`job_id`（任务状态的 `id`），需会话和 CSRF。
  任务被替换或不存在返回 `404 BATCH_NOT_FOUND`。已结束的同一个任务可重复停止。

状态字段包含 `id`、`account_id`、`status`、`count`、`created`、`interval_seconds`、`cooldown_minutes`、`batch_size`（5）、`label_prefix`、
`alias_total`、`max_aliases`（750）、`last_email`、`started_at`，以及可选 `next_run_at`、`finished_at`、`error_code`、`message`。
`cooldown_count` 为累计冷却次数；冷却中 `next_run_at` 为恢复时间，`finished_at` 为空。
`status` 为 `running`、`cooling_down`、`stopping`、`completed`、`stopped`、`failed` 或 `limit_reached`。

以下为单通道模式（双通道 auto 见文末 19+4 策略）。任务顺序执行，第一项立即开始，每项完成后间隔指定秒数再执行下一项。每成功创建 5 个且仍有剩余任务时主动冷却，等待 `cooldown_minutes` 分钟后继续（同时满足 `interval_seconds` 最小间隔）；冷却期间任务不创建新别名。最后一项完成即结束，不额外冷却。
关闭网页不影响任务；停止不取消已发送的上游请求，正在执行的一项可能完成。临时上游错误进入冷却，首次使用设定的冷却时长、连续失败逐次加倍至 120 分钟（设定超过 120 分钟时不缩短），成功后重置；上游 `Retry-After` 更长时遵守更长等待时间。识别 HTTP 429 和 Apple `-41015` 为 `UPSTREAM_RATE_LIMITED`。恢复前按同一任务唯一标签核对别名列表；已存在则计入进度，不重复创建。错误不消耗目标数量；认证、配置错误及额度耗尽仍结束任务。冷却可停止且同账号不可重复启动。
单通道任务仅保存在内存中，重启容器后不自动恢复；双通道 auto 使用持久化小时计划。此任务接口不向 Token API 开放。
手动 `POST /api/create` 始终创建新别名；Token `POST /api/v1/aliases` 优先分配已有未占用别名，耗尽后才创建。新建时均检查每账号 750 个总数上限（含已停用别名），并与任务串行创建。

---

## 程序调用 API（新增）

基础地址：`http://HOST:8081/api/v1`，复用现有服务，无需额外端口。

### Token 配置

顶部导航 **API 设置**（`/api-settings`）可查看并复制接口地址、显示/复制当前 Token、生成或自定义新 Token，以及启用/停用程序 API。
更换 Token 或停用服务前会弹出确认框；旧 Token 在更换后立即失效，新发起的请求需使用新密钥。
生成 Token 不会自动启用已停用的 API。界面操作仅供已登录管理员，所有修改和密钥读取请求均校验 CSRF。

配置保存到数据目录 `api-settings.json`（权限 0600，原子写入）；该文件在 Docker 的数据卷中持久化，重启仍有效。
文件存在时优先于环境变量；`ICLOUD_HME_API_TOKEN` 仅在首次没有设置文件时作为初始 Token。
初始环境变量留空则程序 API 关闭，返回 `503 API_DISABLED`。建议通过页面生成随机密钥；
自定义密钥须为 32–512 个无空格的可见 ASCII 字符。现有 `.env` 密钥在首次打开设置页时仍保持有效。

每次请求携带 `Authorization: Bearer TOKEN`；无需网页登录、Cookie 或 CSRF。
缺失或错误密钥返回 `401 API_TOKEN_INVALID`。Token 仅用于下表接口，具备所有已配置账号的
别名分配、导入占用记录和邮件读取权限；不开放账号设置、删除邮件或删除别名权限。
请保管好密钥，仅通过请求头发送；跨主机调用请使用 HTTPS，不要将密钥写进 URL 或公开代码。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/accounts` | 获取账号摘要和 `id`（不返回凭据） |
| GET | `/aliases?account_id=ACCOUNT_ID` | 获取别名列表 |
| POST | `/aliases` 或 `/aliases/allocate` | 优先已有未占用别名，无可用时按需创建；参数见下表 |
| GET | `/aliases/usage` | 查询全部调用商占用统计；可加 `?caller_id=CALLER_ID` 查询单个调用商，不分配或创建 |
| POST | `/aliases/usage` | 导入指定调用商的历史记录；JSON：`{caller_id, entries: [{account_id, email, claimed_at}]}` |
| GET | `/inbox?account_id=ACCOUNT_ID&alias=ALIAS` | 读取邮件摘要；省略 alias 则读取该账号收件箱 |
| GET | `/inbox/MESSAGE_ID?account_id=ACCOUNT_ID` | 读取邮件正文（当前需已配置 IMAP/App 专用密码，ID 使用 IMAP 返回值） |

### 别名统一分配

`POST /api/v1/aliases` 已从“始终新建”改为“先分配已有别名，再按需新建”；`/aliases/allocate` 是同一逻辑的显式入口。网页手动创建、自动批量创建和读取别名列表保持原行为。

| JSON 参数 | 默认值 / 说明 |
| --- | --- |
| `account_id` | 可选；留空检查所有账号的已有别名，再考虑创建 |
| `caller_id` | 可选，默认 `default`；调用商固定标识，1–64 个字母/数字/`-_.`，去除首尾空格并转小写；每个调用商独立去重 |
| `auto_create` | `true`；关闭后池耗尽返回 `409 NO_AVAILABLE_ALIAS` |
| `max_aliases` | `750`；范围 1–750，含已停用；达上限仍可取用已有未占用别名 |
| `label` | 可选，新建标签，最多 200 字符；不修改已有别名的标签 |
| `label_prefix` | 默认 `api`；未指定 label 时生成新建标签，最多 150 字符 |
| `request_id` | 可选，最多 128 个字母/数字/`-_.`；同调用商、同 ID、同参数重试返回原结果；不同调用商可使用同 ID |
| `exclude_emails` | 可选，最多 10000 个地址；永久导入当前调用商的占用记录，不影响其他调用商 |

响应包含 `caller_id`、`account_id`、`email`、`source`（`existing` / `created`）、`allocated_at`（Unix 秒）、`request_id`。`source` 表示已有/新建，不表示调用商。服务按 **caller_id + 邮箱** 判定是否使用：A 用过的邮箱仍可分配给 B；同一调用商优先取用自己尚未用过的已有别名，耗尽后才新建。不使用主邮箱或已停用别名。分配在返回地址前持久化到 `data/alias-usage.json`（0600，原子写入）；同调用商并发/重启/更换 Token 后不重复分配，注册失败也保留占用。

**兼容旧客户端：** 不传 caller_id、空字符串和 `default` 均归默认分组；原 v1 记录全部归默认分组，保留已用地址和原 request_id 重试结果。命名调用商可独立使用这些邮箱。记录在后续写入时升级为 v2；不要将 v2 记录直接交给旧版本程序读取。此版本使用单服务进程管理同一数据目录。

**调用商不是权限隔离：** caller_id 由请求方提供，不会根据 IP、Token 或 User-Agent 自动识别。每个程序需固定发送自己的 caller_id；共用 Token 的程序仍有所有账号的别名和邮件读取权限。同一邮箱用于不同调用商时，收件可能混合多个调用商的邮件，调用程序需自行区分对应业务邮件。

历史数据可通过 `POST /aliases/usage` 批量迁移，每次最多 10000 条；顶层 caller_id 可选（默认 default），email 必填，account_id 可选，claimed_at 可选 Unix 秒。同调用商、同邮箱重复导入不覆盖已有结果。响应的 `used_count` 是全部调用商记录数，`caller_used_count` 是当前调用商记录数，包含导入记录。

`GET /aliases/usage` 返回全部记录数 `used_count`、不同邮箱数 `unique_email_count` 和按标识排序的 `callers: [{caller_id, used_count}]`；同邮箱被两家调用商使用计 2 条记录、1 个不同邮箱。加 `?caller_id=vendor-a` 后仅统计该调用商。API 设置页显示同样的统计，点击“刷新调用商统计”更新。

请求超时后，保留相同 `request_id` 与参数再查询/重试；不要换新 ID 盲目重发。幂等保障针对已持久化的成功分配；上游创建结果不明确时先查询别名列表核实。

邮件列表参数：`limit` 默认 20，范围 1–100；`days` 默认 7，范围 1–90。
IMAP 与 Web API 回退均默认合并 `INBOX` 与 `Junk`（IMAP 中不存在 Junk 时只读取 INBOX），按日期倒序排列，`limit` 是合并后的总上限。
邮件保留在原文件夹，列表和详情使用只读/PEEK，不移动邮件、不改变未读状态。
每封邮件返回 `folder` 字段（`INBOX` 或 `Junk`）。IMAP 收件箱 `id` 保持数字字符串，
垃圾邮件 `id` 为 `Junk:UID`（例如 `Junk:3`）；请将 ID 当作不透明字符串，详情和删除 URL 中使用 URL 编码，切勿自行转换为整数。
IMAP 连接/认证失败时回退 Web API；连接成功后的文件夹读取错误直接报错，避免悄悄漏掉垃圾邮件。
Web API 回退同样读取两个文件夹，先按 alias/days 筛选再合并排序、应用总 limit；任一文件夹读取失败则报错，不返回缺少 Junk 的部分结果。
Web API 的别名匹配仍是有限批次的本地筛选（摘要可用字段匹配，不保证精确收件人搜索）；精确按收件人查找请配置 IMAP。
Web API ID 为线程 ID（Junk 加 `Junk:` 前缀），不是 IMAP UID；详情接口仍仅支持 IMAP 返回的 ID。
响应 `data.method` 标明 `imap` 或 `web_api`。空收件箱返回 `messages: []`。
本接口通过查询已有收件箱收信，不是推送服务；需要持续收件时建议每 10–30 秒轮询，
按 `account_id + message.id` 去重。避免高频创建别名；分配请求使用稳定的 request_id，
上游创建请求结果不明确时先查询别名列表核实。

### curl 示例

```bash
BASE_URL='http://127.0.0.1:8081'
TOKEN='TOKEN' # 替换为服务端配置的密钥
ACCOUNT_ID='ACCOUNT_ID' # 从下面账号接口取得
CALLER_ID='vendor-a' # 每个调用商使用固定且不同的标识，如 vendor-a、vendor-b

curl -fsS "$BASE_URL/api/v1/accounts" -H "Authorization: Bearer $TOKEN"

curl -fsS "$BASE_URL/api/v1/aliases" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"account_id\":\"$ACCOUNT_ID\",\"caller_id\":\"$CALLER_ID\",\"label_prefix\":\"API\",\"request_id\":\"REQUEST_ID\"}"

curl -fsS -G "$BASE_URL/api/v1/aliases/usage" -H "Authorization: Bearer $TOKEN" \
  --data-urlencode "caller_id=$CALLER_ID"

# 将 ALIAS 替换为上一步返回的 data.email
ALIAS='ALIAS'
curl -fsS -G "$BASE_URL/api/v1/inbox" -H "Authorization: Bearer $TOKEN" \
  --data-urlencode "account_id=$ACCOUNT_ID" --data-urlencode "alias=$ALIAS" \
  --data-urlencode 'limit=20' --data-urlencode 'days=7'

# MESSAGE_ID 为 data.messages 中的 id（method=imap）
curl -fsS -G "$BASE_URL/api/v1/inbox/MESSAGE_ID" \
  -H "Authorization: Bearer $TOKEN" --data-urlencode "account_id=$ACCOUNT_ID"
```

分配成功：`{"success":true,"data":{"caller_id":"vendor-a","account_id":"ACCOUNT_ID","email":"ALIAS","source":"existing","allocated_at":1790000000,"request_id":"REQUEST_ID"}}`。
`created_at` 为可选字段，存在时由上游返回。邮件列表字段与下方原有收件箱接口一致。

---

## 认证端点

### 1. 登录

```http
POST /api/auth/login
Content-Type: application/json

{"username": "管理员账号", "password": "管理员密码"}
```

**成功响应：** 设置 `hme_session` Cookie

```json
{
  "success": true,
  "data": {
    "csrf_token": "随机值",
    "expires_at": "2026-08-05T22:00:00+08:00"
  }
}
```

**错误：**

- `401 INVALID_CREDENTIALS` — 账号或密码错误（不设置 Cookie）
- `429 RATE_LIMITED` — 同一 IP 15 分钟内失败超过 5 次，响应带 `Retry-After` 头

### 2. 查询会话

```http
GET /api/auth/session
Cookie: hme_session=...
```

**成功响应：** 同上（csrf_token / expires_at）。会话无效返回 `401 AUTH_REQUIRED`。

### 3. 退出

```http
POST /api/auth/logout
Cookie: hme_session=...
X-CSRF-Token: <token>

{"success": true, "data": {"logged_out": true}}
```

---

## 账号端点

### 4. 列出账号

```http
GET /api/accounts
```

**响应：** `Summary[]`，排序为 active → pending → error，同状态按 name、id。

```json
{
  "success": true,
  "data": [
    {
      "id": "acc_12345678",
      "name": "主号",
      "real_email": "owner@example.com",
      "icloud_email": "owner@icloud.com",
      "host": "icloud.com",
      "status": "active",
      "alias_total": 15,
      "alias_active": 12,
      "has_cookies": true,
      "has_app_password": true,
      "has_proxy": false,
      "last_validated": "2026-08-04T09:00:00+08:00",
      "status_message": "",
      "created_at": "2026-08-01T09:00:00+08:00"
    }
  ]
}
```

**禁止出现的字段：** `cookies`、`app_password`、`proxy`。`status_message` 只映射固定文案（pending → "等待配置或验证凭据"，error → "凭据验证失败"），不返回内部错误原文。

### 5. 添加账号

```http
POST /api/accounts
X-CSRF-Token: <token>

{
  "name": "新账号",
  "icloud_email": "owner@icloud.com",
  "host": "icloud.com",
  "proxy": "http://user:pass@host:port",
  "cookies": "X-APPLE-WEBAUTH-TOKEN=abc; X-APPLE-WEBAUTH-USER=def"
}
```

- `name` 必填，去空白后 1–64 字符
- `icloud_email` 必填，`net/mail` 校验且地址值必须等于输入
- `host` 只能是 `icloud.com` 或 `icloud.com.cn`（默认 `icloud.com`）
- `proxy` 可选，必须是 `http`/`https`/`socks5` URL
- `cookies` 可选，支持 Cookie Header 字符串或 JSON 文本
- 无 Cookie 时状态为 `pending`，不访问网络

**成功响应：** `201`，返回 `Summary`。

### 6. 编辑账号基本信息

```http
PATCH /api/accounts/:id
X-CSRF-Token: <token>

{"name": "新名称", "host": "icloud.com.cn"}
```

只接受可选的 `name`、`icloud_email`、`host`，至少一个字段存在。响应返回更新后的 `Summary`。账号不存在返回 `404 ACCOUNT_NOT_FOUND`。

### 7. 更新代理

```http
PUT /api/accounts/:id/proxy
X-CSRF-Token: <token>

{"proxy": "http://user:pass@host:port"}
```

空字符串表示清除代理。响应只返回更新后的 `Summary`（代理值从不回显）。

### 8. 更新 Cookie

```http
PUT /api/accounts/:id/cookies
X-CSRF-Token: <token>

{"cookies": "a=1; b=2"}
```

`cookies` 同时兼容字符串与对象：

```json
{"cookies": {"a": "1", "b": "2"}}
```

两种输入最终都交给 `account.ParseCookieInput`。响应只返回更新后的 `Summary`。

### 9. 设置 App 专用密码

```http
POST /api/accounts/:id/password
X-CSRF-Token: <token>

{"icloud_email": "your_email@icloud.com", "app_password": "xxxx-xxxx-xxxx-xxxx"}
```

服务端会用 IMAP 连接验证凭据。成功返回 `Summary`；IMAP 验证失败返回 `502 UPSTREAM_FAILURE`。

### 10. iCloud 密码登录（获取 Cookie）

```http
POST /api/accounts/:id/login
X-CSRF-Token: <token>

{"password": "用户的常规iCloud密码", "otp_code": "123456"}
```

- `otp_code` 可选，启用 2FA 时使用
- 需要 OTP：`409 OTP_REQUIRED`
- 验证码错误：`401 OTP_INVALID`
- 成功：**只返回 `Summary`，绝不返回 Cookies**（Cookie 自动持久化到账号配置）

### 11. 删除账号

```http
DELETE /api/accounts/:id
X-CSRF-Token: <token>
```

**响应：** `{"id": "acc_3"}`。不存在返回 `404 ACCOUNT_NOT_FOUND`。

---

## 业务端点

### 12. 创建 HME 别名

```http
POST /api/create
X-CSRF-Token: <token>

{"account_id": "acc_1", "label": "注册某网站"}
```

- `account_id` 必填
- `label` 可选，最长 200 字符

**响应：**

```json
{
  "success": true,
  "data": {
    "email": "xyz123@icloud.com",
    "label": "注册某网站",
    "created_at": "2026-01-15T10:30:00+08:00",
    "account_id": "acc_1"
  }
}
```

### 13. 读取邮件

```http
GET /api/inbox?account_id=acc_1&alias=xyz123@icloud.com&limit=20&days=7
```

- `account_id` 必填
- `alias` 可选，只返回发给该别名的邮件
- `limit` 1–100（默认 20）
- `days` 1–90（默认 7）；非法整数直接 `400 VALIDATION_ERROR`

**响应（IMAP 优先，Web API 回退）：**

```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "alias": "xyz123@icloud.com",
    "count": 2,
    "method": "imap",
    "messages": [
      {
        "id": "1042",
        "folder": "INBOX",
        "from": "GitHub <noreply@github.com>",
        "to": "xyz123@icloud.com",
        "subject": "[GitHub] Please verify your email address",
        "date": "2026-07-09T14:32:10+08:00",
        "preview": "Almost done! To finish setting up your account..."
      }
    ]
  }
}
```

`method` 为 `imap` 或 `web_api`。两条路径均查询 `INBOX` 和 `Junk`，按日期倒序合并并应用总 limit；IMAP 支持按收件人筛选，Web API 在有限批次内本地筛选 alias/days。`folder` 和带文件夹的 `id` 规则见上方程序调用 API。

### 14. 列出别名

```http
GET /api/aliases?account_id=acc_1
```

**响应：** alias 对象字段风格为 camelCase（兼容 iCloud 原始格式）：

```json
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "count": 15,
    "aliases": [
      {
        "email": "xyz123@icloud.com",
        "anonymousId": "abc123",
        "label": "注册某网站",
        "active": true,
        "createdAt": "2026-01-15T10:30:00Z"
      }
    ]
  }
}
```

### 15. 停用/激活/删除别名

```http
POST /api/aliases/:id/deactivate
POST /api/aliases/:id/reactivate
DELETE /api/aliases/:id
X-CSRF-Token: <token>

{"account_id": "acc_1"}
```

- `:id` 为别名的 `anonymousId`，非空且 URL 解码后不超过 256 字符
- `account_id` 必填
- 删除不可恢复；直接删除失败时会先停用再删

### 16. 重新加载配置

```http
POST /api/reload
X-CSRF-Token: <token>
```

重新读取 `accounts.json`。

---

## curl 使用示例（Cookie Jar + CSRF）

```bash
BASE="http://localhost:8081"

# 1. 登录,保存 Cookie 到 jar
curl -c cookies.txt -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"你的管理员账号","password":"你的管理员密码"}'

# 2. 从响应中提取 csrf_token(可用 jq)
CSRF=$(curl -b cookies.txt "$BASE/api/auth/session" | jq -r '.data.csrf_token')

# 3. 读取账号列表(GET 无需 CSRF)
curl -b cookies.txt "$BASE/api/accounts"

# 4. 添加账号(mutation 需要 CSRF 头)
curl -b cookies.txt -X POST "$BASE/api/accounts" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"name":"新账号","icloud_email":"owner@icloud.com"}'

# 5. 创建别名
curl -b cookies.txt -X POST "$BASE/api/create" \
  -H "Content-Type: application/json" \
  -H "X-CSRF-Token: $CSRF" \
  -d '{"account_id":"acc_1","label":"GitHub"}'

# 6. 读取邮件
curl -b cookies.txt "$BASE/api/inbox?account_id=acc_1&limit=10"
```

---

## 认证方式（iCloud 账号侧）

### Cookie 认证（功能最完整）

用于创建/停用/激活/删除别名、读取邮件（Web API 回退）。

**获取方式：**
1. 浏览器登录 [icloud.com](https://www.icloud.com) 或 [icloud.com.cn](https://www.icloud.com.cn) (国区)
2. F12 → Application → Cookies
3. 导出 Cookie 为 `{"key":"value"}` JSON，粘贴到管理界面「更新 Cookie」

**关键 Cookie：** `X-APPLE-WEBAUTH-TOKEN`（认证 token）、`X-APPLE-WEBAUTH-USER`（含 dsid）、`X-APPLE-WEBAUTH-HSA-TRUST`（设备信任）、`X-APPLE-DS-WEB-SESSION-TOKEN`（会话）

**有效期：** 约 24 小时

### App Password 认证（IMAP 优先读邮件）

用于 IMAP 读取邮件（优先路径，支持服务端按收件人搜索）。在 [appleid.apple.com](https://appleid.apple.com) → 登录和安全 → App 专用密码 生成。

---

## 技术说明

**Web API 路径** (`internal/mail/web_client.go`)：
1. 调用 `setup.icloud.com.cn/setup/ws/1/validate` 获取 `mccgateway` URL
2. 调用 `mccgateway/mailws2/v1/thread/search` 读取邮件

**⚠️ 已知坑：**
- `validate` 返回的 mccgateway URL 可能带 `:443` 端口，tls-client 的 cookie jar 按不带端口的 host 存储 cookie，带端口请求时 cookie 无法附加导致 403；**解决：** 解析 URL 后剥离端口号

**IMAP 路径** (`internal/mail/client.go`)：标准 IMAP 协议，连接 `imap.mail.me.com:993`，需要 App Password。

**升级差异（相对旧版）：**
- 全部 API 需要管理员登录（`401 AUTH_REQUIRED`）
- 账号响应不再返回 Cookie/密码/代理原文，改用 `has_cookies`/`has_app_password`/`has_proxy`
- `POST /api/accounts/:id/login` 成功响应不再返回 `cookies` 字段
- `accounts.json` 使用 `{"accounts": {id: {...}}}` map wrapper 格式（参考 `accounts.json.template`）

## 限制

- **创建频率**：iCloud 限制别名创建频率，过快会返回 429（服务端自动重试最多 5 次）
- **Cookie 有效期**：约 24 小时，需定期更新
- **邮件读取**：依赖 IMAP 连接，超时默认 30 秒
- **请求体上限**：1 MiB

## 自动创建的新旧通道

`POST /api/alias-batch` 可选 `channel`：

- `icloud_web`：默认旧通道，保留原有间隔和每批 5 个的设置。
- `apple_account`：单独新通道，保留上一版自定义间隔/批次设置；需独立登录。
- `auto`：采用 icloud-mail 提交 `886eb52` 的自动计划策略，不再一新一旧轮流创建。

```json
{"account_id":"ACCOUNT_ID","continuous":true,"label_prefix":"自动注册","channel":"auto"}
```

`auto` 固定策略（客户端传入的 `interval_seconds` / `cooldown_minutes` 被规范为 120 / 60，不是可调节的加速参数）：

- 所有新启动的 auto 任务固定 `continuous:true,count:0`，忽略旧客户端传入的 count。取消目标数量，每小时持续循环，手动停止前不因累计数量结束。单通道不支持 `continuous:true`。
- 一小时 23 个随机分布时隙，间隔至少 2 分钟，最后一个时隙位于计划起点后一小时；首次也等待计划时间。
- 新通道 19 个，旧通道 4 个。旧通道为第 6、12、18、23 个时隙。
- 新通道未登录或会话被拒绝时，降级为旧通道每小时 5 个分散时隙（第 5、10、14、19、23 个）。新登录恢复后自动切回 19+4。
- 没有有效旧通道会话时保留任务，跳过时隙，不发起创建；旧通道恢复后继续。
- 每通道滚动一小时的尝试预算为新 19 / 旧 4；仅旧通道模式为 5。共享预算为滚动一小时 23 次、24 小时 552 次。失败的远端尝试同样占用预算。
- 单通道限流至少冷却一小时（更长 Retry-After 优先）；冷却时跳过该通道时隙，不转给另一通道。共享目录请求限流则整体推迟。
- 逾期超过 2 分钟的计划重新生成，不集中补发。慢请求不会压缩后续最小间隔。
- 这些数字是参考项目的本地调度上限，不是 Apple 公布或保证的配额。

状态额外返回 `continuous:true`、`count:0`、累计 `created`（可以超过 750，账号当前总数仍受限）及 `policy: "upstream_19_4"`、`schedule_mode: "dual" | "web_only" | "offline"`、`schedule_slot`（下一时隙，1–23），以及每通道计数与冷却状态 `channels`。

计划、进度、尝试账本和冷却原子写入 `data/dual-schedules.json`（0600）。重启恢复尚未完成的双通道任务；手动停止不会恢复；重新启动任务也不会清空已用预算。重启前有未确认的在途创建时暂停为 `ALIAS_CONFIRMATION_REQUIRED`，核对后再启动，不自动重复创建。持久化失败时停止，未提交账本前不会发起新的创建。

持续双通道保留 750 个当前总数检查；容量已满时保持 `cooling_down` 任务，每小时只核对一次，释放容量后恢复，不自动删除别名。持续运行不绕过限流或账号要求；创建结果不确定、持久化失败等情况仍暂停核对。旧文件中不带 continuous 的有限数量任务仍按原 count 恢复，不擅自扩大原授权范围；新启动 auto 才切为持续模式。没有迁移参考项目的 PostgreSQL 或邮箱池。单通道任务仍不跨重启恢复。手动 `/api/create` 和服务 API 的原有旧通道行为不变，与双通道自动计划账本分开。

`POST /api/accounts/:id/apple-login`：管理员登录态 + CSRF，首次 `{"password":"PASSWORD"}`；若返回 `OTP_REQUIRED`，再提交 `{"otp_code":"123456"}`。待验证会话仅存内存，5 分钟过期；密码不落盘。新通道会话与旧 Cookie 独立持久化，自动续期；公开摘要仅返回 `has_apple_session` 等非秘密信息。

## AppleServices 账号导入

`POST /api/integrations/appleservices/accounts`

仅接受 `Content-Type: application/json` 与
`Authorization: Bearer <ICLOUD_HME_IMPORT_TOKEN>`。密钥 32–512 字符且无空白；
空配置关闭接口。密钥比较为恒定时间；不接受管理会话或 `ICLOUD_HME_API_TOKEN`。
仅通过 HTTPS 代理访问，客户端不跟随跳转、不自动重试。

```json
{
  "version": 1,
  "source_id": "appleservices-<64位小写SHA256>",
  "apple_id": "owner@example.test",
  "name": "示例账号",
  "icloud_email": "owner@icloud.com",
  "host": "icloud.com",
  "cookies": {
    "X-APPLE-WEBAUTH-TOKEN": "synthetic-token",
    "X-APPLE-WEBAUTH-USER": "synthetic-user"
  },
  "app_password": "abcd-efgh-ijkl-mnop"
}
```

`source_id = "appleservices-" + hex(SHA256("appleservices-hme-v1\x00" +
lower(trim(apple_id))))`，其中 `\x00` 是一个 NUL 字节；服务端重算。
`host` 仅接受 `icloud.com`、`icloud.com.cn`，不接受代理、任意 URL、IMAP host
或文件字段。最大 256 KiB，拒绝未知字段、重复键、尾随 JSON、URL 查询参数及
压缩请求体。邮箱和 Apple ID 去首尾空白并小写，名称最长 64 字符；Apple ID 不限于邮箱形式。
应用专用密码原样交给 IMAP 核验（非空、最多 512 字节、无控制字符），不猜测格式。
Cookie 是名称到字符串的非空对象；身份由 Apple 响应证明，不凭某个 Cookie 名判断。

验证次序：真实 Apple validate 响应身份 → 完整 HME 列表及已验证转发候选中的
非 HME iCloud 主邮箱 → 固定 iCloud IMAP TLS 认证。IMAP 仅 LOGIN，随后断开；
不读取/更改邮件。所有检查和旧账号身份核验均在全局账号锁外完成。

全部成功且持久化完成才返回新增 **201**、更新 **200**：

```json
{"success":true,"data":{"id":"acc_fixture","source_id":"appleservices-<请求相同值>","status":"active","hme_verified":true,"imap_verified":true}}
```

账号 ID 仅字母、数字、下划线、短横线，最多 128 字符。
源映射及验证身份写入原 `accounts.json`（向后兼容）；同源幂等，保留原 ID 和
用户设置、Apple Account 会话、使用记录、任务。旧账号匹配必须以旧 Cookie
的服务端验证身份及匹配收件地址为证据，元数据不是身份凭证。多个匹配、同源
DSID 改变或无法证明旧记录身份时冲突，不猜测认领，不创建重复记录。
提交前比较账号集内容版本；其他请求在验证期间修改账号时返回 409，手动重试。
旧异步操作通过导入凭据代数检查，避免覆盖新凭据。保存失败回滚内存且保留旧文件。

失败格式：`{"success":false,"code":"静态错误码","message":"同一静态错误码"}`。
不包含 Apple 原始响应、Cookie、密码、密钥或内部异常；失败绝不保存候选账号。

| 状态 | 错误码 |
| --- | --- |
| 400 | `INVALID_IMPORT_REQUEST` |
| 401 | `IMPORT_UNAUTHORIZED` |
| 404 | `IMPORT_DISABLED` |
| 409 | `ACCOUNT_IDENTITY_CONFLICT` |
| 413 | `IMPORT_TOO_LARGE` |
| 422 | `APPLE_VERIFICATION_FAILED` / `HME_VERIFICATION_FAILED` / `IMAP_VERIFICATION_FAILED` |
| 429 | `IMPORT_RATE_LIMITED`（带 `Retry-After`） |
| 503 | `IMPORT_UNAVAILABLE` / `IMPORT_BUSY` / `IMPORT_STORAGE_FAILED` |

代理层可能以 400/403/413/425 拒绝不合规请求。每连接 IP 5 次失败/15 分钟；
最多 2 个在途请求；90 秒总验证期限；不会自动重试写入或 Apple 创建动作。
部署及密钥安全保存步骤见 README。首次真实联调需 Windows 面板执行同步验收。


## Windows 自动登录集成

账号导入后可调用 `POST /api/integrations/appleservices/apple-login`，使用专用导入 Bearer 密钥执行 status、login、verify。完整字段、状态和部署步骤见 [WINDOWS_LOGIN_INTEGRATION.md](WINDOWS_LOGIN_INTEGRATION.md)。该接口不使用服务 API token，也不更改现有浏览器认证。
