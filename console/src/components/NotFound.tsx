import { Link } from '@tanstack/react-router'

export function NotFound() {
  return (
    <div className="card">
      <div className="empty">
        <strong>Nothing here</strong>
        <Link to="/">Back to services</Link>
      </div>
    </div>
  )
}
