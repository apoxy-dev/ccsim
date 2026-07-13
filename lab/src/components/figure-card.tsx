// Figure chrome shared by every figure: title row, optional right-side
// slot (legend or buttons), optional control strip, body, and footnote.

import type { ReactNode } from 'react'

export function FigureCard({
  title,
  aside,
  controls,
  children,
  note,
}: {
  title: string
  aside?: ReactNode
  controls?: ReactNode
  children: ReactNode
  note?: ReactNode
}) {
  return (
    <div className="fig-card">
      <div className="fig-head">
        <div className="fig-title">{title}</div>
        {aside}
      </div>
      {controls && <div className="fig-controls">{controls}</div>}
      {children}
      {note && <div className="fig-note">{note}</div>}
    </div>
  )
}
