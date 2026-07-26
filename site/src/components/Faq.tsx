import type { ReactNode } from 'react'
import { useState } from 'react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { Reveal, EASE } from './Reveal'

interface QA {
  q: string
  a: ReactNode
}

const ITEMS: QA[] = [
  {
    q: 'Is this just another trash can for rm?',
    a: (
      <p>
        No. Trash tools replace <code>rm</code> with a different command and
        only cover deletion. undo keeps your habits and covers the rest of the
        accident surface: <b>mv over a file you needed, &gt; truncating a
        config, chmod -R gone wrong, a script that cleaned too much</b>. And it
        works when the deletion happens three processes deep inside a build
        tool, where no alias can save you.
      </p>
    ),
  },
  {
    q: 'What does it cost while I work?',
    a: (
      <p>
        Between commands: nothing runs. During a command: a few extra syscalls
        per destructive operation, and deletions are saved by hardlink, which
        copies no data. The shell hook adds roughly a millisecond around each
        command. If a command changed nothing, its session is deleted on the
        spot.
      </p>
    ),
  },
  {
    q: 'What about sudo, Go binaries, static tools?',
    a: (
      <>
        <p>
          Not covered, and we say so on the tin. LD_PRELOAD reaches dynamically
          linked programs that go through libc: that is coreutils and your
          shell, which is where the everyday accidents live. <b>sudo strips
          LD_PRELOAD</b>, Go programs make raw syscalls, static binaries load
          nothing. For scripts outside the hooked shell there is{' '}
          <code>undo run</code>.
        </p>
        <p>It is a safety net, not a backup. Keep backups.</p>
      </>
    ),
  },
  {
    q: 'Why not btrfs or zfs snapshots?',
    a: (
      <p>
        If you have them configured, keep them, they are great. But snapshots
        are time-granular: rolling back to 15 minutes ago also reverts the work
        you wanted to keep, and the accident has to land on the right side of
        the snapshot interval. undo is <b>command-granular</b>: it reverts
        exactly what that one command touched, on any filesystem, including the
        ext4 machine you actually broke.
      </p>
    ),
  },
  {
    q: 'Does it run on macOS, or my distro?',
    a: (
      <>
        <p>
          undo is <b>Linux, amd64 and arm64</b>: any glibc distro (Debian,
          Ubuntu, Fedora, Arch, openSUSE, and friends), and WSL2 counts, it is
          Linux. Alpine and other musl systems work when built from source;
          the prebuilt shim targets glibc. Hooks ship for zsh, bash 5+, and
          fish 3.4+; every other shell can use <code>undo run</code>.
        </p>
        <p>
          macOS is out: SIP blocks library injection into system binaries, so
          the mechanism cannot cover <code>rm</code> there. Windows is WSL2
          only. Snap and Flatpak will never happen, sandboxes and an
          LD_PRELOAD hook are architecturally incompatible.
        </p>
      </>
    ),
  },
  {
    q: 'Can I see what it will do before it does it?',
    a: (
      <p>
        Always. undo prints the change list and asks before reverting,{' '}
        <code>undo diff</code> shows content diffs against the backups,{' '}
        <code>-n</code> is a dry run, and <code>undo -i</code> lets you
        cherry-pick single files out of a session.
      </p>
    ),
  },
]

function QaItem({ item, defaultOpen }: { item: QA; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(defaultOpen ?? false)
  const reduce = useReducedMotion()
  const panelId = `qa-${item.q.replace(/\W+/g, '-').toLowerCase()}`

  return (
    <div className="qa-item">
      <button
        type="button"
        className="qa-q"
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="marker" aria-hidden="true">
          {open ? '-' : '+'}
        </span>
        {item.q}
      </button>
      <AnimatePresence initial={false}>
        {open && (
          <motion.div
            id={panelId}
            className="qa-a"
            initial={reduce ? false : { height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={reduce ? undefined : { height: 0, opacity: 0 }}
            transition={{ duration: 0.32, ease: EASE }}
          >
            <div className="qa-a-inner">{item.a}</div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  )
}

export function Faq() {
  return (
    <section id="faq">
      <div className="wrap">
        <Reveal>
          <h2>You are right to be skeptical.</h2>
        </Reveal>
        <Reveal delay={0.04}>
          <div className="qa">
            {ITEMS.map((item, i) => (
              <QaItem key={item.q} item={item} defaultOpen={i === 0} />
            ))}
          </div>
        </Reveal>
      </div>
    </section>
  )
}
