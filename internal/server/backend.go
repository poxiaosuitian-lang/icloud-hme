// Package server - 可替换业务接口与 Manager 适配器。
//
// Backend 边界固定为高层业务动作,不把具体 *hme.Client 或 *mail.Client 暴露给 handler。
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

// BackendError 是后端返回的稳定错误,携带 HTTP 状态码与稳定错误码。
type BackendError struct {
	CreationChannel string
	Status          int
	Code            string
	Message         string
	RetryAfter      time.Duration
}

func (e *BackendError) Error() string { return e.Message }

// InboxQuery 是收件箱查询参数。
type InboxQuery struct {
	AccountID string
	Alias     string
	Limit     int
	Days      int
}

// InboxResult 是收件箱查询结果。
type InboxResult struct {
	AccountID string         `json:"account_id"`
	Alias     string         `json:"alias,omitempty"`
	Count     int            `json:"count"`
	Messages  []mail.Message `json:"messages"`
	Method    string         `json:"method"`
}

// Backend 是可替换的业务接口;handler 只依赖本接口,测试使用内存 fake。
type Backend interface {
	ListAccounts() []account.Summary
	AddAccount(account.AddAccountInput) (account.Summary, error)
	UpdateAccount(string, account.UpdateAccountInput) (account.Summary, error)
	UpdateProxy(string, string) (account.Summary, error)
	UpdateCookies(string, string) (account.Summary, error)
	SetAppPassword(string, string, string) (account.Summary, error)
	SetMailbox(string, account.MailboxConfig) (account.Summary, error)
	LoginAccount(string, string, string) (account.Summary, error)
	RemoveAccount(string) bool
	CreateAlias(string, string) (*hme.CreateResult, error)
	ListAliases(string) ([]hme.Alias, error)
	SetAliasActive(string, string, bool) (bool, error)
	DeleteAlias(string, string) error
	ListInbox(InboxQuery) (InboxResult, error)
	GetMessage(string, string) (*mail.FullMessage, error)
	DeleteMessage(string, string) error
	Reload() error
}

// managerBackend 是生产 Backend,包装 *account.Manager。
type managerBackend struct {
	mgr *account.Manager
}

// ListAccounts 返回账号安全摘要列表。
func (b *managerBackend) ListAccounts() []account.Summary {
	return b.mgr.ListSummaries()
}

// AddAccount 添加账号。
func (b *managerBackend) AddAccount(in account.AddAccountInput) (account.Summary, error) {
	sum, err := b.mgr.AddAccountWithInput(in)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	return sum, nil
}

// UpdateAccount 编辑账号基本信息。
func (b *managerBackend) UpdateAccount(id string, in account.UpdateAccountInput) (account.Summary, error) {
	sum, err := b.mgr.UpdateMetadata(id, in)
	if err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	return sum, nil
}

// UpdateProxy 更新或清除账号代理。
func (b *managerBackend) UpdateProxy(id, proxy string) (account.Summary, error) {
	sum, err := b.mgr.UpdateProxy(id, proxy)
	if err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	return sum, nil
}

// UpdateCookies 更新账号 Cookie。cookies 为原始文本(Header String 或 JSON)。
func (b *managerBackend) UpdateCookies(id, cookies string) (account.Summary, error) {
	parsed, err := account.ParseCookieInput(cookies)
	if err != nil {
		return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: err.Error()}
	}
	if err := b.mgr.UpdateCookies(id, parsed); err != nil {
		return account.Summary{}, mapAccountErr(err)
	}
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// SetAppPassword 设置 iCloud 邮箱与 App 专用密码并测试 IMAP 连接。
func (b *managerBackend) SetAppPassword(id, icloudEmail, appPassword string) (account.Summary, error) {
	if err := b.mgr.SetAppPassword(id, icloudEmail, appPassword); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		if strings.Contains(msg, "不能为空") {
			return account.Summary{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
		}
		// IMAP 连接失败属于上游错误,不拼接详细错误
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "IMAP 验证失败,请检查邮箱与 App 专用密码"}
	}
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// SetMailbox configures and verifies an external IMAP mailbox.
func (b *managerBackend) SetMailbox(id string, config account.MailboxConfig) (account.Summary, error) {
	if err := b.mgr.SetMailbox(id, config); err != nil {
		if strings.Contains(err.Error(), "账号不存在") {
			return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
		}
		return account.Summary{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "收件邮箱验证失败,请检查邮箱、授权码和 IMAP 配置"}
	}
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// LoginAccount 使用 iCloud 密码登录账号,成功只返回 Summary,绝不返回 Cookies。
func (b *managerBackend) LoginAccount(id, password, otpCode string) (account.Summary, error) {
	client, err := b.mgr.HMEClientWithPassword(id, password, otpCode)
	if err != nil {
		log.Printf("iCloud 登录失败 account=%s err=%v", id, err)
		return account.Summary{}, classifyLoginErr(err)
	}
	_ = client
	sum, ok := b.mgr.GetAccount(id)
	if !ok {
		return account.Summary{}, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	return sum.Summary(), nil
}

// classifyLoginErr 把 iCloud 登录错误映射为稳定错误。
func classifyLoginErr(err error) *BackendError {
	var rejected *hme.LoginRejectedError
	if errors.As(err, &rejected) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "ICLOUD_LOGIN_REJECTED", Message: "Apple 拒绝了账号密码登录，尚未更新 Cookie。请核对下方 iCloud 登录邮箱，并使用该账号的登录密码（不是 App 专用密码）；若官网登录也失败，请先在官网完成验证。"}
	}
	msg := err.Error()
	if strings.Contains(msg, "用户名或密码错误") {
		return &BackendError{Status: http.StatusUnauthorized, Code: "ICLOUD_INVALID_CREDENTIALS", Message: "iCloud 账号或密码错误"}
	}
	if strings.Contains(msg, "需要提供 OTP") {
		return &BackendError{Status: http.StatusConflict, Code: "OTP_REQUIRED", Message: "需要提供 OTP 验证码"}
	}
	if strings.Contains(msg, "OTP 验证会话已失效") {
		return &BackendError{Status: http.StatusConflict, Code: "OTP_SESSION_EXPIRED", Message: "验证码会话已失效,请重新输入密码"}
	}
	if strings.Contains(msg, "2FA 验证失败") {
		return &BackendError{Status: http.StatusUnauthorized, Code: "OTP_INVALID", Message: "OTP 验证码错误"}
	}
	if strings.Contains(msg, "Apple 网页版 iCloud 访问受限") {
		return &BackendError{Status: http.StatusConflict, Code: "ICLOUD_WEB_ACCESS_RESTRICTED", Message: "验证码正确，但 Apple 阻止了网页版 iCloud 会话；请在 Apple 设备的 iCloud 设置中启用“在网页上访问 iCloud 数据”后重试"}
	}
	if strings.Contains(msg, "账号不存在") {
		return &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	if isSessionError(msg) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: "iCloud 会话失效,请更新 Cookie"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "iCloud 登录失败,请稍后重试"}
}

// RemoveAccount 删除账号。
func (b *managerBackend) RemoveAccount(id string) bool {
	return b.mgr.RemoveAccount(id)
}

// CreateAlias 创建 HME 别名。
func (b *managerBackend) CreateAlias(accountID, label string) (*hme.CreateResult, error) {
	client, err := b.mgr.HMEClient(accountID, false)
	if err != nil {
		return nil, mapAccountErr(err)
	}
	// A failed reservation may already have succeeded upstream. Never generate
	// another candidate automatically; the caller refreshes the count first.
	result, err := client.CreateAlias(label, 1)
	_ = b.mgr.SaveCookies(accountID, client.Cookies, client.AccountRevision, client.WebRecoveryRevision)
	if err != nil {
		return nil, classifyUpstreamErr("创建邮箱失败", err)
	}
	return result, nil
}

// ListAliases 列出账号的 HME 别名。
func (b *managerBackend) ListAliases(accountID string) ([]hme.Alias, error) {
	client, err := b.mgr.HMEClient(accountID, false)
	if err != nil {
		return nil, mapAccountErr(err)
	}
	aliases, err := client.ListAliases()
	_ = b.mgr.SaveCookies(accountID, client.Cookies, client.AccountRevision, client.WebRecoveryRevision)
	if err != nil {
		return nil, classifyUpstreamErr("获取别名列表失败", err)
	}
	return aliases, nil
}

// SetAliasActive 停用或激活别名。
func (b *managerBackend) SetAliasActive(accountID, anonymousID string, active bool) (bool, error) {
	client, err := b.mgr.HMEClient(accountID, false)
	if err != nil {
		return false, mapAccountErr(err)
	}
	var success bool
	if active {
		success, err = client.ReactivateHME(anonymousID)
	} else {
		success, err = client.DeactivateHME(anonymousID)
	}
	_ = b.mgr.SaveCookies(accountID, client.Cookies, client.AccountRevision, client.WebRecoveryRevision)
	if err != nil {
		msg := "操作失败"
		if !active {
			msg = "停用失败"
		} else {
			msg = "激活失败"
		}
		return false, classifyUpstreamErr(msg, err)
	}
	return success, nil
}

// DeleteAlias 删除别名。
func (b *managerBackend) DeleteAlias(accountID, anonymousID string) error {
	client, err := b.mgr.HMEClient(accountID, false)
	if err != nil {
		return mapAccountErr(err)
	}
	err = client.Delete(anonymousID)
	_ = b.mgr.SaveCookies(accountID, client.Cookies, client.AccountRevision, client.WebRecoveryRevision)
	if err != nil {
		return classifyUpstreamErr("删除失败", err)
	}
	return nil
}

// ListInbox 读取收件箱摘要:IMAP (App Password) 优先,Web API (Cookie) 回退。
func (b *managerBackend) ListInbox(q InboxQuery) (InboxResult, error) {
	// 优先使用 IMAP 连接池 (App Password 认证,复用长连接)
	var imapMessages []mail.Message
	poolErr := b.mgr.WithMailClient(q.AccountID, func(mc *mail.Client) error {
		var e error
		if q.Alias != "" {
			imapMessages, e = mc.FindByRecipient(q.Alias, q.Limit, q.Days)
		} else {
			imapMessages, e = mc.ListInbox(q.Limit, q.Days)
		}
		return e
	})
	if poolErr == nil {
		// JSON 中空切片必须返回 [] 而不是 null。前端会把 messages 当作列表
		// 使用；新建别名尚无邮件时，nil 切片会导致页面读取 length 报错。
		if imapMessages == nil {
			imapMessages = []mail.Message{}
		}
		return InboxResult{
			AccountID: q.AccountID,
			Alias:     q.Alias,
			Count:     len(imapMessages),
			Messages:  imapMessages,
			Method:    "imap",
		}, nil
	}
	// A folder-read failure must not silently return a partial inbox.
	var folderErr *mail.FolderReadError
	if errors.As(poolErr, &folderErr) {
		return InboxResult{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取收件箱或垃圾邮件失败，请重试"}
	}
	// IMAP 失败,继续尝试 Web API

	// 回退到 Web API (Cookie 认证,无需 App Password)
	wmc, err := b.mgr.WebMailClient(q.AccountID)
	if err != nil {
		return InboxResult{}, &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "无可用邮件客户端: 需要 App Password 或 Cookie"}
	}

	messages, err := wmc.ListInboxFiltered(q.Alias, q.Limit, q.Days)
	if err != nil {
		return InboxResult{}, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取收件箱或垃圾邮件失败，请重试"}
	}
	if messages == nil {
		messages = []mail.Message{}
	}
	return InboxResult{AccountID: q.AccountID, Alias: q.Alias, Count: len(messages), Messages: messages, Method: "web_api"}, nil
}

func (b *managerBackend) GetMessage(accountID string, uid string) (*mail.FullMessage, error) {
	mc, err := b.mgr.MailClient(accountID)
	if err != nil {
		return nil, mapAccountErr(err)
	}
	if err := mc.Connect(); err != nil {
		return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取邮件失败"}
	}
	defer mc.Disconnect()
	message, err := mc.GetFullID(uid)
	if err != nil {
		return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "读取邮件详情失败"}
	}
	return message, nil
}

func (b *managerBackend) DeleteMessage(accountID string, uid string) error {
	mc, err := b.mgr.MailClient(accountID)
	if err != nil {
		return mapAccountErr(err)
	}
	if err := mc.Connect(); err != nil {
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "删除邮件失败"}
	}
	defer mc.Disconnect()
	if err := mc.DeleteID(uid); err != nil {
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "删除邮件失败"}
	}
	return nil
}

// Reload 重新加载配置。
func (b *managerBackend) Reload() error {
	if err := b.mgr.Reload(); err != nil {
		return &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "重新加载配置失败"}
	}
	return nil
}

// mapAccountErr 把账号管理器错误映射为稳定错误。
func mapAccountErr(err error) *BackendError {
	msg := err.Error()
	if strings.Contains(msg, "账号不存在") {
		return &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
	}
	if strings.Contains(msg, "Cookie") && strings.Contains(msg, "未配置") {
		return &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: "账号未配置 Cookie"}
	}
	return &BackendError{Status: http.StatusBadRequest, Code: "VALIDATION_ERROR", Message: msg}
}

// classifyUpstreamErr 把上游 (iCloud) 错误映射为稳定错误,不拼接上游响应体。
func classifyUpstreamErr(fixedMsg string, err error) *BackendError {
	if err == nil {
		return nil
	}
	var upstream *hme.UpstreamError
	if errors.As(err, &upstream) {
		// Only bounded metadata is logged; never log upstream bodies or credentials.
		log.Printf("iCloud upstream failure: operation=%s http=%d code=%q response_kind=%s rate_limited=%t retry_after_seconds=%.0f", upstream.Operation, upstream.Status, upstream.Code, upstream.ResponseKind, upstream.RateLimited, upstream.RetryAfter.Seconds())
		if upstream.RateLimited {
			return &BackendError{Status: http.StatusTooManyRequests, Code: "UPSTREAM_RATE_LIMITED", Message: "iCloud 别名创建频率受限，请等待冷却", RetryAfter: upstream.RetryAfter}
		}
		if upstream.RetryAfter > 0 && upstream.Status >= 500 {
			return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: fixedMsg, RetryAfter: upstream.RetryAfter}
		}
		if upstream.Status == http.StatusUnauthorized {
			return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: "iCloud 会话失效,请更新 Cookie"}
		}
		if upstream.Status == http.StatusForbidden {
			return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FORBIDDEN", Message: "iCloud 拒绝了此操作（HTTP 403），这不一定是 Cookie 失效；请检查 iCloud 官方页面是否允许同一操作"}
		}
		return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: fixedMsg}
	}
	if isSessionError(err.Error()) {
		return &BackendError{Status: http.StatusUnauthorized, Code: "UPSTREAM_UNAUTHORIZED", Message: "iCloud 会话失效,请更新 Cookie"}
	}
	return &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: fixedMsg}
}

// isSessionError 判断错误是否由会话失效引起。
func isSessionError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "401") ||
		strings.Contains(m, "session") || strings.Contains(m, "cookie") ||
		strings.Contains(m, "unauthorized") || strings.Contains(m, "认证") ||
		strings.Contains(m, "会话校验失败")
}

// asBackendError 提取 BackendError,非 BackendError 统一为 INTERNAL_ERROR。
func asBackendError(err error) *BackendError {
	var be *BackendError
	if errors.As(err, &be) {
		return be
	}
	return &BackendError{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR", Message: "内部错误"}
}

// cookieInputToJSON 把 handler 解析出的 map 转回 JSON 文本,交给 ParseCookieInput。
func cookieInputToJSON(cookies map[string]string) string {
	raw, err := json.Marshal(cookies)
	if err != nil {
		return ""
	}
	return string(raw)
}
