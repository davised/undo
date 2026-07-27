import type { ReactNode } from 'react'
import { motion, useReducedMotion } from 'motion/react'

const EASE = [0.23, 1, 0.32, 1] as const

interface RevealProps {
  children: ReactNode
  delay?: number
  className?: string
}

/**
 * Slides content up once when it enters the viewport.
 *
 * Offset only, never opacity. `initial` is what the server renders, so an
 * opacity-0 default leaves every section below the hero blank whenever JS
 * does not run. Sliding from an already-visible default degrades to plain
 * readable content instead.
 */
export function Reveal({ children, delay = 0, className }: RevealProps) {
  const reduce = useReducedMotion()
  return (
    <motion.div
      className={className}
      initial={reduce ? false : { y: 12 }}
      whileInView={{ y: 0 }}
      // fire a little before the block is fully in view so it has finished
      // by the time the reader gets to it, instead of animating under them
      viewport={{ once: true, amount: 0.15, margin: '0px 0px -8% 0px' }}
      transition={{ duration: 0.34, ease: EASE, delay }}
    >
      {children}
    </motion.div>
  )
}

export { EASE }
