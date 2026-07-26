import { motion, useReducedMotion } from 'motion/react'
import { TypedTerminal } from './TypedTerminal'
import { EASE } from './Reveal'

export function Hero() {
  const reduce = useReducedMotion()

  return (
    <header className="hero" id="top">
      <div className="wrap hero-grid">
        <div className="hero-copy">
          <motion.div
            initial={reduce ? false : { y: 16 }}
            animate={{ y: 0 }}
            transition={{ duration: 0.6, ease: EASE }}
          >
            <h1>
              One command destroyed it. One command{' '}
              <em>brought it back.</em>
            </h1>
            <p className="lead">
              <b>undo</b> reverts what the last shell command did to the
              filesystem. No daemon, no snapshots, no new habits.
            </p>
          </motion.div>

          <motion.div
            className="hero-ctas"
            initial={reduce ? false : { y: 16 }}
            animate={{ y: 0 }}
            transition={{ duration: 0.6, ease: EASE, delay: 0.12 }}
          >
            <a className="btn btn-hot" href="#install">
              Install
            </a>
            <a className="btn btn-ghost" href="https://github.com/edaywalid/undo">
              View on GitHub
            </a>
          </motion.div>
        </div>

        <motion.div
          className="hero-term"
          initial={reduce ? false : { scale: 0.985 }}
          animate={{ scale: 1 }}
          transition={{ duration: 0.6, ease: EASE, delay: 0.2 }}
        >
          <TypedTerminal />
        </motion.div>
      </div>
    </header>
  )
}
