import { useCallback, useEffect, useRef, useState } from 'react'

interface CopyButtonProps {
  text: string
  label: string
  copiedLabel?: string
  className?: string
}

export function CopyButton({
  text,
  label,
  copiedLabel = 'Copied',
  className,
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
      if (timer.current) clearTimeout(timer.current)
      timer.current = setTimeout(() => setCopied(false), 1400)
    })
  }, [text])

  return (
    <button type="button" className={className} onClick={onCopy}>
      {copied ? copiedLabel : label}
    </button>
  )
}
