import { createFileRoute } from '@tanstack/react-router'
import { Nav } from '../components/Nav'
import { Hero } from '../components/Hero'
import { Trace } from '../components/Trace'
import { Ledger } from '../components/Ledger'
import { Install } from '../components/Install'
import { Faq } from '../components/Faq'
import { Footer } from '../components/Footer'

export const Route = createFileRoute('/')({
  component: LandingPage,
})

function LandingPage() {
  return (
    <>
      <Nav />
      <main>
        <Hero />
        <Trace />
        <Ledger />
        <Install />
        <Faq />
      </main>
      <Footer />
    </>
  )
}
