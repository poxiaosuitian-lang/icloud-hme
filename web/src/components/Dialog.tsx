import { useEffect, useRef, type ReactNode } from 'react'

interface DialogProps {
  title: string
  open: boolean
  onClose: () => void
  children: ReactNode
}

/** 可访问 Dialog:Escape 关闭、焦点圈定、关闭后回到触发按钮 */
export default function Dialog({ title, open, onClose, children }: DialogProps) {
  const ref = useRef<HTMLDivElement>(null)
  const lastFocused = useRef<Element | null>(null)
  const onCloseRef = useRef(onClose)

  // 表单输入会让父组件重新渲染，而调用方通常会传入一个新的内联函数。
  // 始终保留最新回调，但不要因此重新执行下面的焦点初始化逻辑。
  useEffect(() => {
    onCloseRef.current = onClose
  }, [onClose])

  useEffect(() => {
    if (!open) return
    lastFocused.current = document.activeElement
    const node = ref.current
    if (!node) return
    const focusables = () =>
      Array.from(
        node.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
        ),
      ).filter((el) => !el.hasAttribute('disabled'))
    focusables()[0]?.focus()

    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onCloseRef.current()
        return
      }
      if (e.key !== 'Tab') return
      const items = focusables()
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', handleKey)
    return () => {
      document.removeEventListener('keydown', handleKey)
      if (lastFocused.current instanceof HTMLElement) {
        lastFocused.current.focus()
      }
    }
  }, [open])

  if (!open) return null

  return (
    <div className="dialog-backdrop">
      <button type="button" className="dialog-overlay" aria-label="关闭对话框" tabIndex={-1} onClick={onClose} />
      <div
        ref={ref}
        className="dialog"
        role="dialog"
        aria-modal="true"
        aria-label={title}
      >
        <h3>{title}</h3>
        {children}
      </div>
    </div>
  )
}
