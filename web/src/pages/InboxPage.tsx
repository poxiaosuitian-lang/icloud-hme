import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { request, ApiError } from '../api/client'
import type { AccountSummary, Alias, FullMessage, InboxResult, InboxMessage } from '../api/types'
import AsyncState from '../components/AsyncState'
import Dialog from '../components/Dialog'
import ConfirmDialog from '../components/ConfirmDialog'
import MailBody from '../components/MailBody'
import { useToast } from '../components/ToastProvider'
import { IconKey, IconMail, IconTrash } from '../components/icons'

function formatDate(raw: string): string {
  const d = new Date(raw)
  if (Number.isNaN(d.getTime())) return raw
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).format(d)
}

export default function InboxPage() {
  const [accounts, setAccounts] = useState<AccountSummary[]>([])
  const [aliases, setAliases] = useState<Alias[]>([])
  const [accountId, setAccountId] = useState('')
  const [alias, setAlias] = useState('')
  const [limit, setLimit] = useState(20)
  const [days, setDays] = useState(7)

  const [result, setResult] = useState<InboxResult | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [retryKey, setRetryKey] = useState(0)
  const [detail, setDetail] = useState<FullMessage | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [deleteFor, setDeleteFor] = useState<InboxMessage | null>(null)
  const [deleting, setDeleting] = useState(false)

  const [searchParams, setSearchParams] = useSearchParams()
  const abortRef = useRef<AbortController | null>(null)
  const detailAbortRef = useRef<AbortController | null>(null)
  const { show } = useToast()

  useEffect(() => () => detailAbortRef.current?.abort(), [])

  function closeMessage() {
    detailAbortRef.current?.abort()
    setDetail(null)
    setDetailLoading(false)
  }

  async function openMessage(message: InboxMessage) {
    detailAbortRef.current?.abort()
    const controller = new AbortController()
    detailAbortRef.current = controller
    setDetail(null)
    setDetailLoading(true)
    try {
      const data = await request<FullMessage>(`/api/inbox/${encodeURIComponent(message.id)}?account_id=${encodeURIComponent(accountId)}`, { signal: controller.signal })
      if (!controller.signal.aborted) setDetail(data)
    } catch (err) {
      if (!controller.signal.aborted) show(err instanceof ApiError ? err.message : '读取邮件详情失败')
    } finally {
      if (!controller.signal.aborted) setDetailLoading(false)
    }
  }

  async function deleteMessage() {
    if (!deleteFor) return
    setDeleting(true)
    try {
      await request(`/api/inbox/${encodeURIComponent(deleteFor.id)}?account_id=${encodeURIComponent(accountId)}`, { method: 'DELETE' })
      setDeleteFor(null)
      setDetail(null)
      show('邮件已删除')
      setRetryKey((key) => key + 1)
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除邮件失败')
    } finally {
      setDeleting(false)
    }
  }

  // 加载账号列表并初始化筛选状态(只保存 account_id/alias/limit/days)
  useEffect(() => {
    let cancelled = false
    request<AccountSummary[]>('/api/accounts')
      .then((data) => {
        if (cancelled) return
        setAccounts(data)
        const queryId = searchParams.get('account_id')
        const valid = data.find((a) => a.id === queryId)
        const target = valid ? valid.id : data[0]?.id ?? ''
        setAccountId(target)
        if (target) {
          const next: Record<string, string> = { account_id: target }
          const qAlias = searchParams.get('alias')
          if (qAlias) {
            setAlias(qAlias)
            next.alias = qAlias
          }
          const qLimit = searchParams.get('limit')
          if (qLimit) next.limit = qLimit
          const qDays = searchParams.get('days')
          if (qDays) next.days = qDays
          setSearchParams(next, { replace: true })
        }
      })
      .catch((err) => {
        if (cancelled) return
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 账号变化时加载别名列表(供筛选)
  useEffect(() => {
    if (!accountId) return
    let cancelled = false
    request<{ account_id: string; count: number; aliases: Alias[] }>(
      `/api/aliases?account_id=${encodeURIComponent(accountId)}`,
    )
      .then((data) => {
        if (cancelled) return
        setAliases(data.aliases ?? [])
      })
      .catch(() => {
        if (cancelled) return
        setAliases([])
      })
    return () => {
      cancelled = true
    }
  }, [accountId])

  // 查询收件箱;账号变化时清空旧邮件并中止旧请求
  useEffect(() => {
    if (!accountId) return
    abortRef.current?.abort()
    const controller = new AbortController()
    abortRef.current = controller
    let cancelled = false
    const params = new URLSearchParams({ account_id: accountId })
    if (alias) params.set('alias', alias)
    params.set('limit', String(limit))
    params.set('days', String(days))
    request<InboxResult>(`/api/inbox?${params.toString()}`, {
      signal: controller.signal,
    })
      .then((data) => {
        if (cancelled) return
        // 兼容旧版后端曾在空收件箱返回 messages: null 的响应，避免
        // 选择尚未收到邮件的新别名时因读取 .length 导致整个页面空白。
        setResult({
          ...data,
          messages: Array.isArray(data.messages) ? data.messages : [],
        })
        setError('')
      })
      .catch((err) => {
        if (cancelled || err instanceof ApiError && err.status === 0) return
        setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
        setResult(null)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
      controller.abort()
    }
  }, [accountId, alias, limit, days, retryKey])

  const qAlias = useMemo(() => alias, [alias])

  function handleSearch() {
    const next: Record<string, string> = { account_id: accountId }
    if (qAlias) next.alias = qAlias
    next.limit = String(limit)
    next.days = String(days)
    setSearchParams(next, { replace: true })
    setRetryKey((k) => k + 1)
  }

  function handleAccountChange(id: string) {
    closeMessage()
    setAccountId(id)
    setAlias('')
    setResult(null)
    setSearchParams({ account_id: id }, { replace: true })
  }

  const methodText = result?.method === 'imap' ? 'IMAP' : 'Web API'

  return (
    <section>
      <div className="page-header">
        <div className="page-title">
          <h2>收件箱</h2>
          <p>点击邮件所在行查看详情；IMAP 合并收件箱与垃圾邮件</p>
        </div>
      </div>

      <div className="card list-toolbar inbox-toolbar">
          <div className="toolbar-field inbox-account-field">
            <label htmlFor="inbox-account">账号</label>
            <select
              id="inbox-account"
              value={accountId}
              onChange={(e) => handleAccountChange(e.target.value)}
            >
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
          </div>
          <div className="toolbar-field inbox-alias-field">
            <label htmlFor="inbox-alias">别名</label>
            <select
              id="inbox-alias"
              value={alias}
              onChange={(e) => setAlias(e.target.value)}
            >
              <option value="">全部</option>
              {aliases.map((a) => (
                <option key={a.anonymousId} value={a.email}>
                  {a.email}
                </option>
              ))}
            </select>
          </div>
          <div className="toolbar-field">
            <label htmlFor="inbox-limit">每页</label>
            <select
              id="inbox-limit"
              value={limit}
              onChange={(e) => setLimit(Number(e.target.value))}
            >
              <option value={1}>1</option>
              <option value={20}>20</option>
              <option value={100}>100</option>
            </select>
          </div>
          <div className="toolbar-field">
            <label htmlFor="inbox-days">时间范围</label>
            <select
              id="inbox-days"
              value={days}
              onChange={(e) => setDays(Number(e.target.value))}
            >
              <option value={1}>1 天</option>
              <option value={7}>7 天</option>
              <option value={30}>30 天</option>
              <option value={90}>90 天</option>
            </select>
          </div>
            <button className="primary" onClick={handleSearch}>
              查询
            </button>
      </div>

      <AsyncState
        loading={loading}
        error={error}
        empty={!result || result.messages.length === 0}
        emptyText="暂无邮件"
        onRetry={() => {
          setLoading(true)
          setRetryKey((k) => k + 1)
        }}
      >
        {result && result.messages.length > 0 && (
          <>
            <p className="hint" style={{ marginBottom: 8, display: 'flex', alignItems: 'center', gap: 8 }}>
              <span>共 {result.count} 封</span>
              <span className={result.method === 'imap' ? 'badge badge-info' : 'badge badge-neutral'}>
                {result.method === 'imap' ? <IconKey size={12} /> : <IconMail size={12} />}
                读取方式：{methodText}
              </span>
            </p>
            <div className="table-wrap">
              <table className="compact-table inbox-table">
                <colgroup>
                  <col />
                  <col className="status-column" />
                  <col className="mail-address-column" />
                  <col className="mail-address-column" />
                  <col className="date-column" />
                  <col className="mail-actions-column" />
                </colgroup>
                <thead>
                  <tr>
                    <th>主题</th>
                    <th>来源</th>
                    <th>发件人</th>
                    <th>收件人</th>
                    <th>日期</th>
                    <th><span className="visually-hidden">操作</span></th>
                  </tr>
                </thead>
                <tbody>
                  {result.messages.map((m) => (
                    <tr key={m.id} className="mail-row" onClick={() => void openMessage(m)}>
                      <td><button className="link-button cell-ellipsis" title={m.subject || '（无主题）'} aria-haspopup="dialog" onClick={(event) => { event.stopPropagation(); void openMessage(m) }}>{m.subject || '（无主题）'}</button></td>
                      <td><span className={m.folder === 'Junk' ? 'badge badge-neutral' : 'badge badge-info'}>{m.folder === 'Junk' ? '垃圾邮件' : '收件箱'}</span></td>
                      <td><span className="cell-ellipsis" title={m.from}>{m.from}</span></td>
                      <td><span className="cell-ellipsis" title={m.to}>{m.to}</span></td>
                      <td className="date-cell">{formatDate(m.date)}</td>
                      <td><button className="icon-button mail-delete-button" aria-label="删除邮件" title="删除邮件" onClick={(event) => { event.stopPropagation(); setDeleteFor(m) }}><IconTrash size={14} /></button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </AsyncState>
      <Dialog title={detail?.subject || '邮件详情'} open={detail !== null || detailLoading} onClose={closeMessage}>
        {detailLoading && <p className="hint">读取中…</p>}
        {detail && <>
          <p className="hint">来源：{detail.folder === 'Junk' ? '垃圾邮件' : '收件箱'}</p>
          <p className="hint">发件人：{detail.from}</p>
          <p className="hint">收件人：{detail.to}</p>
          <p className="hint">日期：{formatDate(detail.date)}</p>
          <MailBody body={detail.body || '无正文'} />
        </>}
        <div className="form-actions">
          {detail && <button className="danger" onClick={() => setDeleteFor(detail)}>删除邮件</button>}
          <button onClick={closeMessage}>关闭</button>
        </div>
      </Dialog>
      {deleteFor && <ConfirmDialog title="删除邮件" message={`邮件将从${deleteFor.folder === 'Junk' ? '垃圾邮件' : '收件箱'}中永久删除。`} open busy={deleting} onClose={() => setDeleteFor(null)} onConfirm={() => void deleteMessage()} />}
    </section>
  )
}
