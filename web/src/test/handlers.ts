import { http, HttpResponse } from 'msw'

export const handlers = [
  http.get('/api/settings/recovery', () => HttpResponse.json({ success: true, data: { waiting_connections: 0, accounts: [] } })),
  http.post('/api/auth/login', () =>
    HttpResponse.json(
      { success: false, code: 'INVALID_CREDENTIALS', message: '管理员密码错误' },
      { status: 401 },
    ),
  ),
  http.get('/api/auth/session', () =>
    HttpResponse.json(
      { success: false, code: 'AUTH_REQUIRED', message: '请先登录' },
      { status: 401 },
    ),
  ),
  http.get('/api/accounts', () =>
    HttpResponse.json({ success: true, data: [] }),
  ),
  http.get('/api/alias-batch', () =>
    HttpResponse.json({ success: true, data: null }),
  ),
]
