import { useCallback, useEffect, useRef, useState } from 'react'
import { motion, useReducedMotion } from 'motion/react'
import { TypedTerminal } from './TypedTerminal'
import { CopyButton } from './CopyButton'
import { EASE } from './Reveal'

const INSTALL = 'curl -fsSL https://undo.edaywalid.com/install.sh | sh'

// The compatibility answer a skeptic wants before they paste a pipe-to-shell:
// which distro, which shell, which arch.
const COMPAT = ['Ubuntu', 'Debian', 'Fedora', 'Arch', 'openSUSE', 'WSL2']
const SHELLS = ['zsh', 'bash', 'fish']

export function Hero() {
  const reduce = useReducedMotion()
  const [flash, setFlash] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    return () => {
      if (timer.current) clearTimeout(timer.current)
    }
  }, [])

  const onCopied = useCallback(() => {
    setFlash(true)
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => setFlash(false), 700)
  }, [])

  // Offset only, never opacity: the hero has to be readable when JS never
  // runs, and an opacity-0 default leaves the whole page blank if it doesn't.
  const rise = (delay: number) => ({
    initial: reduce ? false : { y: 16 },
    animate: { y: 0 },
    transition: { duration: 0.6, ease: EASE, delay },
  })

  return (
    <header className="hero" id="top">
      <div className="wrap hero-center">
        <motion.div {...rise(0)}>
          <h1>
            One command destroyed it. One command <em>brought it back.</em>
          </h1>
          <p className="lead">
            <b>undo</b> reverts what the last shell command did to the
            filesystem. No daemon, no snapshots, no new habits.
          </p>
        </motion.div>

        {/* The one action. Above the fold, full measure, one click. */}
        <motion.div className="hero-act" {...rise(0.1)}>
          <div className={flash ? 'hero-install lit' : 'hero-install'}>
            <code className="hero-install-cmd">
              <span className="t-p">$</span>{' '}
              <span className="t-c">{INSTALL}</span>
            </code>
            <CopyButton
              className="hero-copy-btn"
              text={INSTALL}
              label="Copy"
              onCopied={onCopied}
            />
          </div>

          <p className="hero-compat">
            {COMPAT.join(' · ')}
            <span className="hero-compat-sep" aria-hidden="true" />
            {SHELLS.join(' · ')}
            <span className="hero-compat-sep" aria-hidden="true" />
            amd64 &amp; arm64
          </p>
        </motion.div>

        <motion.div className="hero-ctas" {...rise(0.16)}>
          <a className="btn btn-ghost" href="https://github.com/edaywalid/undo">
            View on GitHub
          </a>
          <a className="hero-alt" href="#install">
            Homebrew, AUR, .deb, .rpm, source
          </a>
        </motion.div>

        <motion.div className="hero-term" {...rise(0.24)}>
          <TypedTerminal />
        </motion.div>
      </div>
    </header>
  )
}
