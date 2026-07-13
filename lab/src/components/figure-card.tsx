// Figure chrome shared by every figure: title row, optional right-side
// slot (legend or buttons), body, and footnote.

import type { ReactNode } from 'react'

export function FigureCard({
  title,
  aside,
  children,
  note,
}: {
  title: string
  aside?: ReactNode
  children: ReactNode
  note?: ReactNode
}) {
  return (
    <div className="fig-card">
      <div className="fig-head">
        <div className="fig-title">{title}</div>
        {aside}
      </div>
      {children}
      {note && <div className="fig-note">{note}</div>}
    </div>
  )
}
