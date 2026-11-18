import { useCallback, useEffect, useRef, useState } from "react"

// usePolling loads data now and then every `ms` while the tab is visible.
// It pauses while the tab is hidden (or `paused` is set) and refreshes as
// soon as the tab becomes visible again.
export function usePolling<T>(load: () => Promise<T>, ms: number, opts: { paused?: boolean } = {}) {
  const [data, setData] = useState<T | undefined>()
  const [error, setError] = useState<string | undefined>()
  const [updatedAt, setUpdatedAt] = useState<number>()
  const loadRef = useRef(load)
  useEffect(() => {
    loadRef.current = load
  }, [load])

  const refresh = useCallback(async () => {
    try {
      setData(await loadRef.current())
      setError(undefined)
      setUpdatedAt(Date.now())
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  const paused = !!opts.paused
  useEffect(() => {
    void refresh()
    if (paused) return
    const id = window.setInterval(() => {
      if (document.visibilityState === "visible") void refresh()
    }, ms)
    const onVisible = () => {
      if (document.visibilityState === "visible") void refresh()
    }
    document.addEventListener("visibilitychange", onVisible)
    return () => {
      window.clearInterval(id)
      document.removeEventListener("visibilitychange", onVisible)
    }
  }, [refresh, ms, paused])

  return { data, error, refresh, updatedAt }
}

// useNow re-renders every `ms` so relative times ("5s ago") stay fresh.
export function useNow(ms = 1000) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), ms)
    return () => window.clearInterval(id)
  }, [ms])
  return now
}

// Hash routes: #overview, #macs, #mac/<id>, #jobs, #job/<id>, #keys,
// #activity, #settings. Old bookmarks (#macs etc.) keep working.
export type Route = { page: string; id?: string }

export function parseHash(h = location.hash): Route {
  const [page, ...rest] = h.replace(/^#\/?/, "").split("/")
  return { page: page || "overview", id: rest.length ? decodeURIComponent(rest.join("/")) : undefined }
}

export function href(page: string, id?: string) {
  return `#${page}${id ? `/${encodeURIComponent(id)}` : ""}`
}

export function useRoute(): Route {
  const [route, setRoute] = useState(parseHash)
  useEffect(() => {
    const on = () => {
      setRoute(parseHash())
      window.scrollTo({ top: 0 })
    }
    window.addEventListener("hashchange", on)
    return () => window.removeEventListener("hashchange", on)
  }, [])
  return route
}
