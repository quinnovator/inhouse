import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createRouter } from '@tanstack/react-router'
import { NotFound } from './components/NotFound'
import { ApiError } from './lib/api'
import { connection } from './lib/stores'
import { routeTree } from './routeTree.gen'

export function getRouter() {
  const queryClient = new QueryClient({
    // Any answer from the control node means it's reachable; only network
    // failures and 5xx turn the header's "Reconnecting…" on.
    queryCache: new QueryCache({
      onSuccess: () => connection.set(true),
      onError: (err) => {
        if (err instanceof ApiError && err.unreachable) connection.set(false)
      },
    }),
    defaultOptions: {
      queries: {
        staleTime: 2000,
        // Refusals and missing things won't change on retry.
        retry: (count, err) => err instanceof ApiError && err.unreachable && count < 3,
      },
    },
  })

  return createRouter({
    routeTree,
    context: { queryClient },
    defaultNotFoundComponent: NotFound,
    scrollRestoration: true,
    Wrap: ({ children }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>,
  })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof getRouter>
  }
}
