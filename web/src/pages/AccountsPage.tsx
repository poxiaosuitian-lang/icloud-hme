import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { request, ApiError } from '../api/client'
import type { AccountSummary } from '../api/types'
import AsyncState from '../components/AsyncState'
import AccountFormDialog from '../components/AccountFormDialog'
import CookieDialog from '../components/CookieDialog'
import ICloudLoginDialog from '../components/ICloudLoginDialog'
import AppPasswordDialog from '../components/AppPasswordDialog'
import ProxyDialog from '../components/ProxyDialog'
import MailboxDialog from '../components/MailboxDialog'
import ConfirmDialog from '../components/ConfirmDialog'
import { useToast } from '../components/ToastProvider'
import {
  IconCheck,
  IconClock,
  IconAlert,
  IconPlus,
  IconEdit,
  IconTrash,
  IconKey,
  IconMail,
  IconAliases,
  IconAccounts,
} from '../components/icons'

const statusMeta: Record<string, { text: string; badge: string; icon: typeof IconCheck }> = {
  active: { text: '正常', badge: 'badge badge-active', icon: IconCheck },
  pending: { text: '待配置', badge: 'badge badge-pending', icon: IconClock },
  error: { text: '异常', badge: 'badge badge-error', icon: IconAlert },
}

function StatusBadge({ status }: { status: string }) {
  const meta = statusMeta[status] ?? {
    text: status,
    badge: 'badge badge-neutral',
    icon: IconClock,
  }
  const Icon = meta.icon
  return (
    <span className={meta.badge}>
      <Icon />
      {meta.text}
    </span>
  )
}

function credText(acc: AccountSummary): string {
  const parts: string[] = []
  if (acc.has_cookies) parts.push('Cookie')
  if (acc.has_app_password) parts.push('App 密码')
  if (acc.mailbox) parts.push(`收件箱:${acc.mailbox.email}`)
  if (acc.has_proxy) parts.push('代理')
  return parts.length > 0 ? `已配置（${parts.join('·')}）` : '未配置'
}

export default function AccountsPage() {
  const [accounts, setAccounts] = useState<AccountSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [retryKey, setRetryKey] = useState(0)

  // dialog 状态
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<AccountSummary | null>(null)
  const [cookieFor, setCookieFor] = useState<AccountSummary | null>(null)
  const [appleLoginFor, setAppleLoginFor] = useState<AccountSummary | null>(null)
  const [loginFor, setLoginFor] = useState<AccountSummary | null>(null)
  const [appPwdFor, setAppPwdFor] = useState<AccountSummary | null>(null)
  const [proxyFor, setProxyFor] = useState<AccountSummary | null>(null)
  const [mailboxFor, setMailboxFor] = useState<AccountSummary | null>(null)
  const [deleteFor, setDeleteFor] = useState<AccountSummary | null>(null)
  const [deleting, setDeleting] = useState(false)

  const { show } = useToast()

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const data = await request<AccountSummary[]>('/api/accounts')
      setAccounts(data)
      setError('')
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '网络连接失败，请检查服务状态')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    request<AccountSummary[]>('/api/accounts')
      .then((data) => {
        if (cancelled) return
        setAccounts(data)
        setError('')
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
  }, [retryKey])

  function handleRetry() {
    setLoading(true)
    setRetryKey((k) => k + 1)
  }

  async function handleDelete() {
    if (!deleteFor) return
    setDeleting(true)
    try {
      await request(`/api/accounts/${deleteFor.id}`, { method: 'DELETE' })
      setDeleteFor(null)
      show('账号已删除')
      void load()
    } catch (err) {
      show(err instanceof ApiError ? err.message : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  return (
    <section className="accounts-page">
      <div className="page-header">
        <div className="page-title">
          <h2>账号管理</h2>
          <p>统一管理账号连接、登录凭据与收件服务</p>
        </div>
        <button
          className="primary"
          onClick={() => {
            setEditing(null)
            setFormOpen(true)
          }}
        >
          <IconPlus size={16} />
          添加账号
        </button>
      </div>

      {!loading && !error && accounts.length > 0 && (
        <dl className="accounts-overview" aria-label="账号概览">
          <div><dt>全部账号</dt><dd>{accounts.length}<span>个账号</span></dd></div>
          <div><dt>运行正常</dt><dd>{accounts.filter((acc) => acc.status === 'active').length}<span>个账号</span></dd></div>
          <div><dt>新通道会话</dt><dd>{accounts.filter((acc) => acc.has_apple_session).length}<span>个会话</span></dd></div>
          <div><dt>启用别名</dt><dd>{accounts.reduce((sum, acc) => sum + acc.alias_active, 0)}<span>个别名</span></dd></div>
        </dl>
      )}

      <AsyncState
        loading={loading}
        error={error}
        empty={accounts.length === 0}
        emptyText="暂无账号，点击“添加账号”开始"
        onRetry={handleRetry}
      >
        <div className="account-card-list">
          {accounts.map((acc) => (
            <article className="account-card" key={acc.id} aria-labelledby={`account-title-${acc.id}`}>
              <div className="account-card-body">
                <div className="account-identity">
                  <span className="account-avatar" aria-hidden="true"><IconAccounts size={24} /></span>
                  <div className="account-identity-text">
                    <div className="account-title-line">
                      <h3 id={`account-title-${acc.id}`}>{acc.name}</h3>
                      <StatusBadge status={acc.status} />
                      <span className="account-region">{acc.host === 'icloud.com.cn' ? '中国区' : '全球区'}</span>
                    </div>
                    <p className="account-email">{acc.icloud_email || acc.real_email || '未设置邮箱'}</p>
                    <span className="account-id">{acc.id}</span>
                  </div>
                </div>
                <dl className="account-metrics">
                  <div><dt>启用 / 全部别名</dt><dd>{acc.alias_active} / {acc.alias_total}</dd></div>
                  <div><dt>最近验证</dt><dd className="account-verified-at">
                    {acc.last_validated && !Number.isNaN(Date.parse(acc.last_validated))
                      ? <time dateTime={acc.last_validated} title={acc.last_validated}>{new Date(acc.last_validated).toLocaleString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false })}</time>
                      : '尚未验证'}
                  </dd></div>
                </dl>
                <div className="account-configuration">
                  <span className="account-configuration-label">凭据配置</span>
                  <p>{credText(acc)}</p>
                  {acc.status_message && <p className="account-status-note">{acc.status_message}</p>}
                </div>
              </div>

              <div className="account-operations">
                <div className="account-action-section" role="group" aria-label={`${acc.name}的登录与凭据`}>
                  <h4>登录与凭据</h4>
                  <div className="account-action-grid">
                    <button
                      className={acc.has_cookies ? 'configured' : ''}
                      title={acc.has_cookies ? '当前已登录；点击可重新登录并刷新 Cookie' : '使用 Apple 账户密码登录'}
                      onClick={() => setLoginFor(acc)}
                    >
                      {acc.has_cookies ? <IconCheck size={16} /> : <IconKey size={16} />}
                      <span>{acc.has_cookies ? 'iCloud 已登录' : 'iCloud 登录'}</span>
                    </button>
                    <button onClick={() => setAppleLoginFor(acc)} className={acc.has_apple_session ? 'configured' : ''}>
                      {acc.has_apple_session ? <IconCheck size={16} /> : <IconKey size={16} />}
                      <span>{acc.has_apple_session ? '新通道会话已保存' : '登录新通道'}</span>
                    </button>
                    <button onClick={() => setCookieFor(acc)}><IconEdit size={16} /><span>更新 Cookie</span></button>
                    <button
                      className={acc.has_app_password ? 'configured' : ''}
                      title={acc.has_app_password ? '当前已设置；点击可更新 App 专用密码' : '设置用于 IMAP 收件的 App 专用密码'}
                      onClick={() => setAppPwdFor(acc)}
                    >
                      {acc.has_app_password ? <IconCheck size={16} /> : <IconKey size={16} />}
                      <span>{acc.has_app_password ? 'App 密码已设置' : '设置 App 密码'}</span>
                    </button>
                    <button onClick={() => setProxyFor(acc)}><IconAccounts size={16} /><span>设置代理</span></button>
                  </div>
                </div>
                <div className="account-action-section" role="group" aria-label={`${acc.name}的账号与邮箱`}>
                  <h4>账号与邮箱</h4>
                  <div className="account-action-grid">
                    <button onClick={() => setMailboxFor(acc)}><IconMail size={16} /><span>接入收件邮箱</span></button>
                    <Link to={`/aliases?account_id=${encodeURIComponent(acc.id)}`}><IconAliases size={16} /><span>别名</span></Link>
                    <Link to={`/inbox?account_id=${encodeURIComponent(acc.id)}`}><IconMail size={16} /><span>收件箱</span></Link>
                    <button onClick={() => { setEditing(acc); setFormOpen(true) }}><IconEdit size={16} /><span>编辑</span></button>
                    <button className="danger" onClick={() => setDeleteFor(acc)}><IconTrash size={16} /><span>删除</span></button>
                  </div>
                </div>
              </div>
            </article>
          ))}
        </div>
      </AsyncState>

      <AccountFormDialog
        open={formOpen}
        onClose={() => setFormOpen(false)}
        onSaved={() => {
          setFormOpen(false)
          show('账号已保存')
          void load()
        }}
        editing={editing ? { id: editing.id, name: editing.name, icloudEmail: editing.icloud_email, host: editing.host } : null}
      />
      {cookieFor && (
        <CookieDialog
          accountId={cookieFor.id}
          open
          onClose={() => setCookieFor(null)}
          onSaved={() => {
            show('Cookie 已更新')
            void load()
          }}
        />
      )}
      {appleLoginFor && (
        <ICloudLoginDialog accountId={appleLoginFor.id} channel="apple_account"
          loginEmail={appleLoginFor.icloud_email?.trim() || appleLoginFor.real_email?.trim()}
          configured={appleLoginFor.has_apple_session} open
          onClose={() => setAppleLoginFor(null)}
          onSaved={() => { setAppleLoginFor(null); void load() }} />
      )}
      {loginFor && (
        <ICloudLoginDialog
          accountId={loginFor.id}
          loginEmail={loginFor.icloud_email?.trim() || loginFor.real_email?.trim()}
          configured={loginFor.has_cookies}
          open
          onClose={() => setLoginFor(null)}
          onSaved={() => {
            setLoginFor(null)
            show('登录成功')
            void load()
          }}
        />
      )}
      {appPwdFor && (
        <AppPasswordDialog
          accountId={appPwdFor.id}
          configured={appPwdFor.has_app_password}
          initialEmail={appPwdFor.icloud_email}
          open
          onClose={() => setAppPwdFor(null)}
          onSaved={() => {
            setAppPwdFor(null)
            show('App 专用密码已设置')
            void load()
          }}
        />
      )}
      {proxyFor && (
        <ProxyDialog
          accountId={proxyFor.id}
          open
          onClose={() => setProxyFor(null)}
          onSaved={() => {
            show('代理已更新')
            void load()
          }}
        />
      )}
      {mailboxFor && (
        <MailboxDialog
          accountId={mailboxFor.id}
          current={mailboxFor.mailbox}
          open
          onClose={() => setMailboxFor(null)}
          onSaved={() => { setMailboxFor(null); show('收件邮箱已接入'); void load() }}
        />
      )}
      {deleteFor && (
        <ConfirmDialog
          title="删除账号"
          message={`将移除本地账号配置「${deleteFor.name}」，不会删除 Apple 账号本身。`}
          requireText={deleteFor.name}
          open
          busy={deleting}
          onClose={() => setDeleteFor(null)}
          onConfirm={() => void handleDelete()}
        />
      )}
    </section>
  )
}
