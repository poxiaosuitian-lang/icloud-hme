import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import MailBody from './MailBody'

describe('MailBody', () => {
  it('makes HTTP, HTTPS and www links clickable without changing query parameters', () => {
    const body = '验证：https://example.com/verify?token=abc%2F123&next=%2Fhome。\nhttp://example.org/a\nwww.example.net/help'
    const { container } = render(<MailBody body={body} />)
    const links = screen.getAllByRole('link')
    expect(links.map((link) => link.getAttribute('href'))).toEqual([
      'https://example.com/verify?token=abc%2F123&next=%2Fhome',
      'http://example.org/a',
      'https://www.example.net/help',
    ])
    for (const link of links) {
      expect(link).toHaveAttribute('target', '_blank')
      expect(link).toHaveAttribute('rel', 'noopener noreferrer')
    }
    expect(container.textContent).toBe(body)
  })

  it('excludes enclosing and sentence punctuation but preserves balanced URL parentheses', () => {
    render(<MailBody body={'(https://example.com/verify?a=1). https://example.com/wiki/Topic_(test), https://example.com/x]!'} />)
    expect(screen.getAllByRole('link').map((link) => link.getAttribute('href'))).toEqual([
      'https://example.com/verify?a=1',
      'https://example.com/wiki/Topic_(test)',
      'https://example.com/x',
    ])
  })

  it('keeps malicious HTML and executable schemes inert', () => {
    const body = '<img src=x onerror=alert(1)> <script>alert(2)</script> javascript:alert(3) data:text/html,evil file:///etc/passwd'
    const { container } = render(<MailBody body={body} />)
    expect(container.textContent).toBe(body)
    expect(container.querySelector('img, script, iframe')).toBeNull()
    expect(screen.queryByRole('link')).toBeNull()
  })

  it('keeps invalid URLs and www inside email addresses as plain text', () => {
    const body = 'https:// http://?x=1 a@www.example.com'
    const { container } = render(<MailBody body={body} />)
    expect(screen.queryByRole('link')).toBeNull()
    expect(container.textContent).toBe(body)
  })
})
