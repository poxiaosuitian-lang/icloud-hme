import { useState } from 'react'
import Dialog from './Dialog'
import { request, ApiError } from '../api/client'

interface ICloudLoginDialogProps {
  channel?: 'icloud_web' | 'apple_account'
  accountId: string
  loginEmail?: string
  configured?: boolean
  open: boolean
  onClose: () => void
  onSaved: () => void
}

/** iCloud 密码登录对话框:支持 OTP 两阶段 */
export default function ICloudLoginDialog({
  accountId,
  channel = 'icloud_web',
  loginEmail,
  configured = false,
  open,
  onClose,
  onSaved,
}: ICloudLoginDialogProps) {
  const apple = channel === 'apple_account'
  const [password, setPassword] = useState('')
  const [otp, setOtp] = useState('')
  const [otpRequired, setOtpRequired] = useState(false)
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit() {
    if (submitting) return
    setSubmitting(true)
    setError('')
    try {
      await request(`/api/accounts/${accountId}/${apple ? 'apple-login' : 'login'}`, {
        method: 'POST',
        body: JSON.stringify({
          ...(otpRequired ? { otp_code: otp } : { password }),
        }),
      })
      setPassword('')
      setOtp('')
      setOtpRequired(false)
      onSaved()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'OTP_REQUIRED') {
        setOtpRequired(true)
        setPassword('')
        setOtp('')
      } else if (err instanceof ApiError && err.code === 'OTP_SESSION_EXPIRED') {
        setOtpRequired(false)
        setOtp('')
        setError(err.message)
      } else if (err instanceof ApiError && err.code === 'OTP_INVALID') {
        setOtp('')
        setError(err.message)
      } else {
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog
      title={apple ? 'Apple Account 新通道登录' : configured ? 'iCloud 登录状态' : 'iCloud 登录'}
      open={open}
      onClose={() => {
        setPassword('')
        setOtp('')
        setOtpRequired(false)
        setError('')
        onClose()
      }}
    >
      {apple && <p className="hint">新通道使用独立的 Apple Account 会话，不覆盖 iCloud Web 旧通道。登录后自动续期；会话被拒绝时需重新登录。</p>}
      {error && (
        <div className="alert-error" role="alert">
          {error}
        </div>
      )}
      {configured && !otpRequired && !error && (
        <div className="alert-info">
          <strong>{apple ? '已保存 Apple Account 独立会话' : '已保存 iCloud Cookie'}</strong>
          <span className="alert-detail">保存过 Cookie 不代表当前会话有效；重新登录成功后才会更新。</span>
        </div>
      )}
      {otpRequired && (
        <div className="alert-info">该账号启用了双重认证，请输入验证码。</div>
      )}
      {!otpRequired && (
        <div className="form-field">
          {loginEmail && <p className="hint">iCloud 登录邮箱：{loginEmail}</p>}
          <label htmlFor="icloud-login-password">iCloud 登录密码</label>
          <input
            id="icloud-login-password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
          <p className="hint">填写此 iCloud 账号的登录密码，不是收件用的 App 专用密码或管理台密码。</p>
        </div>
      )}
      {otpRequired && (
        <div className="form-field">
          <label htmlFor="icloud-login-otp">验证码</label>
          <input
            id="icloud-login-otp"
            type="text"
            inputMode="numeric"
            maxLength={6}
            value={otp}
            onChange={(e) => setOtp(e.target.value.replace(/\D/g, ''))}
            autoComplete="one-time-code"
          />
        </div>
      )}
      <div className="form-actions">
        <button onClick={onClose}>取消</button>
        <button
          className="primary"
          onClick={() => void handleSubmit()}
          disabled={submitting || (otpRequired ? otp.length !== 6 : password.length === 0)}
        >
          {submitting ? '登录中…' : otpRequired ? '验证' : configured ? '重新登录' : '登录'}
        </button>
      </div>
    </Dialog>
  )
}
