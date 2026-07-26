import type { ReactNode } from 'react'
import {
  createRootRoute,
  HeadContent,
  Outlet,
  Scripts,
} from '@tanstack/react-router'
import '@fontsource-variable/archivo'
import '@fontsource-variable/jetbrains-mono'
import appCss from '../styles.css?url'

export const Route = createRootRoute({
  head: () => ({
    meta: [
      { charSet: 'utf-8' },
      { name: 'viewport', content: 'width=device-width, initial-scale=1' },
      { title: 'undo, revert your last shell command' },
      {
        name: 'description',
        content:
          'Deleted the wrong thing? undo reverts what the last shell command did to the filesystem. No daemon, no snapshots, no new habits.',
      },
      { property: 'og:title', content: 'undo, revert your last shell command' },
      {
        property: 'og:description',
        content:
          'rm -rf the wrong directory? One word puts everything back. A safety net for the shell, on any Linux.',
      },
      { property: 'og:type', content: 'website' },
      { property: 'og:url', content: 'https://undo.edaywalid.com/' },
      { property: 'og:image', content: 'https://undo.edaywalid.com/og.png' },
      { property: 'og:image:width', content: '1200' },
      { property: 'og:image:height', content: '630' },
      {
        property: 'og:image:alt',
        content: 'undo: rm -rf thesis/, undo, restored 4 change(s)',
      },
      { name: 'twitter:card', content: 'summary_large_image' },
      { name: 'twitter:title', content: 'undo, revert your last shell command' },
      {
        name: 'twitter:description',
        content:
          'rm -rf the wrong directory? One word puts everything back. A safety net for the shell, on any Linux.',
      },
      { name: 'twitter:image', content: 'https://undo.edaywalid.com/og.png' },
    ],
    links: [
      { rel: 'stylesheet', href: appCss },
      { rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' },
    ],
  }),
  component: RootComponent,
})

function RootComponent() {
  return (
    <RootDocument>
      <Outlet />
    </RootDocument>
  )
}

function RootDocument({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <head>
        <HeadContent />
      </head>
      <body>
        {children}
        <Scripts />
      </body>
    </html>
  )
}
