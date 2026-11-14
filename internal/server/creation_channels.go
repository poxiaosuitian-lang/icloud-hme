package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/icloudmail/apple"
)

const channelWeb = "icloud_web"
const channelApple = "apple_account"
const channelAuto = "auto"

type channelBackend interface {
	CreateAliasChannel(context.Context, string, string, string) (*hme.CreateResult, error)
}

func normalizeCreationChannel(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = channelWeb
	}
	return value, value == channelWeb || value == channelApple || value == channelAuto
}

func (s *Server) creationChannels(id, mode string) ([]string, error) {
	if mode == channelWeb {
		return []string{channelWeb}, nil
	}
	if _, ok := s.be.(channelBackend); !ok {
		return nil, &BackendError{Status: 400, Code: "CHANNEL_UNAVAILABLE", Message: "当前后端未启用新通道"}
	}
	for _, a := range s.be.ListAccounts() {
		if a.ID != id {
			continue
		}
		if a.HasAppleSession {
			if mode == channelApple {
				return []string{channelApple}, nil
			}
			return []string{channelApple, channelWeb}, nil
		}
		observed, _ := s.recoveryBinding(id)
		s.requestRecovery(observed)
		if mode == channelAuto {
			return []string{channelWeb}, nil
		}
		return nil, &BackendError{Status: 401, Code: "APPLE_ACCOUNT_LOGIN_REQUIRED", Message: "请先在账号页面登录 Apple Account 新通道"}
	}
	return nil, &BackendError{Status: 404, Code: "ACCOUNT_NOT_FOUND", Message: "账号不存在"}
}

func (s *Server) createOnChannel(ctx context.Context, id, label, channel string) (*hme.CreateResult, error) {
	observed, _ := s.recoveryBinding(id)
	var result *hme.CreateResult
	var err error
	if channel == "" || channel == channelWeb {
		channel = channelWeb
		result, err = s.be.CreateAlias(id, label)
	} else if be, ok := s.be.(channelBackend); ok {
		result, err = be.CreateAliasChannel(ctx, id, label, channel)
	} else {
		err = &BackendError{Status: 400, Code: "CHANNEL_UNAVAILABLE", Message: "当前后端未启用新通道"}
	}
	if err != nil {
		e := *asBackendError(err)
		e.CreationChannel = channel
		if channel == channelApple && e.Code == "APPLE_ACCOUNT_LOGIN_REQUIRED" {
			if result != nil && result.Email != "" {
				e.Code = "ALIAS_CONFIRMATION_REQUIRED"
				e.Message = "创建结果待确认，请核对别名列表；不会自动重放"
			} else {
				s.requestRecovery(observed)
			}
		}
		return result, &e
	}
	return result, nil
}

func mapAppleError(err error) *BackendError {
	e := &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "Apple Account 请求失败，请稍后重试", RetryAfter: apple.RetryDelay(err)}
	switch {
	case errors.Is(err, account.ErrAppleOTPRequired):
		e.Status = 409
		e.Code = "OTP_REQUIRED"
		e.Message = "请输入 Apple Account 双重认证验证码"
	case errors.Is(err, account.ErrAppleOTPExpired):
		e.Status = 409
		e.Code = "OTP_SESSION_EXPIRED"
		e.Message = "验证会话已过期，请重新输入密码"
	case errors.Is(err, apple.ErrTwoFactorCode):
		e.Status = 401
		e.Code = "OTP_INVALID"
		e.Message = "验证码错误，请重试"
	case errors.Is(err, apple.ErrAuthentication):
		e.Status = 401
		e.Code = "APPLE_ACCOUNT_AUTH_FAILED"
		e.Message = "Apple Account 登录失败，请核对账号和密码"
	case errors.Is(err, apple.ErrInvalidSession):
		e.Status = 401
		e.Code = "APPLE_ACCOUNT_LOGIN_REQUIRED"
		e.Message = "Apple Account 新通道会话已失效，请重新登录"
	case errors.Is(err, apple.ErrRateLimited), apple.IsRateLimited(err):
		e.Status = 429
		e.Code = "UPSTREAM_RATE_LIMITED"
		e.Message = "Apple Account 新通道限流，等待冷却"
	case errors.Is(err, apple.ErrAliasCapacity):
		e.Status = 409
		e.Code = "ALIAS_LIMIT_REACHED"
		e.Message = "Apple 返回隐私邮箱容量上限"
	case errors.Is(err, apple.ErrTermsRequired), errors.Is(err, apple.ErrCreationNotAllowed):
		e.Status = 409
		e.Code = "APPLE_ACCOUNT_ACTION_REQUIRED"
		e.Message = "请在 Apple Account 官网检查账号状态和隐私邮箱开通条件"
	case errors.Is(err, account.ErrAppleSessionChanged):
		e.Status = 409
		e.Code = "ACCOUNT_CHANGED"
		e.Message = "账号配置已改变，请重新登录"
	}
	return e
}

func (b *managerBackend) CreateAliasChannel(ctx context.Context, id, label, channel string) (*hme.CreateResult, error) {
	if channel == channelWeb {
		return b.CreateAlias(id, label)
	}
	result, err := b.mgr.CreateAppleAlias(ctx, id, label)
	if err != nil {
		mapped := mapAppleError(err)
		// Do not regenerate after a response loss or a failed local session save.
		if mapped.Code == "UPSTREAM_FAILURE" {
			mapped.Code = "ALIAS_CONFIRMATION_REQUIRED"
			mapped.Message = "新通道创建结果待确认，任务已停止；请核对别名列表后再启动"
		}
		return result, mapped
	}
	return result, nil
}

func (s *Server) loginAppleAccountHandler(c *gin.Context) {
	b, okBackend := s.be.(*managerBackend)
	if !okBackend {
		failCode(c, 400, "CHANNEL_UNAVAILABLE", "当前后端未启用新通道")
		return
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"otp_code"`
	}
	if c.ShouldBindJSON(&req) != nil || (req.Password == "" && len(req.Code) != 6) || len(req.Password) > 1024 {
		failCode(c, 400, "VALIDATION_ERROR", "请输入密码或六位验证码")
		return
	}
	if _, exists := b.mgr.GetAccount(c.Param("id")); !exists {
		failCode(c, 404, "ACCOUNT_NOT_FOUND", "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	summary, err := b.mgr.LoginAppleAccount(ctx, c.Param("id"), req.Password, req.Code)
	if err != nil {
		backendFail(c, mapAppleError(err))
		return
	}
	ok(c, summary)
}
