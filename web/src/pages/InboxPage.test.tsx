import { http, HttpResponse } from 'msw'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import InboxPage from './InboxPage'
import { server } from '../test/server'
import { setCSRFToken } from '../api/client'
import { ToastProvider } from '../components/ToastProvider'
import type { AccountSummary, InboxResult } from '../api/types'

const accounts: AccountSummary[] = [
  {
    id: 'acc_1',
    name: '主号',
    real_email: 'a@example.com',
    icloud_email: 'a@icloud.com',
    host: 'icloud.com',
    status: 'active',
    alias_total: 2,
    alias_active: 2,
    has_cookies: true,
    has_app_password: true,
    has_proxy: false,
    last_validated: '2026-08-04T09:00:00+08:00',
    created_at: '2026-08-01T09:00:00+08:00',
  },
]

const inboxResult: InboxResult = {
  account_id: 'acc_1',
  alias: 'alpha@icloud.com',
  count: 1,
  method: 'imap',
  messages: [
    {
      id: '1',
      from: 'sender@example.com',
      to: 'alpha@icloud.com',
      subject: '主题一',
      date: '2026-08-04T10:00:00+08:00',
      preview: '预览内容',
    },
  ],
}

function renderPage(initialPath = '/inbox') {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route
          path="/inbox"
          element={
            <ToastProvider>
              <InboxPage />
            </ToastProvider>
          }
        />
      </Routes>
    </MemoryRouter>,
  )
}

describe('InboxPage', () => {
  beforeEach(() => {
    setCSRFToken('csrf-test')
    server.resetHandlers()
  })

  it('合并垃圾邮件时显示来源，并用带文件夹的 ID 请求详情', async () => {
    const junk = { ...inboxResult.messages[0], id: 'Junk:1', folder: 'Junk', subject: '垃圾箱验证码' }
    let detailID = ''
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: {
        ...inboxResult, count: 2, messages: [inboxResult.messages[0], junk],
      } })),
      http.get('/api/inbox/:id', ({ params }) => {
        detailID = String(params.id)
        return HttpResponse.json({ success: true, data: { ...junk, body: '测试正文', content_type: 'text/plain' } })
      }),
    )
    renderPage()
    expect(await screen.findByText('垃圾邮件')).toBeInTheDocument()
    expect(screen.getByText('主题一')).toBeInTheDocument()
    await userEvent.setup().click(screen.getByRole('button', { name: '垃圾箱验证码' }))
    expect(await screen.findByText('测试正文')).toBeInTheDocument()
    expect(detailID).toBe('Junk:1')
    expect(screen.getByText('来源：垃圾邮件')).toBeInTheDocument()
  })

  it('打开邮件详情后正文中的链接可点击，并在新标签页打开', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: inboxResult })),
      http.get('/api/inbox/:id', () => HttpResponse.json({ success: true, data: {
        ...inboxResult.messages[0],
        body: '点击验证 (https://example.com/verify?token=abc&next=%2Fhome)。',
        content_type: 'text/plain',
      } })),
    )
    renderPage()
    await userEvent.setup().click(await screen.findByRole('button', { name: '主题一' }))
    const link = await screen.findByRole('link', { name: 'https://example.com/verify?token=abc&next=%2Fhome' })
    expect(link).toHaveAttribute('href', 'https://example.com/verify?token=abc&next=%2Fhome')
    expect(link).toHaveAttribute('target', '_blank')
    expect(link).toHaveAttribute('rel', 'noopener noreferrer')
  })

  it('账号必选;alias 可空;limit/days 生效;query 经 URLSearchParams', async () => {
    let lastUrl = ''
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', ({ request }) => {
        lastUrl = request.url
        return HttpResponse.json({ success: true, data: inboxResult })
      }),
    )
    renderPage()
    await screen.findByText('主题一')
    // 确认 query 参数
    const url = new URL(lastUrl)
    expect(url.searchParams.get('account_id')).toBe('acc_1')
    expect(url.searchParams.get('limit')).toBe('20')
    expect(url.searchParams.get('days')).toBe('7')
    // 修改 limit/days 再查询
    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText(/每页/), '100')
    await user.selectOptions(screen.getByLabelText(/时间范围/), '30')
    await user.click(screen.getByRole('button', { name: /查询/ }))
    await waitFor(() => {
      const u = new URL(lastUrl)
      expect(u.searchParams.get('limit')).toBe('100')
      expect(u.searchParams.get('days')).toBe('30')
    })
  })

  it('从 URL 的 alias 参数初始化筛选,支持别名页直达收件箱', async () => {
    const inboxUrls: string[] = []
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/aliases', () =>
        HttpResponse.json({
          success: true,
          data: {
            account_id: 'acc_1',
            count: 1,
            aliases: [
              {
                email: 'alpha@icloud.com',
                anonymousId: 'anon_alpha',
                label: 'Alpha',
                active: true,
              },
            ],
          },
        }),
      ),
      http.get('/api/inbox', ({ request }) => {
        inboxUrls.push(request.url)
        return HttpResponse.json({ success: true, data: inboxResult })
      }),
    )
    renderPage('/inbox?account_id=acc_1&alias=alpha%40icloud.com')
    await screen.findByText('主题一')
    await waitFor(() => {
      expect(inboxUrls.some((url) => new URL(url).searchParams.get('alias') === 'alpha@icloud.com')).toBe(true)
    })
    expect(screen.getByLabelText(/别名/)).toHaveValue('alpha@icloud.com')
  })

  it('展示 method=imap 或 web_api', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () =>
        HttpResponse.json({
          success: true,
          data: { ...inboxResult, method: 'web_api' },
        }),
      ),
    )
    renderPage()
    await screen.findByText('主题一')
    expect(screen.getByText(/Web API/)).toBeInTheDocument()
  })

  it('空列表、网络错误、401 状态', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () =>
        HttpResponse.json({
          success: true,
          data: { account_id: 'acc_1', count: 0, messages: [], method: 'imap' },
        }),
      ),
    )
    renderPage()
    expect(await screen.findByText(/暂无邮件/)).toBeInTheDocument()
  })

  it('兼容空收件箱返回 messages=null，不出现空白页', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () =>
        HttpResponse.json({
          success: true,
          data: { account_id: 'acc_1', count: 0, messages: null, method: 'imap' },
        }),
      ),
    )
    renderPage('/inbox?account_id=acc_1&alias=alpha%40icloud.com')
    expect(await screen.findByText(/暂无邮件/)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '收件箱' })).toBeInTheDocument()
  })

  it('恶意 HTML 只作为文本显示,不产生 img 节点', async () => {
    const evil = {
      ...inboxResult,
      messages: [
        {
          id: '2',
          from: 'evil@example.com',
          to: 'alpha@icloud.com',
          subject: '<img src=x onerror=alert(1)>',
          date: '2026-08-04T10:00:00+08:00',
          preview: '<script>alert(2)</script>预览',
        },
      ],
    }
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: evil })),
    )
    renderPage()
    await screen.findByText(/<img src=x onerror=alert\(1\)>/)
    expect(document.querySelector('img')).toBeNull()
    expect(document.querySelector('script')).toBeNull()
  })

  it('快速切换筛选:第一请求晚返回不覆盖第二请求', async () => {
    let release: (() => void) | undefined
    let calls = 0
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => {
        calls++
        if (calls === 1) {
          // 第一次请求挂起,稍后返回旧数据
          return new Promise<Response>((resolve) => {
            release = () =>
              resolve(
                HttpResponse.json({
                  success: true,
                  data: {
                    ...inboxResult,
                    messages: [{ ...inboxResult.messages[0], subject: '旧主题' }],
                  },
                }),
              )
          })
        }
        return HttpResponse.json({
          success: true,
          data: {
            ...inboxResult,
            alias: 'second',
            messages: [
              {
                id: '9',
                from: 's2@example.com',
                to: 'alpha@icloud.com',
                subject: '第二请求主题',
                date: '2026-08-04T11:00:00+08:00',
                preview: '第二请求',
              },
            ],
          },
        })
      }),
    )
    renderPage()
    await screen.findByText(/加载中/)
    // 触发第二次查询(首次挂起中)
    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText(/每页/), '100')
    await user.click(screen.getByRole('button', { name: /查询/ }))
    await screen.findByText('第二请求主题')
    // 第一次请求此时才返回
    release?.()
    // 旧数据不得覆盖新数据
    await new Promise((r) => setTimeout(r, 100))
    expect(screen.getByText('第二请求主题')).toBeInTheDocument()
    expect(screen.queryByText('旧主题')).toBeNull()
  })

  it('空 subject 显示(无主题)', async () => {
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () =>
        HttpResponse.json({
          success: true,
          data: {
            ...inboxResult,
            messages: [{ ...inboxResult.messages[0], subject: '' }],
          },
        }),
      ),
    )
    renderPage()
    expect(await screen.findByText(/（无主题）/)).toBeInTheDocument()
  })

  it('列表不显示摘要，点击收件人单元格才请求并展示详情', async () => {
    let detailCalls = 0
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: inboxResult })),
      http.get('/api/inbox/:id', () => {
        detailCalls++
        return HttpResponse.json({ success: true, data: { ...inboxResult.messages[0], body: '完整邮件内容', content_type: 'text/plain' } })
      }),
    )
    renderPage()
    const row = (await screen.findByText('主题一')).closest('tr')!
    expect(screen.queryByRole('columnheader', { name: '摘要' })).not.toBeInTheDocument()
    expect(screen.queryByText('预览内容')).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(detailCalls).toBe(0)
    await userEvent.setup().click(within(row).getByText('alpha@icloud.com'))
    expect(await screen.findByText('完整邮件内容')).toBeInTheDocument()
    expect(detailCalls).toBe(1)
    expect(screen.getByRole('dialog')).toHaveTextContent('发件人：sender@example.com')
  })

  it('点击行内删除按钮只打开确认框，不加载邮件详情或直接删除', async () => {
    let detailCalls = 0
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: inboxResult })),
      http.get('/api/inbox/:id', () => {
        detailCalls++
        return HttpResponse.json({ success: true, data: { ...inboxResult.messages[0], body: '完整邮件内容' } })
      }),
    )
    renderPage()
    await userEvent.setup().click(await screen.findByRole('button', { name: '删除邮件' }))
    expect(screen.getByRole('dialog', { name: '删除邮件' })).toBeInTheDocument()
    expect(detailCalls).toBe(0)
    expect(screen.queryByText('完整邮件内容')).not.toBeInTheDocument()
  })

  it('键盘可以通过主题按钮打开邮件，且只请求一次', async () => {
    let detailCalls = 0
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: inboxResult })),
      http.get('/api/inbox/:id', () => {
        detailCalls++
        return HttpResponse.json({ success: true, data: { ...inboxResult.messages[0], body: '键盘打开正文' } })
      }),
    )
    renderPage()
    const subject = await screen.findByRole('button', { name: '主题一' })
    subject.focus()
    await userEvent.setup().keyboard('{Enter}')
    expect(await screen.findByText('键盘打开正文')).toBeInTheDocument()
    expect(detailCalls).toBe(1)
  })

  it('读取详情时关闭对话框，迟到响应不会重新打开', async () => {
    let release: (() => void) | undefined
    server.use(
      http.get('/api/accounts', () => HttpResponse.json({ success: true, data: accounts })),
      http.get('/api/inbox', () => HttpResponse.json({ success: true, data: inboxResult })),
      http.get('/api/inbox/:id', () => new Promise<Response>((resolve) => {
        release = () => resolve(HttpResponse.json({ success: true, data: { ...inboxResult.messages[0], body: '迟到正文' } }))
      })),
    )
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: '主题一' }))
    expect(await screen.findByText('读取中…')).toBeInTheDocument()
    await waitFor(() => expect(release).toBeDefined())
    await user.click(screen.getByRole('button', { name: '关闭' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    release?.()
    await new Promise((resolve) => setTimeout(resolve, 100))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByText('迟到正文')).not.toBeInTheDocument()
  })
})
