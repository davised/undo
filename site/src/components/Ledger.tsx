import { motion, useReducedMotion } from 'motion/react'
import { Reveal, EASE } from './Reveal'

interface LedgerRow {
  undone?: boolean
  id: string
  when: string
  changes: string
  cmd: string
}

const ROWS: LedgerRow[] = [
  { id: '17847195022114', when: 'Jul 25 16:41:07', changes: '  1', cmd: '> notes.md' },
  { id: '17847194815520', when: 'Jul 25 16:37:41', changes: '132', cmd: 'rm -rf node_modules/.cache' },
  { id: '17847191203348', when: 'Jul 25 15:58:12', changes: '  4', cmd: 'rm -rf thesis/', undone: true },
  { id: '17847189947761', when: 'Jul 25 15:37:19', changes: '  2', cmd: 'mv report.pdf ~/archive/' },
  { id: '17847186671203', when: 'Jul 25 14:42:55', changes: '  7', cmd: './cleanup.sh' },
]

export function Ledger() {
  const reduce = useReducedMotion()

  return (
    <section id="history">
      <div className="wrap">
        <Reveal>
          <h2>Every command that changed something is one entry.</h2>
          <p className="lead">
            Not just the last one. <b>undo list</b> is the filesystem history
            of your session, newest first, one command per row:
          </p>
        </Reveal>
        <motion.pre
          className="panel ledger-pre"
          initial={reduce ? false : 'hidden'}
          whileInView="show"
          viewport={{ once: true, amount: 0.35 }}
          variants={{
            hidden: {},
            show: { transition: { staggerChildren: 0.08, delayChildren: 0.1 } },
          }}
        >
          <motion.span
            style={{ display: 'block' }}
            variants={{
              hidden: reduce ? {} : { opacity: 0 },
              show: { opacity: 1, transition: { duration: 0.3 } },
            }}
          >
            $ undo list
          </motion.span>
          {ROWS.map((row) => (
            <motion.span
              key={row.id}
              style={{ display: 'block' }}
              variants={{
                hidden: reduce ? {} : { opacity: 0, x: -10 },
                show: {
                  opacity: 1,
                  x: 0,
                  transition: { duration: 0.4, ease: EASE },
                },
              }}
            >
              {row.undone ? <span className="u">u</span> : ' '}{' '}
              <span className="n">{row.id}</span>
              {'  '}
              {row.when}
              {'  '}
              {row.changes} changes  {row.cmd}
            </motion.span>
          ))}
        </motion.pre>
        <Reveal delay={0.1}>
          <p className="after">
            Any row is a target: <code>undo apply 178471899</code> reverts the
            mv from an hour ago without touching anything after it. The{' '}
            <b>u</b> marks a session that is currently undone, waiting for a
            possible <code>undo redo</code>. The last 30 sessions are kept,
            within a 1 GiB budget, and <code>undo purge</code> wipes the store.
          </p>
        </Reveal>
      </div>
    </section>
  )
}
