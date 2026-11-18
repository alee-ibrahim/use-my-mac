// Built-in documentation at /docs. The router embeds the project's Markdown
// files and serves them without login; this page renders them.

import { useEffect, useMemo, useState } from "react"
import GithubSlugger from "github-slugger"
import {
  BookOpenIcon,
  ExternalLinkIcon,
  LayoutDashboardIcon,
} from "lucide-react"
import Markdown, { type Components } from "react-markdown"
import rehypeSlug from "rehype-slug"
import remarkGfm from "remark-gfm"

import { ThemeToggle } from "@/components/theme-toggle"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty"
import { Separator } from "@/components/ui/separator"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  SidebarProvider,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { TooltipProvider } from "@/components/ui/tooltip"
import { api, type DocInfo } from "@/lib/api"

const REPO_URL = "https://github.com/alee-ibrahim/use-my-mac"

function slugFromPath(p: string) {
  return decodeURIComponent(p.replace(/^\/docs\/?/, "").replace(/\/+$/, ""))
}

type Heading = { id: string; text: string }

// h2 headings with the same ids rehype-slug gives them (one slugger over every
// heading, in order, ignoring fenced code).
function headings(md: string): Heading[] {
  const slugger = new GithubSlugger()
  const out: Heading[] = []
  let fence = false
  for (const line of md.split("\n")) {
    if (/^\s*(```|~~~)/.test(line)) fence = !fence
    if (fence) continue
    const m = /^(#{1,6})\s+(.+?)\s*#*\s*$/.exec(line)
    if (!m) continue
    const text = m[2]
      .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
      .replace(/[`*_]/g, "")
      .trim()
    const id = slugger.slug(text)
    if (m[1].length === 2) out.push({ id, text })
  }
  return out
}

// Resolves a link inside a doc: other embedded docs stay in /docs, other
// repository files go to the repository, everything else is left alone.
function resolveHref(href: string, from: DocInfo, docs: DocInfo[]) {
  if (!href || href.startsWith("#") || /^[a-z][a-z0-9+.-]*:/i.test(href)) {
    return { href, internal: false }
  }
  const u = new URL(href, "https://repo.invalid/" + from.path)
  const path = decodeURIComponent(u.pathname.slice(1))
  const target = docs.find((d) => d.path === path)
  if (target) return { href: `/docs/${target.slug}${u.hash}`, internal: true }
  return { href: `${REPO_URL}/blob/main/${path}${u.hash}`, internal: false }
}

// react-markdown passes its AST node as a prop; keep it off the DOM.
function rest<T extends { node?: unknown }>(props: T): Omit<T, "node"> {
  const { node, ...r } = props
  void node
  return r
}

function markdownComponents(
  doc: DocInfo,
  docs: DocInfo[],
  go: (href: string) => void
): Components {
  return {
    h1: (p) => (
      <h1
        className="scroll-m-20 text-3xl font-extrabold tracking-tight text-balance lg:text-4xl"
        {...rest(p)}
      />
    ),
    h2: (p) => (
      <h2
        className="mt-10 scroll-m-20 border-b pb-2 text-2xl font-semibold tracking-tight first:mt-0"
        {...rest(p)}
      />
    ),
    h3: (p) => (
      <h3
        className="mt-8 scroll-m-20 text-xl font-semibold tracking-tight"
        {...rest(p)}
      />
    ),
    h4: (p) => (
      <h4
        className="mt-6 scroll-m-20 text-lg font-semibold tracking-tight"
        {...rest(p)}
      />
    ),
    p: (p) => (
      <p className="leading-7 [&:not(:first-child)]:mt-6" {...rest(p)} />
    ),
    ul: (p) => <ul className="my-6 ml-6 list-disc [&>li]:mt-2" {...rest(p)} />,
    ol: (p) => (
      <ol className="my-6 ml-6 list-decimal [&>li]:mt-2" {...rest(p)} />
    ),
    blockquote: (p) => (
      <blockquote
        className="mt-6 border-l-2 pl-6 text-muted-foreground italic"
        {...rest(p)}
      />
    ),
    hr: (p) => <Separator className="my-8" {...rest(p)} />,
    pre: (p) => (
      <pre
        className="mt-6 overflow-x-auto rounded-lg border bg-muted p-4 font-mono text-sm leading-relaxed [&>code]:bg-transparent [&>code]:p-0 [&>code]:font-normal"
        {...rest(p)}
      />
    ),
    code: (p) => (
      <code
        className="relative rounded bg-muted px-[0.3rem] py-[0.2rem] font-mono text-sm font-semibold"
        {...rest(p)}
      />
    ),
    table: (p) => (
      <div className="my-6 rounded-lg border">
        <Table {...rest(p)} />
      </div>
    ),
    thead: (p) => <TableHeader {...rest(p)} />,
    tbody: (p) => <TableBody {...rest(p)} />,
    tr: (p) => <TableRow {...rest(p)} />,
    th: (p) => <TableHead className="whitespace-normal" {...rest(p)} />,
    td: (p) => (
      <TableCell className="align-top whitespace-normal" {...rest(p)} />
    ),
    a: ({ href = "", children, ...p }) => {
      const r = resolveHref(href, doc, docs)
      const external = /^https?:/.test(r.href)
      return (
        <a
          className="font-medium text-primary underline underline-offset-4"
          href={r.href}
          {...(external ? { target: "_blank", rel: "noreferrer" } : {})}
          onClick={(e) => {
            if (
              !r.internal ||
              e.metaKey ||
              e.ctrlKey ||
              e.shiftKey ||
              e.button !== 0
            )
              return
            e.preventDefault()
            go(r.href)
          }}
          {...rest(p)}
        >
          {children}
        </a>
      )
    },
    img: ({ alt, ...p }) => (
      <img
        className="my-6 max-w-full rounded-lg border"
        alt={alt ?? ""}
        {...rest(p)}
      />
    ),
  }
}

function DocsNav({
  docs,
  current,
  toc,
  go,
}: {
  docs: DocInfo[]
  current?: DocInfo
  toc: Heading[]
  go: (href: string) => void
}) {
  const { setOpenMobile } = useSidebar()
  const nav = (href: string) => {
    setOpenMobile(false)
    go(href)
  }
  return (
    <Sidebar>
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" asChild>
              <a
                href="/docs"
                onClick={(e) => {
                  e.preventDefault()
                  nav("/docs")
                }}
              >
                <BookOpenIcon />
                <div className="flex flex-col leading-tight">
                  <span className="font-semibold">use-my-mac</span>
                  <span className="text-xs text-muted-foreground">
                    Documentation
                  </span>
                </div>
              </a>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Guides</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {docs.length === 0 &&
                Array.from({ length: 4 }, (_, i) => (
                  <SidebarMenuItem key={i}>
                    <Skeleton className="h-8 w-full" />
                  </SidebarMenuItem>
                ))}
              {docs.map((d) => {
                const active = d.slug === current?.slug
                return (
                  <SidebarMenuItem key={d.slug}>
                    <SidebarMenuButton asChild isActive={active}>
                      <a
                        href={`/docs/${d.slug}`}
                        onClick={(e) => {
                          e.preventDefault()
                          nav(`/docs/${d.slug}`)
                        }}
                      >
                        <span>{d.title}</span>
                      </a>
                    </SidebarMenuButton>
                    {active && toc.length > 0 && (
                      <SidebarMenuSub>
                        {toc.map((h) => (
                          <SidebarMenuSubItem key={h.id}>
                            <SidebarMenuSubButton
                              href={`#${h.id}`}
                              onClick={() => setOpenMobile(false)}
                            >
                              <span>{h.text}</span>
                            </SidebarMenuSubButton>
                          </SidebarMenuSubItem>
                        ))}
                      </SidebarMenuSub>
                    )}
                  </SidebarMenuItem>
                )
              })}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild>
              <a href="/">
                <LayoutDashboardIcon />
                <span>Open the dashboard</span>
              </a>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <SidebarMenuButton asChild>
              <a href={REPO_URL} target="_blank" rel="noreferrer">
                <ExternalLinkIcon />
                <span>Source code</span>
              </a>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  )
}

export function DocsApp() {
  const [docs, setDocs] = useState<DocInfo[]>([])
  const [path, setPath] = useState(location.pathname)
  const [md, setMd] = useState<{ slug: string; text: string }>()
  const [error, setError] = useState<string>()

  useEffect(() => {
    api
      .docs()
      .then(setDocs, (e) =>
        setError(e instanceof Error ? e.message : String(e))
      )
    const onPop = () => setPath(location.pathname)
    window.addEventListener("popstate", onPop)
    return () => window.removeEventListener("popstate", onPop)
  }, [])

  const slug = slugFromPath(path) || docs[0]?.slug
  const current = docs.find((d) => d.slug === slug)

  useEffect(() => {
    if (!current) return
    let stale = false
    api.doc(current.slug).then(
      (text) => !stale && setMd({ slug: current.slug, text }),
      (e) => !stale && setError(e instanceof Error ? e.message : String(e))
    )
    return () => {
      stale = true
    }
  }, [current])

  const text = md && md.slug === current?.slug ? md.text : undefined
  const toc = useMemo(() => (text ? headings(text) : []), [text])

  useEffect(() => {
    document.title = current
      ? `${current.title} · use-my-mac docs`
      : "use-my-mac docs"
  }, [current])

  // After a page renders, honour the URL fragment or start at the top.
  useEffect(() => {
    if (text === undefined) return
    const id = decodeURIComponent(location.hash.slice(1))
    const el = id ? document.getElementById(id) : null
    if (el) el.scrollIntoView()
    else window.scrollTo(0, 0)
  }, [text])

  const go = (href: string) => {
    const u = new URL(href, location.origin)
    if (u.pathname + u.hash === location.pathname + location.hash) return
    history.pushState(null, "", u.pathname + u.hash)
    setPath(u.pathname)
    if (u.pathname === path && u.hash)
      document
        .getElementById(decodeURIComponent(u.hash.slice(1)))
        ?.scrollIntoView()
  }

  const components = useMemo(
    () => (current ? markdownComponents(current, docs, go) : {}),
    // go only depends on path, which changes with current.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [current, docs]
  )

  let body: React.ReactNode
  if (error) {
    body = (
      <Alert variant="destructive">
        <AlertTitle>Could not load the documentation</AlertTitle>
        <AlertDescription>{error}</AlertDescription>
      </Alert>
    )
  } else if (docs.length > 0 && !current) {
    body = (
      <Empty>
        <EmptyHeader>
          <EmptyTitle>Page not found</EmptyTitle>
          <EmptyDescription>
            There is no page called “{slug}”. Pick one from the sidebar.
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  } else if (text === undefined) {
    body = (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-10 w-2/3" />
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-4/5" />
      </div>
    )
  } else {
    body = (
      <Markdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[rehypeSlug]}
        components={components}
      >
        {text}
      </Markdown>
    )
  }

  return (
    <TooltipProvider>
      <SidebarProvider>
        <DocsNav docs={docs} current={current} toc={toc} go={go} />
        <SidebarInset className="min-w-0">
          <header className="sticky top-0 z-10 flex h-12 items-center gap-2 border-b bg-background px-4">
            <SidebarTrigger />
            <Separator orientation="vertical" className="h-4" />
            <h1 className="truncate text-sm font-medium">
              {current?.title ?? "Documentation"}
            </h1>
            <div className="ml-auto">
              <ThemeToggle />
            </div>
          </header>
          <main className="p-4 md:p-8">
            <article className="mx-auto max-w-3xl pb-16">{body}</article>
          </main>
        </SidebarInset>
      </SidebarProvider>
    </TooltipProvider>
  )
}
