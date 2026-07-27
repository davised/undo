import { useCallback, useEffect, useRef, useState } from 'react'

interface CopyButtonProps {
  text: string
  label: string
  copiedLabel?: string
  className?: string
  /** Fires on a successful copy, for feedback outside the button itself. */
  onCopied?: () => void
}

export function CopyButton({
  text,
  label,
  copiedLabel = 'Copied',
  className,
  onCopied,
}: CopyButtonProps) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    return () => {
      if (timer.current) clearTimeout(timer.current)
    }
  }, [])

  const onCopy = useCallback(() => {
    navigator.clipboard.writeText(text).then(() => {
      setCopied(true)
      onCopied?.()
      if (timer.current) clearTimeout(timer.current)
      timer.current = setTimeout(() => setCopied(false), 1400)
    })
  }, [text, onCopied])

  return (
    <button
      type="button"
      className={className}
      onClick={onCopy}
      // the label swaps to "Copied", so the change needs announcing for
      // anyone who is not looking at the button
      aria-live="polite"
    >
      {copied ? copiedLabel : label}
    </button>
  )
}
