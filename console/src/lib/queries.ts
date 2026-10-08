// Live data from /v1. Lists poll; the event feed asks only for what's new;
// operations long-poll until they finish.

import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ApiError, api } from './api'
import type { Event, Operation } from './types'

export function useWhoami() {
  return useQuery({ queryKey: ['whoami'], queryFn: ({ signal }) => api.whoami(signal), staleTime: Infinity })
}

export function useServices() {
  return useQuery({ queryKey: ['services'], queryFn: ({ signal }) => api.services(signal), refetchInterval: 5000 })
}

export function useService(name: string) {
  return useQuery({
    queryKey: ['service', name],
    queryFn: ({ signal }) => api.service(name, signal),
    // A deleted service stays gone; stop asking.
    refetchInterval: (q) => (q.state.error instanceof ApiError && q.state.error.status === 404 ? false : 4000),
  })
}

export function useSecrets() {
  return useQuery({ queryKey: ['secrets'], queryFn: ({ signal }) => api.secrets(signal) })
}

// A feed is the newest events first; fresh holds the ids that arrived in
// the latest poll, for highlighting.
export type Feed = { items: Event[]; fresh: number[] }

// useEventFeed loads the newest `size` events (of one service, or all the
// caller can see), then polls for events after the newest it has. New
// events also refresh the service data they describe.
export function useEventFeed(service: string | undefined, size: number, keep = size) {
  const client = useQueryClient()
  const key = ['events', service ?? '', size]
  return useQuery({
    queryKey: key,
    queryFn: async ({ signal }): Promise<Feed> => {
      const prev = client.getQueryData<Feed>(key)
      const last = prev?.items[0]?.id
      const got = await api.events(last ? { service, since: last, limit: 100 } : { service, limit: size }, signal)
      if (!prev) return { items: got.reverse(), fresh: [] }
      if (!got.length) return prev
      void client.invalidateQueries({ queryKey: service ? ['service', service] : ['services'] })
      const ids = new Set(got.map((e) => e.id))
      return {
        items: [...got.reverse(), ...prev.items.filter((e) => !ids.has(e.id))].slice(0, keep),
        fresh: [...ids],
      }
    },
    refetchInterval: 4000,
  })
}

// useOperation follows an operation until it finishes. Each request waits
// up to 20 seconds for a change, so this is a long poll, not a busy loop.
export function useOperation(initial: Operation | undefined) {
  return useQuery({
    queryKey: ['operation', initial?.operation_id],
    queryFn: ({ signal }) => api.operation(initial!.operation_id, 20, signal),
    enabled: !!initial,
    initialData: initial,
    initialDataUpdatedAt: 0,
    staleTime: 0,
    refetchInterval: (q) => (q.state.data?.state === 'running' ? 250 : false),
    refetchIntervalInBackground: true,
  })
}
