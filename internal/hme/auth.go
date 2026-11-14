// Package hme - iCloud 认证模块
//
// 基于 Go-iClient 项目实现完整的 SRP (Secure Remote Password) 登录流程,
// 支持双重认证 (2FA),登录成功后提取 session token Cookie。
package hme

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/pbkdf2"

	http "github.com/bogdanfinn/fhttp"
	"icloud-hme/internal/srp"
)

// AuthEndpoints iCloud 认证 API 端点
const (
	OAuthClientID = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"

	authStartFmt    = "https://idmsa.apple.com/appleauth/auth/authorize/signin?frame_id=auth-%s&language=en_US&skVersion=7&iframeId=auth-%s&client_id=%s&redirect_uri=https://www.icloud.com&response_type=code&response_mode=web_message&state=auth-%s&authVersion=latest"
	authFederate    = "https://idmsa.apple.com/appleauth/auth/federate"
	authInit        = "https://idmsa.apple.com/appleauth/auth/signin/init"
	authComplete    = "https://idmsa.apple.com/appleauth/auth/signin/complete?isRememberMeEnabled=false"
	authOptions     = "https://idmsa.apple.com/appleauth/auth"
	submitSecurity  = "https://idmsa.apple.com/appleauth/auth/verify/%s/securitycode"
	authTrust       = "https://idmsa.apple.com/appleauth/auth/2sv/trust"
	authWebFmt      = "https://setup.icloud.com/setup/ws/1/accountLogin"
	authValidateFmt = "https://setup.icloud.com/setup/ws/1/validate?clientBuildNumber=%s&clientMasteringNumber=%s&clientId=%s"
)

var (
	// ErrOTPRequired 表示密码校验已通过，登录会话正在等待双重验证码。
	ErrOTPRequired = errors.New("账号启用了双重认证,需要提供 OTP")
	// ErrOTPInvalid 表示 Apple 拒绝了双重验证码。
	ErrOTPInvalid = errors.New("2FA 验证失败")
	// ErrWebAccessRestricted 表示验证码正确，但 Apple 的网页访问保护仍阻止会话建立。
	ErrWebAccessRestricted = errors.New("Apple 网页版 iCloud 访问受限")
)

// OTPProvider 双重认证回调函数,返回 2FA 验证码
type OTPProvider func() (string, error)

// LoginRejectedError identifies rejection of a fresh password login, not an
// expired iCloud Cookie. It deliberately contains no upstream body or secrets.
type LoginRejectedError struct {
	Status int
}

func (e *LoginRejectedError) Error() string {
	return fmt.Sprintf("Apple 拒绝账号密码登录: HTTP %d", e.Status)
}

// authState 保存认证过程中的状态
type authState struct {
	username       string
	frameId        string
	clientId       string
	authAttr       string
	sessionID      string
	scnt           string
	authToken      string
	trustToken     string
	accountCountry string
	dsid           string
	hashcashBits   int
	hashcashSeed   string
}

// Login 保留单调用兼容入口。HTTP 两阶段登录应优先使用 StartLogin/CompleteLogin，
// 以保证验证码提交复用 Apple 发放验证码时的同一会话。
func (c *Client) Login(username, password string, otpProvider OTPProvider) error {
	err := c.StartLogin(username, password)
	if !errors.Is(err, ErrOTPRequired) || otpProvider == nil {
		return err
	}
	otp, err := otpProvider()
	if err != nil {
		return fmt.Errorf("获取 2FA 验证码失败: %w", err)
	}
	return c.CompleteLogin(otp)
}

// StartLogin 校验 Apple 账户密码并创建两阶段登录会话。
// 需要 2FA 时会主动向可信设备请求验证码，并返回 ErrOTPRequired。
func (c *Client) StartLogin(username, password string) error {
	c.pendingAuth = nil
	state := &authState{
		username: username,
		frameId:  strings.ToLower(uuid.New().String()),
		clientId: OAuthClientID,
	}

	// 1. 提交用户名并建立 Apple 登录 Cookie 会话。
	if err := c.authFederate(state); err != nil {
		return fmt.Errorf("auth federate: %w", err)
	}

	// 2. 获取与当前 Cookie 会话绑定的 Hashcash 挑战。
	if err := c.loadHashcashChallenge(state); err != nil {
		return fmt.Errorf("hashcash: %w", err)
	}

	// 3. SRP 协议初始化。
	params := srp.GetParams(2048)
	params.NoUserNameInX = true
	srpClient := srp.NewSRPClient(params, nil)

	// 4. 获取 salt 和 B
	authInitResp, err := c.authInit(state, base64.StdEncoding.EncodeToString(srpClient.GetABytes()))
	if err != nil {
		return fmt.Errorf("auth init: %w", err)
	}

	// 5. 解码 salt 和 B
	bDec, err := base64.StdEncoding.DecodeString(authInitResp.B)
	if err != nil {
		return fmt.Errorf("decode B: %w", err)
	}
	saltDec, err := base64.StdEncoding.DecodeString(authInitResp.Salt)
	if err != nil {
		return fmt.Errorf("decode salt: %w", err)
	}

	// 6. 按 Apple 返回的 SRP 协议生成密码密钥。
	// s2k_fo 使用 SHA-256 摘要的十六进制文本，而不是原始摘要字节。
	passKey, err := deriveSRPPassword(authInitResp.Protocol, password, saltDec, authInitResp.Iteration)
	if err != nil {
		return err
	}
	c.log("SRP 初始化完成 protocol=%s iteration=%d", authInitResp.Protocol, authInitResp.Iteration)

	// 7. 处理挑战
	srpClient.ProcessClientChanllenge([]byte(username), passKey, saltDec, bDec)

	// 8. 提交 SRP 响应。
	requiresOTP, err := c.authComplete(state, authInitResp.C, base64.StdEncoding.EncodeToString(srpClient.M1), base64.StdEncoding.EncodeToString(srpClient.M2))
	if err != nil {
		return fmt.Errorf("auth complete: %w", err)
	}
	if requiresOTP {
		c.pendingAuth = state
		if err := c.prepareTwoFactor(state); err != nil {
			c.pendingAuth = nil
			return fmt.Errorf("准备 2FA 验证失败: %w", err)
		}
		return ErrOTPRequired
	}

	return c.finishLogin(state)
}

// CompleteLogin 在 StartLogin 保存的同一 Apple 会话中提交验证码。
func (c *Client) CompleteLogin(otp string) error {
	state := c.pendingAuth
	if state == nil {
		return fmt.Errorf("OTP 验证会话已失效,请重新输入密码")
	}
	if err := c.verifyTwoFactorCode(state, otp); err != nil {
		return err
	}

	if err := c.getTrust(state); err != nil {
		return fmt.Errorf("get trust: %w", err)
	}
	if err := c.finishLogin(state); err != nil {
		return err
	}
	c.pendingAuth = nil
	return nil
}

// finishLogin 用 Apple 会话令牌换取 iCloud Web Cookie。
func (c *Client) finishLogin(state *authState) error {
	if err := c.authenticateWeb(state); err != nil {
		return fmt.Errorf("authenticate web: %w", err)
	}

	cookies := c.extractSessionCookies()
	c.Cookies = cookies
	c.log("登录成功,获取到 %d 个 Cookie", len(cookies))
	return nil
}

// deriveSRPPassword 实现 Apple s2k / s2k_fo 两种密码派生方式。
func deriveSRPPassword(protocol, password string, salt []byte, iterations int) ([]byte, error) {
	if iterations <= 0 {
		return nil, fmt.Errorf("SRP iteration 无效: %d", iterations)
	}
	passHash := sha256.Sum256([]byte(password))
	var material []byte
	switch protocol {
	case "s2k":
		material = passHash[:]
	case "s2k_fo":
		material = []byte(hex.EncodeToString(passHash[:]))
	default:
		return nil, fmt.Errorf("不支持的 SRP 协议: %s", protocol)
	}
	return pbkdf2.Key(material, salt, iterations, 32, sha256.New), nil
}

// mintAppleHashcash 生成 Apple 登录接口使用的 Hashcash。
// 格式为 1:<bits>:<UTC时间>:<challenge>::<十进制计数器>。
func mintAppleHashcash(challenge string, bits int, now time.Time) (string, error) {
	if challenge == "" {
		return "", fmt.Errorf("缺少 X-Apple-HC-Challenge")
	}
	// 正常挑战约为 20 位。限制上界，避免异常响应造成无界 CPU 消耗。
	if bits < 1 || bits > 30 {
		return "", fmt.Errorf("无效的 X-Apple-HC-Bits: %d", bits)
	}
	date := now.UTC().Format("20060102150405")
	for counter := uint64(0); ; counter++ {
		candidate := fmt.Sprintf("1:%d:%s:%s::%d", bits, date, challenge, counter)
		digest := sha1.Sum([]byte(candidate)) // Apple 的协议格式规定使用 SHA-1。
		if leadingZeroBits(digest[:]) >= bits {
			return candidate, nil
		}
	}
}

func leadingZeroBits(data []byte) int {
	total := 0
	for _, b := range data {
		if b == 0 {
			total += 8
			continue
		}
		for mask := byte(0x80); mask != 0 && b&mask == 0; mask >>= 1 {
			total++
		}
		break
	}
	return total
}

// --- 认证流程的各步骤 ---

// authStart 初始化 frameId 和 clientId
func (c *Client) authStart(state *authState) error {
	state.frameId = strings.ToLower(uuid.New().String())
	state.clientId = OAuthClientID

	req, err := http.NewRequest("GET", fmt.Sprintf(authStartFmt, state.frameId, state.frameId, state.clientId, state.frameId), nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	c.captureAuthHeaders(state, resp)
	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

// authFederate 提交用户名
func (c *Client) authFederate(state *authState) error {
	data := `{"accountName":"` + state.username + `","rememberMe":true}`
	req, err := http.NewRequest("POST", authFederate, bytes.NewReader([]byte(data)))
	if err != nil {
		return err
	}

	req.Header = c.srpAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	c.captureAuthHeaders(state, resp)
	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

// loadHashcashChallenge 获取与当前 Apple 登录 Cookie 会话绑定的工作量证明挑战。
func (c *Client) loadHashcashChallenge(state *authState) error {
	req, err := http.NewRequest("GET", "https://idmsa.apple.com/appleauth/auth/signin?widgetKey="+url.QueryEscape(state.clientId), nil)
	if err != nil {
		return err
	}
	req.Header = c.continuationAuthHeaders(req.Header, state)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.captureAuthHeaders(state, resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("获取 Hashcash 挑战失败: HTTP %d", resp.StatusCode)
	}
	if state.hashcashSeed == "" || state.hashcashBits == 0 {
		// Apple 并非对每个账号/出口都下发工作量证明。缺失时继续；
		// signin/init 若稍后下发挑战，captureAuthHeaders 仍会接住它。
		c.log("Apple 未要求 Hashcash，继续 SRP 登录")
	}
	return nil
}

// authInitResp authInit 响应
type authInitResp struct {
	Iteration int    `json:"iteration"`
	Salt      string `json:"salt"`
	Protocol  string `json:"protocol"`
	B         string `json:"b"`
	C         string `json:"c"`
}

// authInit 初始化 SRP 认证
func (c *Client) authInit(state *authState, a string) (*authInitResp, error) {
	reqBody := map[string]interface{}{
		"a":           a,
		"accountName": state.username,
		"protocols":   []string{"s2k", "s2k_fo"},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", authInit, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header = c.srpAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	c.captureAuthHeaders(state, resp)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	var result authInitResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// authComplete 提交 SRP 响应，并返回是否需要 2FA。
func (c *Client) authComplete(state *authState, challenge, m1, m2 string) (bool, error) {
	reqBody := map[string]interface{}{
		"accountName": state.username,
		"rememberMe":  false,
		"m1":          m1,
		"c":           challenge,
		"m2":          m2,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequest("POST", authComplete, bytes.NewReader(data))
	if err != nil {
		return false, err
	}

	req.Header = c.srpAuthHeaders(req.Header, state)
	if state.hashcashSeed != "" && state.hashcashBits > 0 {
		hashcash, err := mintAppleHashcash(state.hashcashSeed, state.hashcashBits, time.Now())
		if err != nil {
			return false, err
		}
		req.Header.Set("X-Apple-HC", hashcash)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	c.captureAuthHeaders(state, resp)

	switch resp.StatusCode {
	case 200:
		return false, nil
	case 409:
		return true, nil
	case 401, 403:
		return false, &LoginRejectedError{Status: resp.StatusCode}
	case 412:
		return false, fmt.Errorf("需要先在 appleid.apple.com 同意隐私条款")
	default:
		return false, fmt.Errorf("auth complete 失败: HTTP %d", resp.StatusCode)
	}
}

// prepareTwoFactor 读取验证方式并把会话推进到可提交验证码的状态。
func (c *Client) prepareTwoFactor(state *authState) error {
	req, err := http.NewRequest("GET", authOptions, nil)
	if err != nil {
		return err
	}
	req.Header = c.continuationAuthHeaders(req.Header, state)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.captureAuthHeaders(state, resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("读取 2FA 验证方式失败: HTTP %d", resp.StatusCode)
	}
	c.log("2FA 会话已准备 status=%d", resp.StatusCode)
	return nil
}

// verifyTwoFactorCode 在原始 SRP 会话中提交双重验证码。
func (c *Client) verifyTwoFactorCode(state *authState, otp string) error {
	reqBody := map[string]interface{}{
		"securityCode": map[string]string{"code": otp},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", fmt.Sprintf(submitSecurity, "trusteddevice"), bytes.NewReader(data))
	if err != nil {
		return err
	}

	// 续接 2FA 的接口只接受最小头集合。OAuth-*、Origin、Referer 和
	// X-Requested-With 会让 Apple 把正确验证码判为会话错序。
	req.Header = c.continuationAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	c.captureAuthHeaders(state, resp)
	if !twoFactorCodeAccepted(resp.StatusCode, resp.Header, body) {
		if strings.EqualFold(resp.Header.Get("X-Apple-Edp"), "true") || twoFactorBodySaysValid(body) {
			return fmt.Errorf("%w: 验证码正确但 Apple 返回 HTTP %d；请检查 Apple 账户的“在网页上访问 iCloud 数据”设置", ErrWebAccessRestricted, resp.StatusCode)
		}
		return fmt.Errorf("%w: HTTP %d%s", ErrOTPInvalid, resp.StatusCode, appleServiceErrorSuffix(body))
	}
	c.log("2FA 验证码已接受 status=%d", resp.StatusCode)
	return nil
}

// twoFactorCodeAccepted 仅接受真正完成验证的 2xx 响应。
// HTTP 409 即使正文出现 valid=true，也表示会话仍受 EDP/网页访问策略阻止。
func twoFactorCodeAccepted(status int, header http.Header, body []byte) bool {
	_ = header
	_ = body
	if status >= 200 && status < 300 {
		return true
	}
	return false
}

func twoFactorBodySaysValid(body []byte) bool {
	var payload struct {
		SecurityCode struct {
			Valid bool `json:"valid"`
		} `json:"securityCode"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.SecurityCode.Valid
}

func appleServiceErrorSuffix(body []byte) string {
	var payload struct {
		ServiceErrors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"serviceErrors"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.ServiceErrors) == 0 {
		return ""
	}
	first := payload.ServiceErrors[0]
	if first.Code == "" && first.Message == "" {
		return ""
	}
	return fmt.Sprintf(" (Apple %s: %s)", first.Code, first.Message)
}

// getTrust 获取 trust token
func (c *Client) getTrust(state *authState) error {
	req, err := http.NewRequest("GET", authTrust, nil)
	if err != nil {
		return err
	}

	req.Header = c.continuationAuthHeaders(req.Header, state)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.captureAuthHeaders(state, resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("trust 失败: HTTP %d", resp.StatusCode)
	}
	c.log("Apple 会话已信任 status=%d", resp.StatusCode)
	return nil
}

// authenticateWeb 认证 iCloud Web 服务
func (c *Client) authenticateWeb(state *authState) error {
	country := state.accountCountry
	if country == "" {
		country = "USA"
	}
	data, err := json.Marshal(map[string]interface{}{
		"dsWebAuthToken":     state.authToken,
		"accountCountryCode": country,
		"extended_login":     true,
		"trustToken":         state.trustToken,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", authWebFmt, bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.Origin())
	req.Header.Set("Accept", "*/*")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.captureAuthHeaders(state, resp)

	if resp.StatusCode != 200 {
		return fmt.Errorf("auth web 失败: HTTP %d", resp.StatusCode)
	}

	var result struct {
		DsInfo struct {
			Dsid string `json:"dsid"`
		} `json:"dsInfo"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	state.dsid = result.DsInfo.Dsid

	// 复制 idmsa.apple.com 的 Cookie 到 icloud.com
	u1, _ := url.Parse("https://idmsa.apple.com")
	u2, _ := url.Parse("https://icloud.com")
	cookies := c.httpc.GetCookies(u1)
	c.httpc.SetCookies(u2, cookies)

	return nil
}

// captureAuthHeaders 汇总 Apple 在每一步刷新出的短期会话头。
func (c *Client) captureAuthHeaders(state *authState, resp *http.Response) {
	if value := resp.Header.Get("X-Apple-Auth-Attributes"); value != "" {
		state.authAttr = value
	}
	if value := resp.Header.Get("X-Apple-ID-Session-Id"); value != "" {
		state.sessionID = value
	}
	if value := resp.Header.Get("scnt"); value != "" {
		state.scnt = value
	}
	if value := resp.Header.Get("X-Apple-Session-Token"); value != "" {
		state.authToken = value
	}
	if value := resp.Header.Get("X-Apple-TwoSV-Trust-Token"); value != "" {
		state.trustToken = value
	}
	if value := resp.Header.Get("X-Apple-ID-Account-Country"); value != "" {
		state.accountCountry = value
	}
	if value := resp.Header.Get("X-Apple-HC-Challenge"); value != "" {
		state.hashcashSeed = value
	}
	if value := resp.Header.Get("X-Apple-HC-Bits"); value != "" {
		if bits, err := strconv.Atoi(value); err == nil {
			state.hashcashBits = bits
		}
	}
}

// extractSessionCookies 提取 session token Cookie
func (c *Client) extractSessionCookies() map[string]string {
	cookies := make(map[string]string)
	for _, rawURL := range []string{
		c.Origin(),
		"https://" + c.Host,
		c.SetupURL(),
		"https://idmsa.apple.com",
	} {
		u, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		for _, cookie := range c.httpc.GetCookies(u) {
			if cookie.Name != "" && cookie.Value != "" {
				cookies[cookie.Name] = cookie.Value
			}
		}
	}
	return cookies
}

const appleAuthUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// srpAuthHeaders 用于 federate、signin/init 与 signin/complete。
// 这里刻意不发送 OAuth-*、Origin、Referer；这些额外头会让后续 2FA 会话错序。
func (c *Client) srpAuthHeaders(header http.Header, state *authState) http.Header {
	header.Set("User-Agent", appleAuthUserAgent)
	header.Set("X-Apple-Widget-Key", state.clientId)
	if state.scnt != "" {
		header.Set("scnt", state.scnt)
	}
	if state.sessionID != "" {
		header.Set("X-Apple-ID-Session-Id", state.sessionID)
	}

	header.Set("X-Requested-With", "XMLHttpRequest")
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json, text/javascript")
	return header
}

// continuationAuthHeaders 用于 /auth、验证码提交和 /2sv/trust。
// 它必须保持最小化，尤其不能携带 X-Requested-With 或 OAuth-*。
func (c *Client) continuationAuthHeaders(header http.Header, state *authState) http.Header {
	header.Set("User-Agent", appleAuthUserAgent)
	header.Set("X-Apple-Widget-Key", state.clientId)
	if state.scnt != "" {
		header.Set("scnt", state.scnt)
	}
	if state.sessionID != "" {
		header.Set("X-Apple-ID-Session-Id", state.sessionID)
	}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	return header
}

// updateAuthHeaders 保留给现有调用/测试；新代码按端点选择上面的头集合。
func (c *Client) updateAuthHeaders(header http.Header, state *authState) http.Header {
	return c.srpAuthHeaders(header, state)
}

// Validate 验证当前 Cookie 是否有效
func (c *Client) Validate() (bool, error) {
	if len(c.Cookies) == 0 {
		return false, fmt.Errorf("无 Cookie")
	}
	// 简单实现：尝试调用 validate 端点
	err := c.ValidateSession()
	if err != nil {
		return false, err
	}
	return true, nil
}
