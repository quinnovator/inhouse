// Tiny shared stores outside React: whether the control node answers,
// transient toasts, and a clock for relative times.

import { useSyncExternalStore } from 'react'

function store<T>(initial: T) {
  let value = initial
  const subscribers = new Set<() => void>()
  return {
    get: () => value,
    set(next: T) {
      if (next === value) return
      value = next
      subscribers.forEach((f) => f())
    },
    subscribe(f: () => void) {
      subscribers.add(f)
      return () => subscribers.delete(f)
    },
  }
}

export const connection = store(true)

export function useConnected() {
  return useSyncExternalStore(connection.subscribe, connection.get, () => true)
}

export type Toast = { id: number; message: string; tone?: 'bad' }
const toasts = store<Toast[]>([])
const none: Toast[] = []
let lastToast = 0

export function toast(message: string, tone?: 'bad') {
  const t = { id: ++lastToast, message, tone }
  toasts.set([...toasts.get(), t])
  setTimeout(() => toasts.set(toasts.get().filter((x) => x.id !== t.id)), 6000)
}

export function useToasts() {
  return useSyncExternalStore(toasts.subscribe, toasts.get, () => none)
}

// The clock ticks every 30 seconds so "3 min ago" and "in 5h" stay true
// without refetching anything.
const clock = store(Date.now() / 1000)
let ticking = false

export function useNow() {
  if (!ticking && typeof window !== 'undefined') {
    ticking = true
    setInterval(() => clock.set(Date.now() / 1000), 30_000)
  }
  return useSyncExternalStore(clock.subscribe, clock.get, clock.get)
}
