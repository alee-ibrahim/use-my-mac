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

  return { data, error, refresh, updatedAt, setData }
}

// A live stream: `url` is a server-sent event stream; `apply` folds each
// event (JSON data) into the current value. Events named in `closeOn` end
// the stream (it would otherwise reconnect).
export type LiveStream<T> = {
  url?: string
  events: string[]
  apply: (event: string, data: unknown, prev: T | undefined) => T | undefined
  closeOn?: string[]
}

// useLive is usePolling plus a server-sent event stream: while the stream
// is connected, changes arrive as they happen and polling pauses; when it is
// not (old router, proxy that buffers, network blip) polling takes over.
export function useLive<T>(load: () => Promise<T>, ms: number, stream: LiveStream<T>) {
  const [live, setLive] = useState(false)
  const poll = usePolling(load, ms, { paused: live })
  const applyRef = useRef(stream.apply)
  useEffect(() => {
    applyRef.current = stream.apply
  }, [stream.apply])
  const { setData } = poll
  const { url } = stream
  const events = stream.events.join(",")
  const closeOn = (stream.closeOn ?? []).join(",")
  useEffect(() => {
    if (!url || typeof EventSource === "undefined") return
    const es = new EventSource(url, { withCredentials: true })
    // EventSource reconnects by itself after errors; poll meanwhile.
    es.onerror = () => setLive(false)
    for (const name of events.split(",")) {
      es.addEventListener(name, (e) => {
        let d: unknown
        try {
          d = JSON.parse((e as MessageEvent<string>).data)
        } catch {
          return
        }
        setData((prev) => applyRef.current(name, d, prev))
        setLive(true)
      })
    }
    for (const name of closeOn.split(",").filter(Boolean)) {
      es.addEventListener(name, () => {
        es.close()
        setLive(false)
      })
    }
    return () => {
      es.close()
      setLive(false)
    }
  }, [url, events, closeOn, setData])
  return { ...poll, live }
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
