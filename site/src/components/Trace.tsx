import { motion, useReducedMotion } from 'motion/react'
import { Reveal, EASE } from './Reveal'

const TRACE_LINES = [
  { text: 'rm -rf thesis/', cls: 'fn' },
  {
    parts: [
      '  unlinkat("thesis/draft.md")   ',
      { text: '-> hardlink to session store', cls: 'act' },
      '  ',
      { text: 'journaled', cls: 'ok' },
    ],
  },
  {
    parts: [
      '  unlinkat("thesis/refs.bib")   ',
      { text: '-> hardlink to session store', cls: 'act' },
      '  ',
      { text: 'journaled', cls: 'ok' },
    ],
  },
  {
    parts: [
      '  unlinkat("thesis/notes", DIR) ',
      { text: '-> mode 755 recorded', cls: 'act' },
      '          ',
      { text: 'journaled', cls: 'ok' },
    ],
  },
  {
    parts: [
      '  unlinkat("thesis", DIR)       ',
      { text: '-> mode 755 recorded', cls: 'act' },
      '          ',
      { text: 'journaled', cls: 'ok' },
    ],
  },
  { text: '', cls: '' },
  {
    text: '# hardlinks share the inode: no data copied, no matter the size.',
    cls: 'cmt',
  },
  {
    text: '# the journal is a plain text file. undo replays it bottom-up.',
    cls: 'cmt',
  },
]

export function Trace() {
  const reduce = useReducedMotion()

  return (
    <section id="how">
      <div className="wrap">
        <Reveal>
          <h2>rm ran normally. It was just being watched.</h2>
          <p className="lead">
            A shell hook (zsh, bash, fish) arms a small <b>LD_PRELOAD</b>{' '}
            library around each command. When the command calls libc to destroy
            something, the library saves the file first, then lets the call
            through. This is the trace of that rm:
          </p>
        </Reveal>
        <motion.pre
          className="panel"
          initial={reduce ? false : 'hidden'}
          whileInView="show"
          viewport={{ once: true, amount: 0.35 }}
          variants={{
            hidden: {},
            show: { transition: { staggerChildren: 0.09, delayChildren: 0.1 } },
          }}
        >
          {TRACE_LINES.map((line, i) => (
            <motion.span
              key={i}
              style={{ display: 'block' }}
              variants={{
                // offset only: `hidden` is what the server renders, and a
                // transparent default hides the trace entirely without JS
                hidden: reduce ? {} : { x: -8 },
                show: {
                  x: 0,
                  transition: { duration: 0.4, ease: EASE },
                },
              }}
            >
              {'parts' in line && line.parts
                ? line.parts.map((p, j) =>
                    typeof p === 'string' ? (
                      p
                    ) : (
                      <span key={j} className={p.cls}>
                        {p.text}
                      </span>
                    ),
                  )
                : 'text' in line && (
                    <span className={line.cls || undefined}>
                      {line.text || ' '}
                    </span>
                  )}
            </motion.span>
          ))}
        </motion.pre>
        <Reveal delay={0.1}>
          <p className="after">
            The same interception covers <b>rename</b> (mv over a file you
            needed), <b>open with write flags</b> (the &gt; that truncated your
            config), <b>mkdir</b>, <b>chmod</b>, and friends. Between your
            commands, nothing runs at all.
          </p>
        </Reveal>
      </div>
    </section>
  )
}
