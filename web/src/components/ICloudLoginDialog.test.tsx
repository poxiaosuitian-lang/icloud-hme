import { http, HttpResponse } from 'msw'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, it, vi } from 'vitest'
import ICloudLoginDialog from './ICloudLoginDialog'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'

beforeEach(() => { setCSRFToken('csrf-test'); server.resetHandlers() })
it('新通道使用独立登录接口，验证码阶段不重发密码', async () => {
  const bodies: unknown[] = []
  server.use(http.post('/api/accounts/acc_1/apple-login', async ({ request }) => {
    expect(request.headers.get('X-CSRF-Token')).toBe('csrf-test')
    bodies.push(await request.json())
    if (bodies.length === 1) return HttpResponse.json({ success: false, code: 'OTP_REQUIRED' }, { status: 409 })
    return HttpResponse.json({ success: true, data: { id: 'acc_1', has_apple_session: true } })
  }))
  const saved = vi.fn()
  render(<ICloudLoginDialog accountId="acc_1" channel="apple_account" open onClose={vi.fn()} onSaved={saved} />)
  const user = userEvent.setup()
  expect(screen.getByRole('dialog')).toHaveAccessibleName('Apple Account 新通道登录')
  await user.type(screen.getByLabelText('iCloud 登录密码'), 'fixture-password')
  await user.click(screen.getByRole('button', { name: '登录' }))
  await user.type(await screen.findByLabelText('验证码'), '123456')
  await user.click(screen.getByRole('button', { name: '验证' }))
  await waitFor(() => expect(saved).toHaveBeenCalledOnce())
  expect(bodies).toEqual([{ password: 'fixture-password' }, { otp_code: '123456' }])
})
