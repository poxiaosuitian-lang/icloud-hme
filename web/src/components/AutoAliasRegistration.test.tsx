import { http, HttpResponse } from 'msw'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AutoAliasRegistration from './AutoAliasRegistration'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import type { AliasBatchStatus } from '../api/types'

const running: AliasBatchStatus = {
  id: 'job_1', account_id: 'acc_1', status: 'running', count: 10, created: 2,
  interval_seconds: 5, label_prefix: '自动注册', alias_total: 12, max_aliases: 750,
  last_email: 'new@example.test', started_at: '2026-09-30T00:00:00Z',
}

function renderRegistration(total = 2, onChanged = vi.fn()) {
  return render(<AutoAliasRegistration accountId="acc_1" aliasTotal={total} onAliasesChanged={onChanged} />)
}

async function openDialog() {
  const button = await screen.findByRole('button', { name: '自动注册别名' })
  await waitFor(() => expect(button).toBeEnabled())
  await userEvent.click(button)
  return screen.getByRole('dialog')
}

describe('AutoAliasRegistration', () => {
  beforeEach(() => {
    setCSRFToken('csrf-test')
    server.resetHandlers()
  })

  it('配置本次数量、间隔秒数和标签，通过 CSRF 请求启动任务', async () => {
    let body: unknown
    let csrf = ''
    server.use(http.post('/api/alias-batch', async ({ request }) => {
      body = await request.json()
      csrf = request.headers.get('X-CSRF-Token') ?? ''
      return HttpResponse.json({ success: true, data: { ...running, created: 0, count: 3, interval_seconds: 2.5 } }, { status: 202 })
    }))
    renderRegistration()
    const dialog = await openDialog()
    const user = userEvent.setup()
    expect(screen.getByLabelText('冷却时长（分钟）')).toHaveValue(60)
    await user.clear(screen.getByLabelText('冷却时长（分钟）'))
    await user.type(screen.getByLabelText('冷却时长（分钟）'), '45')
    await user.clear(screen.getByLabelText('本次注册数量'))
    await user.type(screen.getByLabelText('本次注册数量'), '3')
    await user.clear(screen.getByLabelText('每个间隔（秒）'))
    await user.type(screen.getByLabelText('每个间隔（秒）'), '2.5')
    await user.clear(screen.getByLabelText('标签前缀（可选）'))
    await user.type(screen.getByLabelText('标签前缀（可选）'), '订阅')
    await user.click(within(dialog).getByRole('button', { name: '开始注册' }))
    await waitFor(() => expect(body).toEqual({ channel: 'icloud_web', account_id: 'acc_1', count: 3, interval_seconds: 2.5, cooldown_minutes: 45, label_prefix: '订阅' }))
    expect(csrf).toBe('csrf-test')
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('注册数量按剩余额度限制，已有 749 个时默认只能注册 1 个', async () => {
    renderRegistration(749)
    await openDialog()
    const input = screen.getByLabelText('本次注册数量')
    expect(input).toHaveValue(1)
    expect(input).toHaveAttribute('max', '1')
    const user = userEvent.setup()
    await user.clear(input)
    await user.type(input, '2')
    await user.click(screen.getByRole('button', { name: '开始注册' }))
    expect(screen.getByRole('alert')).toHaveTextContent('1-1 的整数')
  })

  it('750 个别名时禁用自动注册', async () => {
    renderRegistration(750)
    expect(await screen.findByRole('button', { name: '自动注册别名' })).toBeDisabled()
    expect(screen.getByText(/还可注册 0 个/)).toBeInTheDocument()
  })

  it('校验整数数量和每个间隔边界，不发送无效注册', async () => {
    let requests = 0
    server.use(http.post('/api/alias-batch', () => { requests++; return HttpResponse.json({ success: true }) }))
    renderRegistration()
    await openDialog()
    const user = userEvent.setup()
    const input = screen.getByLabelText('本次注册数量')
    await user.clear(input)
    await user.type(input, '1.5')
    await user.click(screen.getByRole('button', { name: '开始注册' }))
    expect(screen.getByRole('alert')).toHaveTextContent('整数')
    await user.clear(input)
    await user.type(input, '2')
    await user.clear(screen.getByLabelText('每个间隔（秒）'))
    await user.type(screen.getByLabelText('每个间隔（秒）'), '0')
    await user.click(screen.getByRole('button', { name: '开始注册' }))
    expect(screen.getByRole('alert')).toHaveTextContent('1-86400 秒')
    expect(requests).toBe(0)
  })

  it('刷新页面恢复运行任务并支持停止，无需重发注册', async () => {
    let job = running
    let stopBody: unknown
    server.use(
      http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: job })),
      http.post('/api/alias-batch/stop', async ({ request }) => {
        stopBody = await request.json()
        job = { ...running, status: 'stopped', message: '已停止自动注册' }
        return HttpResponse.json({ success: true, data: job })
      }),
    )
    const changed = vi.fn()
    renderRegistration(12, changed)
    expect(await screen.findByText('正在自动注册')).toBeInTheDocument()
    expect(screen.getByRole('progressbar')).toHaveAttribute('value', '2')
    expect(screen.getByText(/本次已注册 2 \/ 10/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '自动注册别名' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: '停止注册任务' }))
    await waitFor(() => expect(stopBody).toEqual({ account_id: 'acc_1', job_id: 'job_1' }))
    expect(await screen.findByText('已停止')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '停止注册任务' })).toBeDisabled()
    expect(changed).toHaveBeenCalled()
  })

  it('没有运行任务时停止注册任务按钮仍可见且禁用', async () => {
    renderRegistration()
    expect(screen.getByRole('button', { name: '停止注册任务' })).toBeDisabled()
    await waitFor(() => expect(screen.getByRole('button', { name: '自动注册别名' })).toBeEnabled())
    expect(screen.getByRole('button', { name: '停止注册任务' })).toBeDisabled()
  })

  it('停止请求等待响应时显示停止中，并避免重复请求', async () => {
    let job: AliasBatchStatus = running
    let release: (() => void) | undefined
    let requests = 0
    let csrf = ''
    server.use(
      http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: job })),
      http.post('/api/alias-batch/stop', async ({ request }) => {
        requests++
        csrf = request.headers.get('X-CSRF-Token') ?? ''
        await new Promise<void>((resolve) => { release = resolve })
        job = { ...running, status: 'stopped' }
        return HttpResponse.json({ success: true, data: job })
      }),
    )
    renderRegistration()
    const button = await screen.findByRole('button', { name: '停止注册任务' })
    await waitFor(() => expect(button).toBeEnabled())
    await userEvent.click(button)
    await waitFor(() => expect(requests).toBe(1))
    const pending = screen.getByRole('button', { name: '正在停止…' })
    expect(pending).toBeDisabled()
    expect(pending).toHaveAttribute('aria-busy', 'true')
    await userEvent.click(pending)
    expect(requests).toBe(1)
    expect(csrf).toBe('csrf-test')
    release?.()
    expect(await screen.findByText('已停止')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '停止注册任务' })).toBeDisabled()
  })

  it('停止失败时保留运行状态和错误，允许重新停止', async () => {
    server.use(
      http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: running })),
      http.post('/api/alias-batch/stop', () => HttpResponse.json({ success: false, message: '停止任务失败' }, { status: 502 })),
    )
    renderRegistration()
    const button = await screen.findByRole('button', { name: '停止注册任务' })
    await waitFor(() => expect(button).toBeEnabled())
    await userEvent.click(button)
    expect(await screen.findByRole('alert')).toHaveTextContent('停止任务失败')
    await waitFor(() => expect(screen.getByRole('button', { name: '停止注册任务' })).toBeEnabled())
    expect(screen.getByText('正在自动注册')).toBeInTheDocument()
  })

  it('上游拒绝启动时保留配置并显示错误', async () => {
    server.use(http.post('/api/alias-batch', () => HttpResponse.json({
      success: false, code: 'ALIAS_QUOTA_EXCEEDED', message: '本次最多注册 1 个',
    }, { status: 400 })))
    renderRegistration()
    await openDialog()
    await userEvent.click(screen.getByRole('button', { name: '开始注册' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('本次最多注册 1 个')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('轮询更新进度并通知别名列表刷新', async () => {
    let job: AliasBatchStatus = running
    server.use(http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: job })))
    const changed = vi.fn()
    renderRegistration(12, changed)
    await screen.findByText('正在自动注册')
    job = { ...running, status: 'completed', created: 10, alias_total: 20, message: '本次自动注册已完成' }
    expect(await screen.findByText('注册完成', {}, { timeout: 4000 })).toBeInTheDocument()
    expect(screen.getByRole('progressbar')).toHaveAttribute('value', '10')
    expect(changed).toHaveBeenCalledTimes(1)
  })

  it('停止中禁用停止按钮，状态读取失败不重复启动任务', async () => {
    server.use(http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: { ...running, status: 'stopping' } })))
    const view = renderRegistration()
    expect(await screen.findByRole('button', { name: '正在停止…' })).toBeDisabled()
    view.unmount()
    server.use(http.get('/api/alias-batch', () => HttpResponse.json({ success: false, message: '读取失败' }, { status: 502 })))
    renderRegistration()
    expect(await screen.findByRole('alert')).toHaveTextContent('读取失败')
    expect(screen.getByRole('button', { name: '自动注册别名' })).toBeDisabled()
  })
})

it('冷却中保留进度、显示恢复时间、禁止重复启动并可停止', async () => {
 setCSRFToken('csrf-test')
 let job: AliasBatchStatus = { ...running, status: 'cooling_down', cooldown_count: 1,
  next_run_at: '2026-10-07T15:30:00Z', error_code: 'UPSTREAM_RATE_LIMITED', message: '频率受限，等待冷却' }
 let stopBody: unknown
 server.use(
  http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: job })),
  http.post('/api/alias-batch/stop', async ({ request }) => {
   stopBody = await request.json()
   job = { ...job, status: 'stopped', next_run_at: undefined }
   return HttpResponse.json({ success: true, data: job })
  }),
 )
 renderRegistration()
 expect(await screen.findByText('冷却中，到期自动继续')).toBeInTheDocument()
 expect(screen.getByText(/冷却结束：/)).toBeInTheDocument()
 expect(screen.getByText(/已进入冷却 1 次；剩余 8 个/)).toBeInTheDocument()
 expect(screen.getByRole('progressbar')).toHaveAttribute('value', '2')
 expect(screen.getByRole('button', { name: '自动注册别名' })).toBeDisabled()
 const stop = screen.getByRole('button', { name: '停止注册任务' })
 expect(stop).toBeEnabled()
 await userEvent.click(stop)
 await waitFor(() => expect(stopBody).toEqual({ account_id: 'acc_1', job_id: 'job_1' }))
 expect(await screen.findByText('已停止')).toBeInTheDocument()
})

it('冷却结束后轮询显示续跑和最终完成', async () => {
 let job: AliasBatchStatus = { ...running, status: 'cooling_down', cooldown_count: 1, next_run_at: '2026-10-07T15:30:00Z' }
 server.use(http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: job })))
 renderRegistration()
 await screen.findByText('冷却中，到期自动继续')
 job = { ...job, status: 'running', created: 3, next_run_at: undefined }
 expect(await screen.findByText('正在自动注册', {}, { timeout: 4000 })).toBeInTheDocument()
 expect(screen.getByRole('progressbar')).toHaveAttribute('value', '3')
 job = { ...job, status: 'completed', created: 10 }
 expect(await screen.findByText('注册完成', {}, { timeout: 4000 })).toBeInTheDocument()
 expect(screen.getByRole('progressbar')).toHaveAttribute('value', '10')
}, 10000)

it.each(['0', '-1', '1.5', '1441'])('拒绝无效冷却分钟数 %s', async (value) => {
 let requests = 0
 server.use(http.post('/api/alias-batch', () => { requests++; return HttpResponse.json({ success: true }) }))
 renderRegistration()
 await openDialog()
 const user = userEvent.setup()
 await user.clear(screen.getByLabelText('冷却时长（分钟）'))
 await user.type(screen.getByLabelText('冷却时长（分钟）'), value)
 await user.click(screen.getByRole('button', { name: '开始注册' }))
 expect(screen.getByRole('alert')).toHaveTextContent('冷却时长需为 1-1440 分钟的整数')
 expect(requests).toBe(0)
})

it('默认提交 60 分钟冷却时长', async () => {
 let body: Record<string, unknown> | undefined
 server.use(http.post('/api/alias-batch', async ({ request }) => {
  body = await request.json() as Record<string, unknown>
  return HttpResponse.json({ success: true, data: { ...running, cooldown_minutes: 60, batch_size: 5 } }, { status: 202 })
 }))
 renderRegistration()
 await openDialog()
 await userEvent.click(screen.getByRole('button', { name: '开始注册' }))
 await waitFor(() => expect(body?.cooldown_minutes).toBe(60))
})

it('刷新后展示服务端保存的自定义冷却时长和批次数量', async () => {
 server.use(http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: {
  ...running, status: 'cooling_down', created: 5, cooldown_minutes: 45, batch_size: 5,
  next_run_at: '2026-10-15T01:30:00Z', cooldown_count: 1,
 } })))
 renderRegistration()
 expect(await screen.findByText('每批 5 个，冷却 45 分钟后自动继续剩余任务。')).toBeInTheDocument()
 expect(screen.getByRole('progressbar')).toHaveAttribute('value', '5')
 expect(screen.getByRole('button', { name: '停止注册任务' })).toBeEnabled()
})

it('可选择新旧双通道并将模式提交给后端', async () => {
  setCSRFToken('csrf-test')
  server.resetHandlers()
  let body: Record<string, unknown> = {}
  let job: AliasBatchStatus | null = null
  server.use(
    http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: job })),
    http.post('/api/alias-batch', async ({ request }) => {
      body = await request.json() as Record<string, unknown>
      job = { ...running, channel: 'auto' }
      return HttpResponse.json({ success: true, data: job }, { status: 202 })
    }),
  )
  renderRegistration()
  await openDialog()
  await userEvent.selectOptions(screen.getByLabelText('创建通道'), 'auto')
  expect(screen.queryByLabelText('每个间隔（秒）')).not.toBeInTheDocument()
  expect(screen.queryByLabelText('冷却时长（分钟）')).not.toBeInTheDocument()
  expect(screen.getByText(/首次也等待计划时间/)).toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: '开始注册' }))
  await waitFor(() => expect(body.channel).toBe('auto'))
  expect(body.interval_seconds).toBe(120)
  expect(body.cooldown_minutes).toBe(60)
  expect(await screen.findByText('创建模式：新旧双通道')).toBeInTheDocument()
})

it('双通道状态展示原项目小时计划和降级状态，不显示每批五个', async () => {
  setCSRFToken('csrf-test')
  server.resetHandlers()
  server.use(http.get('/api/alias-batch', () => HttpResponse.json({ success: true, data: {
    ...running, channel: 'auto', policy: 'upstream_19_4', schedule_mode: 'web_only', schedule_slot: 10,
  } })))
  renderRegistration()
  expect(await screen.findByText(/降级旧通道，每小时 5 个/)).toBeInTheDocument()
  expect(screen.getByText(/每小时 23 个计划时隙/)).toBeInTheDocument()
  expect(screen.queryByText(/每批 5 个/)).not.toBeInTheDocument()
})
