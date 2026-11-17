# iCloud Hide My Email 本地管理工具

[English](#english) | 中文

通过逆向 iCloud Web 接口和 IMAP 邮件协议，实现 Apple iCloud 隐藏邮箱别名的创建、列出和邮件收取功能。内置中文管理界面（React 单页应用，随二进制内嵌分发）。

## 功能特性

- ✅ **中文管理界面** — 浏览器访问 `http://localhost:8081` 即开即用
- ✅ **创建 HME 别名** — 自动生成 iCloud 隐藏邮箱地址
- ✅ **自动注册别名** — 设置本次数量与每个间隔秒数，后台依次注册、显示进度并支持停止；每账号别名总数限制为 750 个（含已停用别名）
- ✅ **列出所有别名** — 查看账号下的所有 HME 别名
- ✅ **收取邮件** — IMAP 合并收件箱与 Junk 垃圾邮件，显示来源并支持别名筛选；只读展示，不自动搬动邮件。Web API 回退同样合并 INBOX 与 Junk
- ✅ **双路径读信** — 邮件读取优先走 IMAP (App Password),无 App Password 时回退 Web API (Cookie)
- ✅ **多账号管理** — 支持多个 iCloud 账号并行管理
- ✅ **双认证模式** — Cookie (创建别名 + 读邮件回退) 和 App Password (IMAP 优先)
- ✅ **安全模型** — 单管理员会话、CSRF 校验、登录限流、响应脱敏

## 快速开始

### 自动注册别名

在「别名管理」选择账号，点击 **自动注册别名**，填写 **本次注册数量**（1–剩余额度）、
**每个间隔（秒）**（1–86400，可输入小数）和 **冷却时长（分钟）**（1–1440 的整数，默认 60），点击 **开始注册**。标签前缀可留空，系统自动追加任务编号和序号。
以下为单通道模式；双通道 auto 使用文末的 19+4 小时计划。单通道第一项立即执行，后续每项在上一项完成后等待指定秒数；默认本次 10 个、间隔 10 秒，不并发注册。每成功创建 5 个后主动进入冷却，期间不发起任务注册请求，到期自动继续剩余数量；最后一批完成即结束，不再冷却。批次间等待取冷却时长和每个间隔中的较大值。

任务在服务端运行，关闭或刷新网页仍会继续；再次打开页面可查看进度和停止。
每账号同时只运行一个任务，自动、手动和 Token API 创建均在创建前重新检查 750 个总数上限。
如剩余额度小于本次数量，启动时提示减少数量；遇到上游临时错误进入冷却：首次使用本任务设定的冷却时长，连续失败逐次加倍至 120 分钟（设定超过 120 分钟时不缩短，上游 `Retry-After` 更长时优先遵守）；成功后重置退避。冷却结束先查询别名列表，核对同一任务标签，避免响应丢失后重复创建，再继续剩余数量。
冷却期间显示恢复时间、已完成数量和冷却次数，可随时停止，同账号禁止重复启动。Cookie 失效、账号配置错误或额度耗尽仍停止并提示，已注册别名保留。
停止时不再发起新注册，已发送的一次请求可能仍完成。单通道任务不持久化，**容器重启后需手动重新开始**；双通道 auto 的计划和预算持久化并自动恢复。
不会自动启用任务或替用户创建测试别名。

### 1. 安装

#### 方式一：下载二进制发布版（推荐）

从 [GitHub Releases](https://github.com/xiaozhou26/icloud-hme/releases) 下载对应平台的二进制文件：

| 平台 | 文件 |
|---|---|
| Linux x86_64 | `icloud-hme_linux_amd64` |
| Linux ARM64 | `icloud-hme_linux_arm64` |
| macOS Intel | `icloud-hme_darwin_amd64` |
| macOS Apple Silicon | `icloud-hme_darwin_arm64` |
| Windows x86_64 | `icloud-hme_windows_amd64.exe` |

```bash
# 示例：Linux 下直接运行（必须先设置管理员密码）
export ICLOUD_HME_ADMIN_USERNAME='admin'
export ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026'
chmod +x icloud-hme_linux_amd64
./icloud-hme_linux_amd64
```

#### 方式二：Docker

```bash
# 拉取镜像
docker pull ghcr.io/xiaozhou26/icloud-hme:latest

# 运行（将本机 data 目录挂载进去）
docker run -d \
  --name icloud-hme \
  -p 8081:8081 \
  -v /path/to/data:/app/data \
  -e ICLOUD_HME_ADMIN_USERNAME='admin' \
  -e ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026' \
  ghcr.io/xiaozhou26/icloud-hme:latest
```

> ⚠️ 上面的密码仅为示例，**不可照抄**，请务必更换为至少 6 字符的强密码。

镜像支持 `linux/amd64` 和 `linux/arm64` 双架构，自动适配。

#### 方式三：源码编译（需要 Go 1.26+ 与 Node.js 22.12+ 双工具链）

```bash
# 前置要求: Go 1.26+、Node.js 22.12+
git clone https://github.com/xiaozhou26/icloud-hme.git
cd icloud-hme

# 一键构建（安装前端依赖 → 前端测试 → 前端构建 → Go 测试 → 编译）
./build.sh

# 或者手动分步构建
npm --prefix web ci
npm --prefix web run build
go build -o icloud-hme .
```

### 2. 安全配置（必读）

管理界面与 API 均需要管理员登录，升级后所有 API 都必须先通过 `POST /api/auth/login` 获取会话：

| 环境变量 | 说明 | 默认 |
|---|---|---|
| `ICLOUD_HME_ADMIN_USERNAME` | 管理员账号 | `admin` |
| `ICLOUD_HME_ADMIN_PASSWORD` | 管理员密码，**必填**，至少 6 字符 | 无（缺失时拒绝启动） |
| `ICLOUD_HME_SESSION_TTL` | 会话有效期 | `12h`（范围 `15m`–`168h`） |
| `ICLOUD_HME_SECURE_COOKIE` | 通过 TLS 反向代理部署时设为 `true` | `false` |

> **Breaking Change（v0.3+）**：升级后未设置 `ICLOUD_HME_ADMIN_PASSWORD` 将拒绝启动；
> 原有匿名 API 调用将收到 `401 AUTH_REQUIRED`。管理员会话只存内存，进程重启即失效。

### 3. 配置账号

在程序 `data/` 目录下创建 `accounts.json`（参考仓库内 `accounts.json.template`）：

```json
{
  "accounts": {
    "acc_1": {
      "id": "acc_1",
      "name": "主号",
      "real_email": "owner@example.com",
      "icloud_email": "owner@icloud.com",
      "cookies": {
        "X-APPLE-WEBAUTH-TOKEN": "token_value",
        "X-APPLE-WEBAUTH-USER": "v=1:s=1:d=22789132008"
      },
      "host": "icloud.com",
      "proxy": "http://user:pass@host:port",
      "app_password": "xxxx-xxxx-xxxx-xxxx",
      "status": "active"
    }
  }
}
```

> **提示:** 也可以通过管理界面的「账号」页面动态添加账号，无需手动编辑 JSON 文件。`cookies`、`app_password`、`proxy` 都是可选的。

### 4. 启动服务

```bash
# 二进制方式（默认 data 目录）
export ICLOUD_HME_ADMIN_USERNAME='admin'
export ICLOUD_HME_ADMIN_PASSWORD='your-strong-password'
./icloud-hme_linux_amd64

# 指定端口和数据目录
./icloud-hme_linux_amd64 -addr :9090 -data ./my_data

# 调试模式（启用请求日志）
./icloud-hme_linux_amd64 -debug

# 查看完整参数
./icloud-hme_linux_amd64 -h
```

服务默认监听 `:8081`。浏览器打开 `http://localhost:8081` 进入管理界面（账号 / 别名 / 收件箱）。完整 API 契约见 [API.md](API.md)。

## API 接口

> **认证**：除 `POST /api/auth/login` 与 `GET /api/auth/session` 外，所有 `/api` 接口都需要管理员会话 Cookie（`hme_session`）；非 GET/HEAD/OPTIONS 请求还需携带 `X-CSRF-Token` 请求头。完整契约与 curl 示例见 [API.md](API.md)。

### 核心接口

#### 创建 HME 别名

```bash
POST /api/create

# 请求体
{
  "account_id": "acc_1",      # 必填: 账号 ID
  "label": "注册某网站"        # 可选: 别名标签
}

# 响应
{
  "success": true,
  "data": {
    "email": "xyz123@icloud.com",
    "label": "注册某网站",
    "created_at": "2024-01-15T10:30:00Z",
    "account_id": "acc_1"
  }
}
```

#### 读取邮件

```bash
GET /api/inbox?account_id=acc_1&alias=xyz123@icloud.com&limit=20&days=7

# 参数说明:
#   account_id - 必填: 账号 ID
#   alias      - 可选: 只读取发到该别名的邮件
#   limit      - 可选: 返回邮件数量 (默认 20)
#   days       - 可选: 查找最近几天的邮件 (默认 7,仅 IMAP 模式)

# 响应
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
        "from": "noreply@example.com",
        "to": "xyz123@icloud.com",
        "subject": "欢迎注册",
        "preview": "感谢您的注册...",
        "date": "2026-07-09T14:32:10+08:00"
      }
    ]
  }
}

# 读取方式 (自动选择):
#   method: "imap"    — 通过 App Password 认证 (优先)
#   method: "web_api" — 通过 Cookie 认证,无需 App Password (回退)
```

### 账号管理接口

#### 列出所有账号

```bash
GET /api/accounts

# 响应
{
  "success": true,
  "data": [
    {"id": "acc_1", "name": "主号"},
    {"id": "acc_2", "name": "副号"}
  ]
}
```

#### 添加账号

**简化版（cookies 可选）:**

```bash
POST /api/accounts

# 请求体
{
  "name": "新账号",
  "host": "icloud.com",           # 可选
  "proxy": "http://..."           # 可选
}

# 响应 - 状态为 pending,需登录
{
  "success": true,
  "data": {
    "id": "acc_xxx",
    "name": "新账号",
    "status": "pending"
  }
}
```

**完整版（带 Cookie）:**

```bash
POST /api/accounts

# 请求体
{
  "name": "新账号",
  "cookies": "{\"x-apple-session-token\":\"token_value\"}",  # JSON 或 Header 格式
  "host": "icloud.com",           # 可选
  "proxy": "http://..."           # 可选
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_3",
    "name": "新账号",
    "status": "active"
  }
}
```

#### 账号登录（获取 Cookie）

```bash
POST /api/accounts/:id/login

# 请求体
{
  "password": "用户的常规iCloud密码",  # 不是 App Password
  "otp_code": "123456"                  # 可选,2FA 验证码
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_1",
    "cookies": {
      "x-apple-session-token": "...",
      "X-APPLE-WEBAUTH-TOKEN": "..."
    }
  }
}
```

#### 删除账号

```bash
DELETE /api/accounts/:id

# 响应
{
  "success": true,
  "data": {"id": "acc_3"}
}
```

#### 设置 App Password

```bash
POST /api/accounts/:id/password

# 请求体
{
  "icloud_email": "your_email@icloud.com",
  "app_password": "xxxx-xxxx-xxxx-xxxx"
}

# 响应
{
  "success": true,
  "data": {
    "id": "acc_1",
    "icloud_email": "your_email@icloud.com"
  }
}
```

### 别名管理接口

#### 列出所有别名

```bash
GET /api/aliases?account_id=acc_1

# 响应
{
  "success": true,
  "data": {
    "account_id": "acc_1",
    "count": 15,
    "aliases": [
      {
        "email": "xyz123@icloud.com",
        "label": "注册某网站",
        "created_at": "2024-01-15T10:30:00Z"
      }
    ]
  }
}
```

#### 停用别名

```bash
POST /api/aliases/:id/deactivate

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

#### 激活别名

```bash
POST /api/aliases/:id/reactivate

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123",
    "success": true
  }
}
```

#### 删除别名

```bash
DELETE /api/aliases/:id

# 请求体
{
  "account_id": "acc_1"
}

# 响应
{
  "success": true,
  "data": {
    "anonymous_id": "abc123"
  }
}
```

## 认证方式

### 方式一: Cookie 认证 (推荐,功能最完整)

Cookie 认证可实现所有功能:创建别名、读取邮件、管理别名。

**适用范围:**
- 创建/停用/激活/删除 HME 别名 ✅
- 读取邮件 (通过 iCloud Web API,无需 App Password) ✅

**获取 Cookie:**

1. 使用浏览器登录 [icloud.com](https://www.icloud.com) 或 [icloud.com.cn](https://www.icloud.com.cn) (国区)
2. 打开浏览器开发者工具 (F12)
3. 进入 Application → Cookies
4. 导出全部 Cookie 为 `{"key":"value"}` 格式的 JSON

**关键 Cookie (必需):**
- `X-APPLE-WEBAUTH-TOKEN` — 认证 token
- `X-APPLE-WEBAUTH-USER` — 含 dsid (`v=1:s=1:d=22789132008`)
- `X-APPLE-WEBAUTH-HSA-TRUST` — 设备信任 token
- `X-APPLE-DS-WEB-SESSION-TOKEN` — 会话 token

**注意:** 导出的 Cookie 值不要包含多余的引号或转义字符。

### 方式二: App Password 认证 (IMAP,优先读邮件)

App Password 用于 IMAP 读取邮件,是邮件读取的优先路径 (支持服务端按收件人搜索)。

**生成 App Password:**

1. 登录 [appleid.apple.com](https://appleid.apple.com)
2. 进入 "登录和安全" → "App 专用密码"
3. 生成新密码,用于此工具

### 邮件读取双路径

`GET /api/inbox` 自动选择读取方式:

1. **优先: IMAP (App Password)** — 设置了 App Password 时使用,支持服务端按收件人 (`TO`) 搜索
2. **回退: Web API (Cookie)** — 无 App Password 或 IMAP 失败时,通过 `mccgateway` 端点读取,本地按别名过滤

响应中包含 `"method": "web_api"` 或 `"method": "imap"` 字段,标识实际使用的读取方式。

## 项目架构

```
icloud-hme/
├── main.go                 # 入口: 读取安全配置、加载账号、启动服务
├── web/                    # 前端工程 (React + TypeScript + Vite)
│   └── src/                #   管理界面源码
├── accounts.json           # 账号配置文件 (自动生成)
├── go.mod
└── internal/
    ├── account/
    │   ├── manager.go      # 多账号管理器 (持久化、客户端工厂)
    │   └── public.go       # 公开 DTO (Summary) 与输入校验
    ├── auth/
    │   ├── manager.go      # 管理员会话 + CSRF
    │   └── limiter.go      # 登录失败限流
    ├── hme/
    │   ├── client.go       # iCloud HME Web 客户端 (Cookie 认证)
    │   └── auth.go         # SRP 登录 (账号密码 + 2FA 获取 Cookie)
    ├── mail/
    │   ├── client.go       # IMAP 邮件客户端 (App Password 认证)
    │   └── web_client.go   # Web 邮件客户端 (Cookie 认证,无需 App Password)
    ├── server/
    │   ├── server.go       # 路由分组 (认证 + CSRF)
    │   ├── backend.go      # 业务接口与 Manager 适配器
    │   ├── auth.go         # 登录/会话/退出 handler 与中间件
    │   ├── account_handlers.go  # 账号管理 handler
    │   └── middleware.go   # 安全响应头、请求上限
    └── webui/
        └── embed.go        # 内嵌前端资源 + SPA fallback
```

### 核心模块

- **account.Manager**: 管理多个 iCloud 账号,负责配置持久化和客户端创建
- **hme.Client**: 封装 iCloud HME Web API,支持 Cookie 认证
- **hme.auth**: SRP 协议登录,支持账号密码 + 可选 2FA
- **mail.Client**: IMAP 邮件客户端 (App Password,优先读邮件)
- **mail.WebClient**: 通过 iCloud Web API (mccgateway) 读取邮件,无需 App Password
- **server.Server**: HTTP API 服务 + 管理界面静态资源

## 技术栈

- **Go 1.26+** / **Gin** — HTTP 框架
- **React 19 + TypeScript + Vite 8** — 管理界面
- **go-imap** — IMAP 协议实现
- **tls-client** — TLS 指纹模拟 (绕过 iCloud 反爬)

## 常见问题

### Q: 创建别名返回 401/403 错误?

**A:** 区分两种情况：401 表示上游认证失败，可重新登录或更新 Cookie；普通 403 表示操作被拒绝，单凭这个状态不能判断 Cookie 过期。如果别名列表仍可读取，应对照 iCloud 官方页面能否创建地址，并查看服务端脱敏日志中的 `operation`、`code` 和 `response_kind`。空响应的 403 没有具体错误原因，不应猜成限流并反复重试。明确的限流响应仍按配置冷却。

### Q: 读取邮件返回超时?

**A:** 检查网络连接，确保可以访问 `imap.mail.me.com:993`。

### Q: 如何查看某个别名收到了哪些邮件?

**A:** 调用 `GET /api/inbox?account_id=acc_1&alias=your_alias@icloud.com`

### Q: 支持同时管理多个 iCloud 账号吗?

**A:** 支持，在 `accounts.json` 中配置多个账号即可，每个账号有独立的 `id`。

## 开发指南

### 本地开发

```bash
# 前端开发模式 (vite dev server, /api 代理到 :8081)
npm --prefix web ci
npm --prefix web run dev

# 后端开发模式
export ICLOUD_HME_ADMIN_USERNAME='admin'
export ICLOUD_HME_ADMIN_PASSWORD='your-strong-password'
go run main.go -debug

# 前端检查 (lint + test + build)
npm --prefix web run check

# 完整构建 (含前端)
./build.sh

# 交叉编译
GOOS=linux GOARCH=amd64 go build -o icloud-hme .
GOOS=windows GOARCH=amd64 go build -o icloud-hme.exe .
```

### 发布

推送 `v*` tag 到 GitHub 自动触发 CI：

```bash
git tag v0.2.0 && git push origin --tags
```

Actions 会自动构建多平台二进制、Docker 镜像（`ghcr.io/xiaozhou26/icloud-hme`）并创建 Release。

### 代码规范

- 代码注释使用中文
- 错误信息返回给用户时使用中文
- API 响应格式统一: `{success: bool, data: any, message: string}`

## 许可证

MIT License

---
## 社区

友情链接：[LINUX DO](https://linux.do)

## English

A local management tool for Apple iCloud Hide My Email (HME) aliases, supporting creation, listing, and email reading through reverse-engineered iCloud Web API and IMAP protocol. Ships with a built-in Chinese management UI (React SPA embedded in the single binary).

### Features

- Built-in management UI at `http://localhost:8081`
- Create HME aliases automatically
- List all aliases for an account
- Read emails sent to HME aliases via IMAP or Web API
- Manage multiple iCloud accounts
- Dual authentication: Cookie and App Password
- Security: single-admin session, CSRF checks, login rate limiting, redacted API responses

### Quick Start

#### Option 1: Binary (GitHub Releases)

Download the latest binary from [GitHub Releases](https://github.com/xiaozhou26/icloud-hme/releases):

| Platform | File |
|---|---|
| Linux x86_64 | `icloud-hme_linux_amd64` |
| Linux ARM64 | `icloud-hme_linux_arm64` |
| macOS Intel | `icloud-hme_darwin_amd64` |
| macOS Apple Silicon | `icloud-hme_darwin_arm64` |
| Windows x86_64 | `icloud-hme_windows_amd64.exe` |

```bash
# Linux example (admin password is REQUIRED, min 6 chars)
export ICLOUD_HME_ADMIN_USERNAME='admin'
export ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026'
chmod +x icloud-hme_linux_amd64
./icloud-hme_linux_amd64
```

#### Option 2: Docker

```bash
docker pull ghcr.io/xiaozhou26/icloud-hme:latest

docker run -d \
  --name icloud-hme \
  -p 8081:8081 \
  -v /path/to/data:/app/data \
  -e ICLOUD_HME_ADMIN_USERNAME='admin' \
  -e ICLOUD_HME_ADMIN_PASSWORD='change-this-before-running-2026' \
  ghcr.io/xiaozhou26/icloud-hme:latest
```

> The password above is only an example — do NOT copy it. Use a strong password with at least 6 characters.

#### Option 3: Build from source (Go 1.26+ and Node.js 22.12+)

```bash
git clone https://github.com/xiaozhou26/icloud-hme.git
cd icloud-hme

# One-shot build (frontend deps → frontend test → frontend build → Go test → binary)
./build.sh

# Or step by step
npm --prefix web ci
npm --prefix web run build
go build -o icloud-hme .
```

### Configuration

| Env var | Description | Default |
|---|---|---|
| `ICLOUD_HME_ADMIN_USERNAME` | Admin username | `admin` |
| `ICLOUD_HME_ADMIN_PASSWORD` | Admin password, **required**, min 6 chars | none (refuses to start) |
| `ICLOUD_HME_SESSION_TTL` | Session TTL | `12h` (range `15m`–`168h`) |
| `ICLOUD_HME_SECURE_COOKIE` | Set `true` when deployed behind TLS | `false` |

> **Breaking change (v0.3+)**: without `ICLOUD_HME_ADMIN_PASSWORD` the server refuses to start; all API endpoints now require login (`401 AUTH_REQUIRED`). Admin sessions are in-memory only and are lost on restart.

Create `data/accounts.json` (see `accounts.json.template`) and start the server (default port `:8081`). Open `http://localhost:8081` to use the management UI. Full API contract: [API.md](API.md).

## 程序调用 API

`/api/v1` Token API：按 `caller_id` 为不同调用商独立分配别名（优先该调用商未用过的已有别名，耗尽后才新建）、列出别名、按别名查询收件箱和读取邮件正文。相同邮箱可供不同调用商分别使用，同调用商并发和重启后不重复分配，失败也不回收。占用记录保存在 `data/alias-usage.json`；旧客户端和旧记录归默认分组 `default`。支持通过 `/aliases/usage` 查询各调用商统计、导入对应历史记录。`caller_id` 仅区分使用记录，不是权限隔离。
在 `.env` 设置 `ICLOUD_HME_API_TOKEN`（至少 32 字符的独立随机密钥），执行
`docker compose up -d --build` 后通过 `Authorization: Bearer TOKEN` 调用；无需浏览器会话或 CSRF。
未配置 Token 时程序 API 关闭。完整参数、curl 示例和收件轮询说明见 [API.md](API.md#程序调用-api新增)。

顶部导航现有 **API 设置**：管理 Token、开关服务、复制接口地址及调用示例。
页面还显示各调用商的已用邮箱数，可填写“调用商标识”生成带 `caller_id` 的示例；实际调用程序需发送各自固定的标识。
页面保存后，持久化的 `data/api-settings.json` 优先于初始 `.env` Token，配置立即生效。

### 合并垃圾邮件（IMAP）

邮件列表默认合并 `INBOX` 与 `Junk`，新旧隐私别名均适用，无需逐个设置信任规则。
来源会显示为“收件箱”或“垃圾邮件”；邮件留在原文件夹，不自动标记已读。
详情/删除操作使用完整 `id`：收件箱示例 `3`，垃圾邮件示例 `Junk:3`，避免同 UID 跨文件夹混淆。
接口调用方应保留字符串 ID，并对路径参数 URL 编码。IMAP 与 Web API 回退均合并两个文件夹并应用总 limit 和 days；Web API 的别名筛选仍限于有限批次摘要，详情接口仍需 IMAP ID。

### 自动创建：新旧双通道

1. 保留 iCloud Web 登录（用于旧通道及共享目录校验）。
2. 在「账号」页点击「登录新通道」，完成 Apple Account 密码和双重认证。
3. 在「别名 → 自动注册别名」选择「新旧双通道 · 每小时 19+4 持续运行」。

双通道采用参考项目的自动调度：一小时随机分布 23 个时隙，最小间隔 2 分钟；新通道 19 个，旧通道在第 6、12、18、23 个时隙运行。首次同样等待计划时间。仅旧通道可用时降级为每小时 5 个分散时隙，登录恢复后自动切回。限流独立冷却至少一小时，不借用另一通道时隙；共享请求限流整体等待，尊重更长 Retry-After。

双通道计划、进度、滚动预算和冷却保存到 `data/dual-schedules.json`，重启后继续，手动停止不会恢复。错过的计划不集中补发；在途结果不确定时暂停核对，避免重复创建。新启动的双通道任务不再设置本次数量，每小时循环，直到手动停止；前端显示累计注册数而非有限目标进度。750 个账号总数上限仍保留，满额时每小时核对，释放容量后自动恢复，不会删除别名。旧版本已保存的有限数量任务按原目标恢复，不擅自延长；重新启动双通道即使用持续模式。旧通道和单独新通道模式保持原有设置，不随此次变更调整。

以上是参考项目的本地调度上限，不代表 Apple 保证的配额。协议与调度参考 [t508708/icloud-mail](https://github.com/t508708/icloud-mail)，固定提交及原始来源/许可说明保存在 `internal/icloudmail/`。详细接口和区别见 API.md。

## AppleServices 账号导入（Web HME + IMAP）

Windows 面板的“同步到服务器”填写本服务的 **HTTPS 根地址**及独立同步密钥。
接口是 `POST /api/integrations/appleservices/accounts`，不复用程序调用 API Token，
不使用浏览器登录 Cookie；未设置 `ICLOUD_HME_IMPORT_TOKEN` 时返回 404、接口关闭。
精确请求、响应和错误码见 [API.md](API.md#appleservices-账号导入)。

服务器会依次验证 Apple 返回的身份、完整 HME 目录及主 iCloud 收件地址、固定
`imap.mail.me.com:993` 的 TLS 登录。全部通过才原子 upsert；任何失败保留旧凭据。
同一源 ID 并发、重试及重启后都复用同一个账号 ID。旧账号认领还会验证旧 Cookie
的真实身份；多条匹配、身份/收件地址冲突或无法证明旧账号身份时返回 409。
保留原账号名称、代理、外部收件设置、第二通道会话、别名使用记录及任务。
**导入不会创建别名、读取邮件、修改转发、启动任务或登录 Apple Account 第二通道。**

部署使用本 checkout 构建，并保留 `./data:/app/data`：

1. 先备份 `data`、`.env` 和反向代理配置到仅管理员可读的目录，不提交备份。
2. 在服务器设置至少 6 字符的 `ICLOUD_HME_IMPORT_TOKEN`（建议使用更长的随机密钥），写入未跟踪的
   `.env`（权限 `0600`）；不在终端输出、聊天、工单或 Git 中粘贴密钥。
   例如以下脚本仅在尚未配置时追加 64 位十六进制随机密钥：

   ```sh
   python3 - <<'PY'
   import os, secrets
   path = '.env'
   fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
   os.fchmod(fd, 0o600)
   with os.fdopen(fd, 'r+') as f:
       current = f.read()
       if not any(line.startswith('ICLOUD_HME_IMPORT_TOKEN=') for line in current.splitlines()):
           f.write(('\n' if current and not current.endswith('\n') else '') +
                   'ICLOUD_HME_IMPORT_TOKEN=' + secrets.token_hex(32) + '\n')
   PY
   ```

3. HTTPS 代理配置参考 `deploy/appleservices-import.nginx.conf.example`。
   该路由禁止 HTTP/跳转、0-RTT、代理重试/缓存及请求日志。不要记录 Authorization
   或请求体；不要开启 HTTP 客户端调试。Compose 默认仅绑定 `127.0.0.1:8081`；
   代理在另一容器时应使用私有 Docker 网络，不要把凭据接口直接暴露到公网 HTTP。
   `ICLOUD_HME_BIND` 可以显式调整绑定地址（不要在未隔离的网络使用 `0.0.0.0`）。
4. 设置 `ICLOUD_HME_SECURE_COOKIE=true`，执行 `docker compose up -d --build`。
   `data` 目录会收紧为 `0700`，账号文件为 `0600`；旧格式兼容，不清空数据。
5. 用正常证书校验检查 HTTPS/健康状态；无密钥请求应为 401。从服务器 `.env` 安全
   取出 `ICLOUD_HME_IMPORT_TOKEN` 值填入 Windows 面板，再做首次真实同步。
   密钥轮换只更新此变量并重建容器，不影响账号或原来的 API Token。

鉴权/验证失败每连接 IP 最多 5 次/15 分钟，成功清零；并发在途最多 2 个，导入
按事务串行、单次总超时 90 秒。当前不信任任意 X-Forwarded-For，反向代理下多个
客户端可能共享失败额度；不要为了解除限流而信任所有代理。

### 多 Apple 账号的 API 分配协调

- `POST /api/v1/aliases`（或 `/aliases/allocate`）省略 `account_id` 时，从全部账号选择：优先当前 `caller_id` 历史分配较少的账号，再比较全局历史分配数量，最后按账号 ID 稳定排序。依据持久化占用账本，重启不重置分配进度；已关联账号的历史导入也计入统计。
- 先在候选账号中寻找可用的已有别名；全部可读库存均无可用项后，才在成功读取、未达容量上限的账号上按需创建。单个账号读取失败不会阻塞其他账号，失败账号不被当作空库存。全部不可读时保留上游错误，不伪报库存耗尽。
- 显式传 `account_id` 时严格固定账号，不跨号回退。账号摘要状态不是实时健康判断，以实际目录读取为准。上游创建失败/结果不明时不自动跨账号重试，避免多建邮箱。
- `caller_id` 内跨账号统一按邮箱去重；不同调用商保持原来的独立去重规则。同一 `caller_id + request_id + 参数` 重试返回原来的账号和邮箱；参数变化返回 `REQUEST_ID_CONFLICT`。新增请求请使用新 `request_id`。
- **调用方必须保存返回的 `data.account_id` 和 `data.email`**，收件使用 `/api/v1/inbox?account_id=返回的账号&alias=返回的邮箱`；正文查询仍传该账号，不能始终绑定第一个 Apple 账号。
- `auto_create:false` 只取已有库存，适合由后台计划补充库存的场景。此改动不调整现有双通道 19+4 计划；自动创建仍使用原接口的通道及共享账号创建锁/容量检查。
- `/api/v1/aliases/usage` 和管理面板统计新增 `accounts` 数组（`account_id`、`name`、`used_count`、`unique_email_count`、`available`）及 `allocation_policy: "least_used_existing_first"`。`available` 仅表示账号仍存在，不代表上游在线；统计是历史占用量，不是实时剩余库存。`?caller_id=...` 同时限定调用商及账号维度统计。未关联或已删除账号的历史记录保留。
- 账本仍使用 `data/alias-usage.json`，兼容旧记录和旧请求指纹，不清空账号、凭据、密钥或已分配记录。协调与去重保证限于单服务进程，勿让多个副本同时写同一账本。

### Windows 按需登录恢复（事件驱动）

- 仅创建新通道时出现 `APPLE_ACCOUNT_LOGIN_REQUIRED`，或该次创建预检查无新通道会话准备降级，才登记恢复需求。账号必须具有已验证的导入身份绑定。限流、条款确认、容量、网络错误及创建结果不明不会触发自动登录，也不会重放创建。
- Windows 使用现有 `ICLOUD_HME_IMPORT_TOKEN` 调用 `POST /api/integrations/appleservices/recovery/wait`（最长 25 秒）和 `/recovery/complete`；精确请求协议见 `SERVER_CODEX_ON_DEMAND_LOGIN.md`。32 个独立等待槽和 4 个确认槽，不占用登录/导入的 `importSlots`；成功等待不累计认证失败次数。
- 队列以 `data/login-recovery.json` 原子保存，权限 `0600`。一个账号一个恢复状态；租约 300 秒，绑定客户端、source、身份/导入/配置版本和失效会话版本。丢失响应、租约过期和 `local_unavailable` 重发同一 job ID。离线重试至少等 5 分钟；`failed` 最多 3 次尝试，前两次分别退避 5/15 分钟后换 job ID；达到上限或需要验证码则持久暂停。重新登录/同步导致版本变化后，旧任务不再有效。
- `ready` 必须经服务器对已保存 AppleSession 的只读 management 验证，单凭客户端声明不会恢复通道。现有集成登录接口可记录当前租约对应的新会话版本；浏览器手动登录、身份、代理、区域或密码配置变化使旧租约失效。恢复只清除对应版本的拒绝标记，不改变任务停止状态、计划时间、预算或创建数量。
- 管理面板“API 设置 → Windows 按需登录恢复”及受管理员认证保护的 `GET /api/settings/recovery` 展示本地队列、等待连接和验证提示，不访问 Apple。不暴露主密码、OTP、Cookie、租约或客户端 ID。等待连接为 0 可能是正在重连或处理任务，不作为离线的充分证据。
- **没有定时 Apple 会话检查**：原维护定时器仅清理本地过期 OTP challenge，已移除后台网络续期循环；创建过程中按需续期以及显式 `apple-login status`/`complete ready` 的只读验证保留。Windows 无任务时也不登录、不读验证码。
- 协调面向单服务进程；不运行多个写同一账号/队列数据目录的副本。长轮询代理读取超时应超过 35 秒。部署保留账号、任务、密钥和数据卷；没有自然失效事件时只能验收接口与等待连接，不能宣称已完成真实恢复闭环。
