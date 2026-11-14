import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, request } from '../api/client'
import type { AliasBatchStatus, CreationChannel } from '../api/types'
import Dialog from './Dialog'
import { IconPlus } from './icons'

const MAX_ALIASES = 750
const statusLabels: Record<AliasBatchStatus['status'], string> = {
  running: '正在自动注册',
  cooling_down: '冷却中，到期自动继续',
  stopping: '正在停止',
  completed: '注册完成',
  stopped: '已停止',
  failed: '注册失败，任务已停止',
  limit_reached: '已达到 750 个上限',
}

interface Props {
  accountId: string
  aliasTotal: number
  onAliasesChanged: () => void
}

export default function AutoAliasRegistration({ accountId, aliasTotal, onAliasesChanged }: Props) {
  const [channel, setChannel] = useState<CreationChannel>('icloud_web')
  const [open, setOpen] = useState(false)
  const [count, setCount] = useState('10')
  const [interval, setInterval] = useState('10')
  const [cooldownMinutes, setCooldownMinutes] = useState('60')
  const [labelPrefix, setLabelPrefix] = useState('自动注册')
  const [batch, setBatch] = useState<AliasBatchStatus | null>(null)
  const [statusLoading, setStatusLoading] = useState(true)
  const [error, setError] = useState('')
  const [pollError, setPollError] = useState('')
  const [busy, setBusy] = useState(false)
  const [stopPending, setStopPending] = useState(false)
  const [refreshKey, setRefreshKey] = useState(0)
  const lastStatus = useRef<string | null>(null)
  const version = useRef(0)
  const canStop = batch?.status === 'running' || batch?.status === 'cooling_down'
  const active = canStop || batch?.status === 'stopping'
  const remaining = Math.max(0, MAX_ALIASES - aliasTotal)

  const applyBatch = useCallback((next: AliasBatchStatus | null) => {
    const signature = next ? `${next.id}:${next.created}:${next.status}` : ''
    if (lastStatus.current !== null && lastStatus.current !== signature) onAliasesChanged()
    lastStatus.current = signature
    setBatch(next)
  }, [onAliasesChanged])

  // Recover server-side jobs on page load, and poll without overlapping requests.
  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout>
    const controller = new AbortController()
    async function poll() {
      const currentVersion = version.current
      try {
        const next = await request<AliasBatchStatus | null>(
          `/api/alias-batch?account_id=${encodeURIComponent(accountId)}`,
          { signal: controller.signal },
        )
        if (cancelled || currentVersion !== version.current) return
        applyBatch(next)
        setPollError('')
      } catch (err) {
        if (cancelled || currentVersion !== version.current) return
        setPollError(err instanceof ApiError ? err.message : '任务状态读取失败')
      } finally {
        if (!cancelled) {
          setStatusLoading(false)
          timer = setTimeout(() => void poll(), 2000)
        }
      }
    }
    void poll()
    return () => {
      cancelled = true
      clearTimeout(timer)
      controller.abort()
    }
  }, [accountId, applyBatch, refreshKey])

  function openSettings() {
    setCount(String(Math.min(10, remaining)))
    setError('')
    setOpen(true)
  }

  async function start() {
    if (busy || active) return
    const quantity = Number(count)
    const seconds = channel === 'auto' ? 120 : Number(interval)
    const cooldown = channel === 'auto' ? 60 : Number(cooldownMinutes)
    if (!Number.isInteger(quantity) || quantity < 1 || quantity > remaining) {
      setError(`本次注册数量需为 1-${remaining} 的整数`)
      return
    }
    if (!Number.isFinite(seconds) || seconds < 1 || seconds > 86400) {
      setError('每个间隔需为 1-86400 秒')
      return
    }
    if (!Number.isInteger(cooldown) || cooldown < 1 || cooldown > 1440) {
      setError('冷却时长需为 1-1440 分钟的整数')
      return
    }
    setBusy(true)
    setError('')
    version.current++
    try {
      const next = await request<AliasBatchStatus>('/api/alias-batch', {
        method: 'POST',
        body: { channel, account_id: accountId, count: quantity, interval_seconds: seconds, cooldown_minutes: cooldown, label_prefix: labelPrefix.trim() },
      })
      applyBatch(next)
      setOpen(false)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '自动注册启动失败')
    } finally {
      setBusy(false)
      setRefreshKey((key) => key + 1)
    }
  }

  async function stop() {
    if (busy || !batch || !canStop) return
    setBusy(true)
    setStopPending(true)
    setError('')
    version.current++
    try {
      const next = await request<AliasBatchStatus>('/api/alias-batch/stop', {
        method: 'POST', body: { account_id: accountId, job_id: batch.id },
      })
      applyBatch(next)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '停止任务失败')
    } finally {
      setBusy(false)
      setStopPending(false)
      setRefreshKey((key) => key + 1)
    }
  }

  return (
    <div className="card alias-batch-card">
      <div className="alias-batch-header">
        <div>
          <h3>自动注册别名</h3>
          <p className="hint">当前 {aliasTotal} / {MAX_ALIASES} 个（含已停用别名），还可注册 {remaining} 个。</p>
        </div>
        <div className="alias-batch-actions">
          <button className="primary" disabled={busy || active || statusLoading || !!pollError || remaining === 0} onClick={openSettings}>
            <IconPlus size={16} /> 自动注册别名
          </button>
          <button
            className="danger"
            disabled={busy || statusLoading || !canStop}
            aria-busy={stopPending || batch?.status === 'stopping'}
            onClick={() => void stop()}
          >
            {stopPending || batch?.status === 'stopping' ? '正在停止…' : '停止注册任务'}
          </button>
        </div>
      </div>
      {batch && (
        <div className="alias-batch-progress" role="status" aria-live="polite">
          <strong>{statusLabels[batch.status]}</strong>
          <span>本次已注册 {batch.created} / {batch.count} 个；{batch.channel === 'auto' ? '按小时错峰计划运行。' : `每个间隔 ${batch.interval_seconds} 秒。`}</span>
          {batch.channel === 'auto' ? <>
            <span>每小时 23 个计划时隙：新通道 19 个、旧通道 4 个；最小间隔 120 秒，不补发错过的时隙。</span>
            <span>当前：{batch.schedule_mode === 'web_only' ? '降级旧通道，每小时 5 个' : batch.schedule_mode === 'offline' ? '等待旧通道登录，保留计划' : '19+4 双通道计划'}{batch.schedule_slot ? `；下一个时隙 ${batch.schedule_slot}/23` : ''}。</span>
          </> : <span>每批 {batch.batch_size ?? 5} 个，冷却 {batch.cooldown_minutes ?? 60} 分钟后自动继续剩余任务。</span>}
          {batch.channel && <span>创建模式：{batch.channel === 'auto' ? '新旧双通道' : batch.channel === 'apple_account' ? 'Apple Account 新通道' : 'iCloud Web 旧通道'}</span>}
          {batch.channels?.map((item) => <span key={item.channel}>
            {item.channel === 'apple_account' ? '新通道' : '旧通道'}：已创建 {item.created} 个
            {item.disabled ? '；需要重新登录或处理账号状态' : item.next_run_at ? `；冷却至 ${new Date(item.next_run_at).toLocaleString()}` : '；可用'}
            {item.message && `；${item.message}`}
          </span>)}
          <progress aria-label="自动注册进度" value={batch.created} max={batch.count} />
          {batch.last_email && <span>最新别名：{batch.last_email}</span>}
          {batch.next_run_at && <span>{batch.status === 'cooling_down' ? '冷却结束：' : '下次注册：'}{new Date(batch.next_run_at).toLocaleString()}</span>}
          {!!batch.cooldown_count && <span>已进入冷却 {batch.cooldown_count} 次；剩余 {Math.max(0, batch.count - batch.created)} 个。</span>}
          {batch.message && <span>{batch.message}</span>}
          {active && <span className="hint">关闭网页仍会继续；停止后不再发起新注册，正在执行的一次可能仍会完成。</span>}
        </div>
      )}
      {pollError && <p className="alert-error" role="alert">{pollError}，正在重新读取任务状态…</p>}
      {!open && error && <p className="alert-error" role="alert">{error}</p>}
      <Dialog title="自动注册别名" open={open} onClose={() => { if (!busy) setOpen(false) }}>
        <p className="hint">每个账号最多 {MAX_ALIASES} 个别名；当前剩余额度 {remaining} 个。</p>
        {error && <p className="alert-error" role="alert">{error}</p>}
        <div className="form-field">
          <label htmlFor="batch-channel">创建通道</label>
          <select id="batch-channel" value={channel} disabled={busy} onChange={(event) => setChannel(event.target.value as CreationChannel)}>
            <option value="icloud_web">旧通道 · iCloud Web</option>
            <option value="apple_account">新通道 · Apple Account</option>
            <option value="auto">新旧双通道 · 原项目 19+4 小时计划</option>
          </select>
          <p className="hint">新通道需先在账号页面独立登录；创建时需要有效的 iCloud Web 会话核对总数。19+4 是原项目的本地调度上限，不是 Apple 公布或保证的额度。</p>
          {channel === 'auto' && <p className="hint">一小时随机分布 23 个时隙，每次至少间隔 2 分钟，首次也等待计划时间。新通道未登录或失效时降级为旧通道 5 个分散时隙；恢复登录后自动切回双通道。不会把冷却通道的时隙转给另一通道。</p>}
        </div>
        <div className="form-field">
          <label htmlFor="batch-count">本次注册数量</label>
          <input id="batch-count" type="number" min={1} max={remaining} step={1} value={count}
            disabled={busy} onChange={(event) => setCount(event.target.value)} />
        </div>
        {channel !== 'auto' && <>
        <div className="form-field">
          <label htmlFor="batch-interval">每个间隔（秒）</label>
          <input id="batch-interval" type="number" min={1} max={86400} step="any" value={interval}
            disabled={busy} onChange={(event) => setInterval(event.target.value)} />
          <p className="hint">第一项立即开始，后续每项在上一项完成后等待指定秒数，按顺序注册。</p>
        </div>
        <div className="form-field">
          <label htmlFor="batch-cooldown">冷却时长（分钟）</label>
          <input id="batch-cooldown" type="number" min={1} max={1440} step={1} value={cooldownMinutes}
            disabled={busy} onChange={(event) => setCooldownMinutes(event.target.value)} />
          <p className="hint">默认 60 分钟。每个通道成功创建 5 个后独立暂停，到期自动继续；最后一批完成即结束。批次之间也遵守每个间隔设置。</p>
        </div>
        </>}
        <div className="form-field">
          <label htmlFor="batch-prefix">标签前缀（可选）</label>
          <input id="batch-prefix" value={labelPrefix} maxLength={170} disabled={busy}
            onChange={(event) => setLabelPrefix(event.target.value)} placeholder="自动注册" />
          <p className="hint">自动附加任务编号和序号；留空使用「自动注册」。</p>
        </div>
        {channel === 'auto' ? <p className="hint">限流默认冷却至少 60 分钟，尊重更长的 Retry-After；单通道限流不推迟另一通道计划，公共请求限流则整体等待。滚动小时预算、计划和进度会保存，容器重启后继续；重启前正在创建且结果不确定时暂停核对。手动停止不会自动恢复。</p> : <p className="hint">遇到限流或临时错误也按设定时长冷却，连续失败逐次加倍至 120 分钟（设置超过 120 分钟则保留原值）；上游要求更久则按上游时间等待。运行中或冷却中均可停止。Cookie 失效或额度耗尽会停止；容器重启后需手动重新开始。</p>}
        <div className="form-actions">
          <button disabled={busy} onClick={() => setOpen(false)}>取消</button>
          <button className="primary" disabled={busy || active || remaining === 0} onClick={() => void start()}>
            {busy ? '启动中…' : '开始注册'}
          </button>
        </div>
      </Dialog>
    </div>
  )
}
