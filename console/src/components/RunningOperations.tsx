import { Link } from '@tanstack/react-router'
import { Pill, Time } from './ui'
import { actor } from '~/lib/format'
import type { Operation } from '~/lib/types'

export function RunningOperations({ operations, service = true }: { operations: Operation[]; service?: boolean }) {
  if (!operations.length) return <p className="sub">No operations running.</p>
  return (
    <div className="table-wrap">
      <table className={service ? 'grid operations-grid' : 'grid service-operations-grid'}>
        <thead>
          <tr>
            {service && <th scope="col">Service</th>}
            <th scope="col">Operation</th>
            <th scope="col">Started</th>
            <th scope="col">By</th>
          </tr>
        </thead>
        <tbody>
          {operations.map((op) => (
            <tr key={op.operation_id}>
              {service && <td><Link to="/services/$name" params={{ name: op.service }}>{op.service}</Link></td>}
              <td>
                <div className="row"><Pill tone="busy">Running</Pill><strong>{op.kind}</strong>{op.rev ? <span className="mono">r{op.rev}</span> : null}</div>
                <div className="mono sub wrap-anywhere">{op.operation_id}</div>
              </td>
              <td><Time ts={op.created_at} as="ago" /></td>
              <td className="wrap-anywhere">{actor(op.created_by)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
