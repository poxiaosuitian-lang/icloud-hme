import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it, vi } from 'vitest'
import MailboxDialog from './MailboxDialog'

it('关闭再打开清除未保存的凭据，切换账号初始化配置', async () => {
  const props = { accountId: 'acc_one', open: true, onClose: vi.fn(), onSaved: vi.fn() }
  const { rerender } = render(<MailboxDialog {...props} />)
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('邮箱授权码'), 'synthetic-password')
  await user.type(screen.getByLabelText('收件邮箱'), 'draft@example.test')
  rerender(<MailboxDialog {...props} open={false} />)
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  rerender(<MailboxDialog {...props} />)
  expect(screen.getByLabelText('邮箱授权码')).toHaveValue('')
  expect(screen.getByLabelText('收件邮箱')).toHaveValue('')
  rerender(<MailboxDialog {...props} accountId="acc_two" current={{ provider: 'gmail', email: 'configured@example.test', imap_host: 'imap.gmail.com', imap_port: 993 }} />)
  expect(screen.getByLabelText('收件邮箱')).toHaveValue('configured@example.test')
  expect(screen.getByLabelText('邮箱服务商')).toHaveValue('gmail')
  expect(screen.getByLabelText('邮箱授权码')).toHaveValue('')
})
