const LINKS = [
  { href: '#how', label: 'How it works', optional: true },
  { href: '#history', label: 'History', optional: true },
  { href: '#faq', label: 'FAQ', optional: true },
  { href: 'https://github.com/edaywalid/undo', label: 'GitHub', optional: false },
]

export function Nav() {
  return (
    <nav className="nav">
      <div className="wrap nav-inner">
        <a className="brand" href="#top" aria-label="undo, back to top">
          undo
        </a>
        <div className="nav-links">
          {LINKS.map((l) => (
            <a key={l.href} href={l.href} className={l.optional ? 'opt' : undefined}>
              {l.label}
            </a>
          ))}
        </div>
        <a className="nav-cta" href="#install">
          Install
        </a>
      </div>
    </nav>
  )
}
