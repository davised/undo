import { useState } from 'react'

import { CopyButton } from './CopyButton'
import { Reveal } from './Reveal'

interface Method {
  id: string
  label: string
  note: string
  lines: Array<{ cmd: string; comment?: string }>
}

type ShellId = 'zsh' | 'bash' | 'fish'

const SHELLS: Array<{ id: ShellId; label: string; rc: string; reload: string }> = [
  { id: 'zsh', label: 'zsh', rc: '~/.zshrc', reload: 'exec zsh' },
  { id: 'bash', label: 'bash', rc: '~/.bashrc', reload: 'exec bash' },
  {
    id: 'fish',
    label: 'fish',
    rc: '~/.config/fish/config.fish',
    reload: 'exec fish',
  },
]

// Where the hooks land depends on who installed them: the one-liner and
// `make install` use ~/.local, distro packages /usr, and Homebrew its own
// prefix. Pointing brew users at ~/.local was issue #5.
type Where = 'user' | 'system' | 'brew'

const hookCmd = (sh: ShellId, where: Where) => {
  const rc = SHELLS.find((s) => s.id === sh)!.rc
  if (where === 'brew') {
    // double quotes so the prefix is resolved once, now, and the rc ends up
    // with a plain path. Left unexpanded it would run brew, a Ruby program,
    // on every shell startup. $(brew --prefix)/share is the linked path, so
    // it keeps working across upgrades. fish 3.4+ understands $() too.
    return `echo "source $(brew --prefix)/share/undo/undo.${sh}" >> ${rc}`
  }
  const dir = where === 'system' ? '/usr/share/undo' : '~/.local/share/undo'
  return `echo 'source ${dir}/undo.${sh}' >> ${rc}`
}

const methodsFor = (sh: ShellId): Method[] => [
  {
    id: 'curl',
    label: 'Any distro',
    note: 'No root. Offers to set up your shell, and asks first.',
    lines: [
      { cmd: 'curl -fsSL https://undo.edaywalid.com/install.sh | sh' },
      { cmd: SHELLS.find((s) => s.id === sh)!.reload, comment: 'or just open a new terminal' },
    ],
  },
  {
    id: 'brew',
    label: 'Homebrew',
    note: 'Linuxbrew. Add the hook line yourself, then reload.',
    lines: [
      { cmd: 'brew install edaywalid/tap/undo' },
      { cmd: hookCmd(sh, 'brew') },
      { cmd: SHELLS.find((s) => s.id === sh)!.reload },
    ],
  },
  {
    id: 'arch',
    label: 'Arch',
    note: 'From the AUR. Add the hook line yourself, then reload.',
    lines: [
      { cmd: 'yay -S undo-cli-bin', comment: 'or: paru -S undo-cli-bin' },
      { cmd: hookCmd(sh, 'system') },
      { cmd: SHELLS.find((s) => s.id === sh)!.reload },
    ],
  },
  {
    id: 'deb',
    label: 'Debian / Ubuntu',
    note: '.deb from the releases page. Hook line is manual here.',
    lines: [
      { cmd: 'sudo dpkg -i undo_*_linux_amd64.deb' },
      { cmd: hookCmd(sh, 'system') },
      { cmd: SHELLS.find((s) => s.id === sh)!.reload },
    ],
  },
  {
    id: 'rpm',
    label: 'Fedora / openSUSE',
    note: '.rpm from the releases page. Hook line is manual here.',
    lines: [
      { cmd: 'sudo rpm -i undo_*_linux_amd64.rpm' },
      { cmd: hookCmd(sh, 'system') },
      { cmd: SHELLS.find((s) => s.id === sh)!.reload },
    ],
  },
  {
    id: 'source',
    label: 'From source',
    note: 'Needs gcc and go. Installs into ~/.local.',
    lines: [
      { cmd: 'git clone https://github.com/edaywalid/undo && cd undo' },
      { cmd: 'make install' },
      { cmd: hookCmd(sh, 'user') },
    ],
  },
]

export function Install() {
  const [active, setActive] = useState('curl')
  const [shell, setShell] = useState<ShellId>('zsh')
  const methods = methodsFor(shell)
  const method = methods.find((m) => m.id === active) ?? methods[0]
  const script = method.lines.map((l) => l.cmd).join('\n')

  return (
    <section id="install">
      <div className="wrap">
        <Reveal>
          <h2>One command. That is the entire setup.</h2>
          <p className="lead">
            The installer offers to add the hook line to your shell rc,
            showing it to you first. Decline and it just prints the line.
            No root for the default, no account, no daemon.
          </p>
        </Reveal>

        <Reveal delay={0.04}>
          <div className="picker" role="tablist" aria-label="Install method">
            {methods.map((m) => (
              <button
                key={m.id}
                role="tab"
                aria-selected={m.id === active}
                className={m.id === active ? 'chip on' : 'chip'}
                onClick={() => setActive(m.id)}
              >
                {m.label}
              </button>
            ))}
          </div>
        </Reveal>

        <Reveal delay={0.07}>
          <div className="installer">
            <div className="installer-bar">
              <span className="installer-note">{method.note}</span>
              <div className="shell-pick" role="group" aria-label="Your shell">
                {SHELLS.map((s) => (
                  <button
                    key={s.id}
                    type="button"
                    aria-pressed={s.id === shell}
                    className={s.id === shell ? 'shell on' : 'shell'}
                    onClick={() => setShell(s.id)}
                  >
                    {s.label}
                  </button>
                ))}
              </div>
              <CopyButton
                className="copyall"
                text={script}
                label="Copy all"
              />
            </div>
            {/* Deliberately not AnimatePresence: it keeps the outgoing and
                incoming blocks mounted at the same time, and two stacked
                <pre>s make the card jump height for a frame, which reads as
                a blink. One element, keyed, with a CSS fade instead. */}
            <pre key={method.id} className="installer-pre">
              {method.lines.map((l, i) => (
                <div className="cmd-row" key={i}>
                  <span className="t-p">$</span>{' '}
                  <span className="t-c">{l.cmd}</span>
                  {l.comment && (
                    <span className="cmt">{'  # ' + l.comment}</span>
                  )}
                </div>
              ))}
            </pre>
          </div>
        </Reveal>

        <Reveal delay={0.09}>
          <p className="fine">
            Then prove it works. <code>undo doctor</code> deletes and restores
            a canary file, so you get a real answer instead of a promise:
          </p>
        </Reveal>
        <Reveal delay={0.11}>
          <div className="verify">
            <pre>
              <span className="t-p">$</span> <span className="t-c">undo doctor</span>
              {'\n'}
              <span className="cmt">[ok ] capture: 1 change recorded</span>
              {'\n'}
              <span className="t-ok">[ok ] restore: canary recovered intact</span>
            </pre>
          </div>
        </Reveal>
        <Reveal delay={0.13}>
          <p className="fine">
            Packages live on the{' '}
            <a href="https://github.com/edaywalid/undo/releases">
              releases page
            </a>
            . Linux, amd64 and arm64, glibc 2.6 and up. Update any time with{' '}
            <code>undo upgrade</code>, and remove it cleanly with{' '}
            <code>undo uninstall</code>, which takes its lines back out of
            your shell rc and leaves your backups alone. MIT licensed.
          </p>
        </Reveal>
      </div>
    </section>
  )
}
