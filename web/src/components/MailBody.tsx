import type { ReactNode } from 'react'

// Render email content as React text, never as untrusted HTML. Only web URLs
// become links; scripts, data URLs and other executable schemes stay inert.
export default function MailBody({ body }: { body: string }) {
  const content: ReactNode[] = []
  const urls = /\b(?:https?:\/\/|www\.)[^\s<>"'\p{Cc}，。！？；：、（）【】《》]+/giu
  let position = 0

  for (const match of body.matchAll(urls)) {
    const start = match.index
    // Do not match www inside another URL path or an email address.
    if (start > 0 && /[\w@/]/.test(body[start - 1])) continue
    let label = match[0].replace(/[.,!?;:]+$/, '')
    // Keep balanced parentheses used in URLs, but not enclosing punctuation.
    while (/[)\]}]$/.test(label)) {
      const close = label.at(-1)!
      const open = { ')': '(', ']': '[', '}': '{' }[close]!
      if (label.split(close).length <= label.split(open).length) break
      label = label.slice(0, -1).replace(/[.,!?;:]+$/, '')
    }
    const href = /^www\./i.test(label) ? `https://${label}` : label
    try {
      const url = new URL(href)
      if (!['http:', 'https:'].includes(url.protocol) || !url.hostname) continue
    } catch {
      continue
    }

    content.push(body.slice(position, start))
    content.push(
      <a key={start} href={href} target="_blank" rel="noopener noreferrer">
        {label}
      </a>,
    )
    position = start + label.length
  }
  content.push(body.slice(position))

  return <pre className="mail-body">{content}</pre>
}
