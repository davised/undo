import { memo, useEffect, useMemo, useRef, useState } from 'react'
import { useReducedMotion } from 'motion/react'

type Cls = 'p' | 'c' | 'bad' | 'ok' | 'dim'
interface Seg {
  t: string
  c: Cls
}
type Line = Seg[]
interface Frame {
  lines: Line[]
  cursor: [number, number] | null
  dur: number
}

const CLASS: Record<Cls, string> = {
  p: 't-p',
  c: 't-c',
  bad: 't-c t-bad',
  ok: 't-ok',
  dim: 't-o',
}

const PROMPT: Seg = { t: '~/work $ ', c: 'p' }
const segLen = (segs: Line) => segs.reduce((n, s) => n + s.t.length, 0)

// Build the frame list once: same sequence as the README GIF, typed live.
function buildFrames(): Frame[] {
  const frames: Frame[] = []
  const lines: Line[] = []
  const snap = (cursor: [number, number] | null, dur: number) =>
    frames.push({ lines: lines.map((l) => l.slice()), cursor, dur })

  const promptLine = (lead: number) => {
    lines.push([{ ...PROMPT }])
    snap([lines.length - 1, segLen(lines[lines.length - 1])], lead)
  }
  const typeOnLast = (text: string, c: Cls) => {
    const row = lines.length - 1
    const base = lines[row].slice()
    for (let i = 1; i <= text.length; i++) {
      lines[row] = [...base, { t: text.slice(0, i), c }]
      snap([row, segLen(base) + i], 46)
    }
  }
  const outLine = (segs: Line, dur: number) => {
    lines.push(segs)
    snap(null, dur)
  }
  const hold = (dur: number, cursor: [number, number] | null = null) =>
    snap(cursor, dur)

  promptLine(700)
  typeOnLast('rm -rf thesis/', 'bad')
  hold(600, [0, segLen([PROMPT]) + 'rm -rf thesis/'.length])

  promptLine(300)
  typeOnLast('undo', 'c')
  hold(400)
  outLine([{ t: '$ rm -rf thesis/  (14:02:11, 4 changes)', c: 'dim' }], 130)
  outLine([{ t: '  deleted   thesis/draft.md', c: 'dim' }], 95)
  outLine([{ t: '  deleted   thesis/refs.bib', c: 'dim' }], 95)
  outLine([{ t: '  deleted   thesis/notes/', c: 'dim' }], 95)
  outLine([{ t: '  deleted   thesis/', c: 'dim' }], 300)

  const q = 'revert this? [y/N] '
  lines.push([{ t: q, c: 'dim' }])
  snap([lines.length - 1, q.length], 800)
  lines[lines.length - 1] = [
    { t: q, c: 'dim' },
    { t: 'y', c: 'c' },
  ]
  snap([lines.length - 1, q.length + 1], 550)
  outLine([{ t: 'restored 4 change(s)', c: 'ok' }], 800)

  promptLine(300)
  typeOnLast('ls thesis', 'c')
  hold(350)
  outLine([{ t: 'draft.md  notes/  refs.bib', c: 'dim' }], 250)

  promptLine(0)
  const prow = lines.length - 1
  for (let i = 0; i < 3; i++) {
    hold(480, [prow, segLen([PROMPT])])
    hold(480, null)
  }
  hold(2200, [prow, segLen([PROMPT])]) // pause before loop
  return frames
}

// Memoized so a frame that only extends the last line does not re-render
// every other line: the animation ticks ~20 times a second and this is the
// difference between a smooth scroll and a janky one.
const Row = memo(function Row({
  line,
  caret,
}: {
  line: Line
  caret: boolean
}) {
  return (
    <div className="term-row">
      {line.map((seg, s) => (
        <span key={s} className={CLASS[seg.c]}>
          {seg.t}
        </span>
      ))}
      {caret && <span className="term-caret" />}
    </div>
  )
})

export function TypedTerminal() {
  const reduce = useReducedMotion()
  const frames = useMemo(buildFrames, [])
  // start on the completed session so SSR / no-JS / reduced-motion all
  // show the whole story; the effect replays from the top when able
  const [i, setI] = useState(() => frames.length - 1)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const box = useRef<HTMLDivElement>(null)
  const [visible, setVisible] = useState(true)

  // don't burn frames animating a terminal nobody is looking at
  useEffect(() => {
    const el = box.current
    if (!el || typeof IntersectionObserver === 'undefined') return
    const io = new IntersectionObserver(
      ([entry]) => setVisible(entry.isIntersecting),
      { rootMargin: '120px' },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [])

  useEffect(() => {
    if (reduce || !visible) return
    // Schedule outside the state updater. React can invoke an updater more
    // than once (StrictMode does it deliberately), and this one used to
    // start the next timer, so every tick spawned an extra chain and the
    // whole thing ran away.
    let cancelled = false
    let idx = 0
    setI(0)
    const tick = () => {
      if (cancelled) return
      idx = (idx + 1) % frames.length
      setI(idx)
      timer.current = setTimeout(tick, frames[idx].dur)
    }
    timer.current = setTimeout(tick, 700)
    return () => {
      cancelled = true
      if (timer.current) clearTimeout(timer.current)
    }
  }, [frames, reduce, visible])

  const frame = frames[i]
  const rows = Math.max(...frames.map((f) => f.lines.length))

  return (
    <div className="term" aria-hidden="true" ref={box}>
      <div className="term-bar">
        <i />
        <i />
        <i />
        <span>zsh</span>
      </div>
      <pre className="term-body" style={{ minHeight: `${rows * 1.75}em` }}>
        {frame.lines.map((line, r) => (
          <Row
            key={r}
            line={line}
            caret={frame.cursor !== null && frame.cursor[0] === r}
          />
        ))}
      </pre>
    </div>
  )
}
