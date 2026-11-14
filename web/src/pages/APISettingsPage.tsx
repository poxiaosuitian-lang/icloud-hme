import { useEffect, useState } from 'react'
import { ApiError, request } from '../api/client'
import ConfirmDialog from '../components/ConfirmDialog'
import { useToast } from '../components/ToastProvider'
import { copyText } from '../utils/clipboard'

interface Settings { enabled: boolean; has_token: boolean; base_path: string }
interface Usage { used_count: number; unique_email_count: number; callers: { caller_id: string; used_count: number }[] }

export default function APISettingsPage() {
  const [settings, setSettings] = useState<Settings | null>(null)
  const [token, setToken] = useState('')
  const [customToken, setCustomToken] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [confirm, setConfirm] = useState<'rotate' | 'save' | 'disable' | null>(null)
  const [retry, setRetry] = useState(0)
  const [callerID, setCallerID] = useState('vendor-a')
  const [usage, setUsage] = useState<Usage | null>(null)
  const [usageLoading, setUsageLoading] = useState(true)
  const [usageError, setUsageError] = useState('')
  const [usageRetry, setUsageRetry] = useState(0)
  const { show } = useToast()

  useEffect(() => {
    let cancelled = false
    request<Settings>('/api/settings/api').then((data) => {
      if (!cancelled) { setSettings(data); setError('') }
    }).catch((err) => { if (!cancelled) setError(err instanceof ApiError ? err.message : '读取设置失败') })
    return () => { cancelled = true }
  }, [retry])

  useEffect(() => {
    let cancelled = false
    request<Usage>('/api/settings/api/usage').then((data) => {
      if (!cancelled) { setUsage(data); setUsageError('') }
    }).catch((err) => {
      if (!cancelled) setUsageError(err instanceof ApiError ? err.message : '读取调用商统计失败')
    }).finally(() => { if (!cancelled) setUsageLoading(false) })
    return () => { cancelled = true }
  }, [usageRetry])

  async function action(kind: 'reveal' | 'rotate' | 'save' | 'toggle') {
    setBusy(true)
    setError('')
    try {
      if (kind === 'reveal') {
        const data = await request<{ token: string }>('/api/settings/api/token', { method: 'POST' })
        setToken(data.token)
      } else if (kind === 'rotate') {
        const data = await request<Settings & { token: string }>('/api/settings/api/rotate', { method: 'POST' })
        setSettings({ enabled: data.enabled, has_token: data.has_token, base_path: data.base_path })
        setToken(data.token)
        setCustomToken('')
        show('新 Token 已保存，旧 Token 已失效')
      } else {
        const data = await request<Settings>('/api/settings/api', {
          method: 'PUT',
          body: kind === 'save' ? { token: customToken } : { enabled: !settings?.enabled },
        })
        setSettings(data)
        setToken('')
        if (kind === 'save') setCustomToken('')
        show('API 设置已保存并立即生效')
      }
      setConfirm(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '操作失败，请重试')
      setConfirm(null)
    } finally { setBusy(false) }
  }

  const baseURL = `${window.location.origin}${settings?.base_path ?? '/api/v1'}`
  const normalizedCallerID = callerID.trim().toLowerCase() || 'default'
  const callerValid = /^[a-z0-9_.-]{1,64}$/.test(normalizedCallerID)
  const example = `# 先替换 TOKEN、ACCOUNT_ID，并为每个调用商设置固定的 CALLER_ID\nTOKEN='TOKEN'\nBASE_URL='${baseURL}'\nACCOUNT_ID='ACCOUNT_ID'\nCALLER_ID='${callerValid ? normalizedCallerID : 'CALLER_ID'}'\n\n# 查询账号 ID\ncurl "$BASE_URL/accounts" -H "Authorization: Bearer $TOKEN"\n\n# 分配别名：优先该调用商未用过的已有别名，耗尽后才创建；返回 data.email\ncurl "$BASE_URL/aliases" \\\n  -H "Authorization: Bearer $TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -d "{\\"account_id\\":\\"$ACCOUNT_ID\\",\\"caller_id\\":\\"$CALLER_ID\\",\\"label\\":\\"API 创建\\"}"\n\n# 查询该别名收到的邮件（INBOX + Junk）\ncurl -G "$BASE_URL/inbox" \\\n  -H "Authorization: Bearer $TOKEN" \\\n  --data-urlencode "account_id=$ACCOUNT_ID" \\\n  --data-urlencode 'alias=隐藏邮箱地址' \\\n  --data-urlencode 'limit=20' --data-urlencode 'days=7'\n\n# 读取正文（使用 IMAP 返回的邮件 ID）\ncurl -G "$BASE_URL/inbox/MESSAGE_ID" \\\n  -H "Authorization: Bearer $TOKEN" \\\n  --data-urlencode "account_id=$ACCOUNT_ID"`
  async function copy(value: string) { show(await copyText(value) ? '已复制' : '复制失败，请手动选择复制') }

  return <section>
    <div className="page-header"><div className="page-title">
      <h2>API 设置</h2><p>通过独立 Token 分配隐藏邮箱、查询收件箱和读取邮件正文。</p>
    </div></div>
    {error && <div className="alert-error" role="alert">{error} {!settings && <button onClick={() => setRetry((n) => n + 1)}>重试</button>}</div>}
    {!settings && !error && <p role="status">加载中…</p>}
    {settings && <>
      <div className="card api-settings-card">
        <h3>服务配置 <span className={settings.enabled ? 'badge badge-active' : 'badge badge-neutral'}>{settings.enabled ? '已启用' : '已停用'}</span></h3>
        <div className="form-field"><label htmlFor="api-base">API 地址</label>
          <input id="api-base" readOnly value={baseURL} />
        </div>
        <div className="row-actions">
          <button onClick={() => void copy(baseURL)}>复制地址</button>
          <button disabled={busy || (!settings.enabled && !settings.has_token)} onClick={() => settings.enabled ? setConfirm('disable') : void action('toggle')}>{settings.enabled ? '停用 API' : '启用 API'}</button>
        </div>
        <p className="hint">设置立即生效，保存至持久化数据目录，容器重启后仍保留。未设置 Token 时请先生成。</p>
      </div>
      <div className="card api-settings-card">
        <h3>访问密钥</h3>
        <p className="hint">{settings.has_token ? 'Token 已设置' : '尚未设置 Token'}。密钥可访问所有已配置账号的别名和邮件，请妥善保管；跨主机调用请使用 HTTPS。</p>
        <div className="form-field"><label htmlFor="api-token">当前 Token</label>
          <input id="api-token" type={token ? 'text' : 'password'} readOnly autoComplete="off" value={token || (settings.has_token ? '••••••••••••••••' : '')} />
        </div>
        <div className="row-actions">
          <button disabled={busy || !settings.has_token} onClick={() => token ? setToken('') : void action('reveal')}>{token ? '隐藏 Token' : '显示 Token'}</button>
          <button disabled={busy || !token} onClick={() => void copy(token)}>复制 Token</button>
          <button disabled={busy} onClick={() => settings.has_token ? setConfirm('rotate') : void action('rotate')}>{settings.has_token ? '重新生成 Token' : '生成 Token'}</button>
        </div>
        <div className="form-field" style={{ marginTop: 16 }}><label htmlFor="api-custom-token">自定义新 Token（可选）</label>
          <input id="api-custom-token" type="password" autoComplete="new-password" value={customToken} maxLength={512} onChange={(e) => setCustomToken(e.target.value)} placeholder="至少 32 字符，推荐使用随机生成" />
        </div>
        <button disabled={busy || !/^[\x21-\x7e]{32,512}$/.test(customToken)} onClick={() => setConfirm('save')}>保存自定义 Token</button>
        <p className="hint">更换后旧 Token 立即失效；生成 Token 不会自动启用已停用的 API。调用示例始终使用占位符，不包含真实密钥。</p>
      </div>
      <div className="card api-settings-card">
        <h3>调用商使用记录</h3>
        <p className="hint">按 caller_id 独立去重：A 用过的邮箱仍可分配给 B；同一调用商用完已有可用别名后才新建。未传 caller_id 或传 default 时使用默认分组，旧记录全部保留在此分组。</p>
        <p className="hint">caller_id 仅区分使用记录，不是独立权限；共用 Token 的调用方仍可访问所有已配置账号的别名和邮件。同一邮箱用于不同调用商时，收件箱可能包含多个调用商的邮件。</p>
        <button disabled={usageLoading} onClick={() => { setUsageLoading(true); setUsageRetry((n) => n + 1) }}>{usageLoading ? '读取统计中…' : '刷新调用商统计'}</button>
        {usageError && <p className="alert-error" role="alert">{usageError}</p>}
        {usage && <>
          <p className="hint">已用记录共 {usage.used_count} 条，涉及 {usage.unique_email_count} 个不同邮箱。</p>
          {usage.callers.length > 0 ? <div className="table-wrap"><table>
            <thead><tr><th>调用商标识</th><th>已用邮箱数</th></tr></thead>
            <tbody>{usage.callers.map((caller) => <tr key={caller.caller_id}>
              <td>{caller.caller_id === 'default' ? 'default（默认分组 / 旧调用）' : caller.caller_id}</td><td>{caller.used_count}</td>
            </tr>)}</tbody>
          </table></div> : <p className="hint">暂无调用商使用记录</p>}
        </>}
      </div>
      <div className="card api-settings-card">
        <h3>接口与调用示例</h3>
        <ul>
          <li><code>GET /accounts</code>：查询账号 ID</li>
          <li><code>GET /aliases?account_id=ACCOUNT_ID</code>：查询别名列表</li>
          <li><code>POST /aliases</code>：传 caller_id，优先取用该调用商未用过的已有别名，耗尽后按需创建</li>
          <li><code>GET /aliases/usage?caller_id=CALLER_ID</code>：查询该调用商的占用数量；POST 同路径携带 caller_id 可导入对应历史记录</li>
          <li><code>GET /inbox</code>：按账号、别名查询邮件摘要（INBOX + Junk）</li>
          <li><code>GET /inbox/MESSAGE_ID</code>：读取正文（需配置 IMAP / App 密码）</li>
        </ul>
        <p className="hint">请求头：Authorization: Bearer TOKEN，无需网页登录或 CSRF。建议每 10–30 秒查询收件，按邮件 ID 去重；可传 request_id；同调用商、同 ID、同参数重试返回原分配结果。不同调用商可使用相同 request_id。上游创建结果不明时先检查别名列表。</p>
        <p className="hint">占用记录按调用商由服务统一保存，任务失败、重启或更换 Token 后，同一调用商不会重复取用；网页手动创建和批量创建仍始终新建。</p>
        <p className="hint">limit：1–100，默认 20；days：1–90，默认 7。IMAP 与 Web API 回退均合并 INBOX + Junk，先筛选再按时间倒序取总 limit。Web API 别名筛选仅覆盖有限批次摘要，精确收件人查询请配置 IMAP。</p>
        <p className="hint">folder 表示来源文件夹；垃圾邮件 ID 带 Junk: 前缀，请保留完整字符串并在详情 URL 中编码（例如 Junk%3A42）。正文接口仅使用 IMAP 返回的 ID。</p>
        <div className="form-field"><label htmlFor="api-caller-id">调用商标识（用于生成示例）</label>
          <input id="api-caller-id" value={callerID} maxLength={64} onChange={(event) => setCallerID(event.target.value)} placeholder="例如 vendor-a、vendor-b；留空为 default" aria-invalid={!callerValid} />
          <p className="hint">每个调用商固定使用不同标识，支持 1–64 个字母、数字、连字符、下划线和点，忽略大小写。此输入只生成示例，实际调用程序需发送相同的 caller_id。</p>
          {!callerValid && <p className="alert-error" role="alert">调用商标识格式不正确</p>}
        </div>
        <button disabled={!callerValid} onClick={() => void copy(example)}>复制调用示例</button>
        <pre className="api-code"><code>{example}</code></pre>
      </div>
    </>}
    {confirm && <ConfirmDialog open title={confirm === 'disable' ? '停用 API' : '更换 API Token'}
      message={confirm === 'disable' ? '停用后所有程序 API 调用将被阻止，网页管理不受影响。' : '旧 Token 将立即失效，请同步更新调用程序中的密钥。'}
      confirmLabel={confirm === 'disable' ? '确认停用' : '确认更换'} busy={busy}
      onClose={() => { if (!busy) setConfirm(null) }} onConfirm={() => void action(confirm === 'disable' ? 'toggle' : confirm)} />}
  </section>
}
