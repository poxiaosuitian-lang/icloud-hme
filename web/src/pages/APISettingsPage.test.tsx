import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import APISettingsPage from './APISettingsPage'

const secret = 'secret-test-token-12345678901234567890'
const settings = { enabled: true, has_token: true, base_path: '/api/v1' }
const renderPage = () => render(<ToastProvider><APISettingsPage /></ToastProvider>)

beforeEach(() => {
  setCSRFToken('csrf-test')
  server.use(http.get('/api/settings/api', () => HttpResponse.json({ success: true, data: settings })))
  server.use(http.get('/api/settings/api/usage', () => HttpResponse.json({ success: true, data: {
    used_count: 4, unique_email_count: 2,
    callers: [{ caller_id: 'default', used_count: 1 }, { caller_id: 'vendor-a', used_count: 2 }, { caller_id: 'vendor-b', used_count: 1 }],
  } })))
})

it('显示地址和已配置状态，默认隐藏密钥，点击显示后才请求', async () => {
  let reveals = 0
  server.use(http.post('/api/settings/api/token', ({ request }) => {
    expect(request.headers.get('X-CSRF-Token')).toBe('csrf-test')
    reveals++
    return HttpResponse.json({ success: true, data: { token: secret } })
  }))
  renderPage()
  await screen.findByText('已启用')
  expect(screen.getByLabelText('API 地址')).toHaveValue(`${window.location.origin}/api/v1`)
  expect(screen.getByLabelText('当前 Token')).toHaveAttribute('type', 'password')
  expect(reveals).toBe(0)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '显示 Token' }))
  await waitFor(() => expect(screen.getByLabelText('当前 Token')).toHaveValue(secret))
  expect(document.querySelector('.api-code')?.textContent).not.toContain(secret)
  await user.click(screen.getByRole('button', { name: '隐藏 Token' }))
  expect(screen.getByLabelText('当前 Token')).not.toHaveValue(secret)
})

it('确认后更换 Token 并更新显示', async () => {
  let rotated = false
  server.use(http.post('/api/settings/api/rotate', () => {
    rotated = true
    return HttpResponse.json({ success: true, data: { ...settings, token: secret } })
  }))
  renderPage()
  await screen.findByText('已启用')
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '重新生成 Token' }))
  expect(rotated).toBe(false)
  await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: '确认更换' }))
  await waitFor(() => expect(screen.getByLabelText('当前 Token')).toHaveValue(secret))
})

it('停用需确认，提交后显示已停用', async () => {
  server.use(http.put('/api/settings/api', async ({ request }) => {
    expect(await request.json()).toEqual({ enabled: false })
    return HttpResponse.json({ success: true, data: { ...settings, enabled: false } })
  }))
  renderPage()
  await screen.findByText('已启用')
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '停用 API' }))
  await user.click(screen.getByRole('button', { name: '确认停用' }))
  expect(await screen.findByText('已停用')).toBeInTheDocument()
})

it('保存失败显示错误且保留已有状态', async () => {
  server.use(http.post('/api/settings/api/rotate', () => HttpResponse.json({ success: false, message: '保存失败' }, { status: 500 })))
  renderPage()
  await screen.findByText('已启用')
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '重新生成 Token' }))
  await user.click(screen.getByRole('button', { name: '确认更换' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('保存失败')
  expect(screen.getByText('已启用')).toBeInTheDocument()
})

it('说明 API 合并收件箱与 Junk，并保留带文件夹的邮件 ID', async () => {
  renderPage()
  await screen.findByText('已启用')
  expect(screen.getByText(/IMAP 与 Web API 回退均合并 INBOX \+ Junk/)).toBeInTheDocument()
  expect(screen.getByText(/垃圾邮件 ID 带 Junk: 前缀/)).toBeInTheDocument()
  expect(document.querySelector('.api-code')?.textContent).toContain('INBOX + Junk')
})

it('显示按调用商独立去重的统计，生成带 caller_id 的示例', async () => {
  renderPage()
  await screen.findByText('vendor-b')
  expect(screen.getByText('default（默认分组 / 旧调用）')).toBeInTheDocument()
  expect(screen.getByText(/已用记录共 4 条，涉及 2 个不同邮箱/)).toBeInTheDocument()
  expect(screen.getByText(/A 用过的邮箱仍可分配给 B/)).toBeInTheDocument()
  const code = () => document.querySelector('.api-code')?.textContent ?? ''
  expect(code()).toContain("CALLER_ID='vendor-a'")
  expect(code()).toContain('caller_id')
  expect(code()).toContain('$CALLER_ID')
  const user = userEvent.setup()
  const input = screen.getByLabelText('调用商标识（用于生成示例）')
  await user.clear(input)
  await user.type(input, 'Vendor-B')
  expect(code()).toContain("CALLER_ID='vendor-b'")
  await user.clear(input)
  expect(code()).toContain("CALLER_ID='default'")
  await user.type(input, 'bad/id')
  expect(screen.getByRole('button', { name: '复制调用示例' })).toBeDisabled()
  expect(screen.getByText('调用商标识格式不正确')).toBeInTheDocument()
})

it('可刷新调用商统计，失败不清空已读取的记录', async () => {
  renderPage()
  await screen.findByText('vendor-b')
  server.use(http.get('/api/settings/api/usage', () => HttpResponse.json({ success: false, message: '统计读取失败' }, { status: 500 })))
  await userEvent.click(screen.getByRole('button', { name: '刷新调用商统计' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('统计读取失败')
  expect(screen.getByText('vendor-b')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '刷新调用商统计' })).toBeEnabled()
})


it('多账号统计与调用示例按分配结果选择收件账号', async () => {
  server.use(http.get('/api/settings/api/usage', () => HttpResponse.json({ success: true, data: {
    used_count: 3, unique_email_count: 3, callers: [], accounts: [
      { account_id: 'acc_1', name: '主账号', used_count: 2, unique_email_count: 2, available: true },
      { account_id: 'acc_2', name: '备用账号', used_count: 1, unique_email_count: 1, available: true },
    ],
  } })))
  renderPage()
  const table = await screen.findByRole('table', { name: '账号分配统计' })
  expect(within(table).getByText('主账号')).toBeInTheDocument()
  expect(within(table).getByText('备用账号')).toBeInTheDocument()
  expect(within(table).getAllByRole('row')).toHaveLength(3)
  expect(screen.getByText(/传入 account_id 时仅使用指定账号/)).toBeInTheDocument()
  const example = document.querySelector('.api-code')?.textContent ?? ''
  expect(example).toContain("jq -er '.data.account_id'")
  expect(example).toContain("jq -er '.data.email'")
  expect(example).toContain('account_id=$ACCOUNT_ID')
  expect(example).toContain('alias=$ALIAS')
  expect(example).toContain('request_id')
  expect(example).not.toContain('"account_id":"$ACCOUNT_ID"')
})
