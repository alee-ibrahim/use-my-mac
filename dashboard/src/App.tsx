import { lazy, Suspense, useEffect, useState } from "react"
import {
  ActivityIcon,
  BookOpenIcon,
  CpuIcon,
  KeyRoundIcon,
  LayoutDashboardIcon,
  ListChecksIcon,
  PackageIcon,
  LogOutIcon,
  SettingsIcon,
} from "lucide-react"

import { ThemeToggle } from "@/components/theme-toggle"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
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
  SidebarProvider,
  SidebarRail,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Toaster } from "@/components/ui/sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { api, type AuthConfig, type Me } from "@/lib/api"
import { href, useRoute } from "@/lib/hooks"
import { ActivityPage } from "@/pages/activity"
import { JobsPage } from "@/pages/jobs"
import { KeysPage } from "@/pages/keys"
import { Login, SignedOut, SigninError } from "@/pages/login"
import { MacsPage } from "@/pages/macs"
import { RuntimesPage } from "@/pages/runtimes"
import { SettingsPage } from "@/pages/settings"

// Chart pages (and Recharts) load on demand.
const OverviewPage = lazy(() => import("@/pages/overview").then((m) => ({ default: m.OverviewPage })))
const MacDetailPage = lazy(() => import("@/pages/mac-detail").then((m) => ({ default: m.MacDetailPage })))
const JobDetailPage = lazy(() => import("@/pages/job-detail").then((m) => ({ default: m.JobDetailPage })))

type NavItem = { id: string; title: string; icon: React.ComponentType; admin?: boolean; also?: string[] }

const nav: { label: string; items: NavItem[] }[] = [
  {
    label: "Operate",
    items: [
      { id: "overview", title: "Overview", icon: LayoutDashboardIcon },
      { id: "macs", title: "Macs", icon: CpuIcon, also: ["mac"] },
      { id: "jobs", title: "Jobs", icon: ListChecksIcon, also: ["job"] },
      { id: "runtimes", title: "Runtimes", icon: PackageIcon },
      { id: "activity", title: "Activity", icon: ActivityIcon },
    ],
  },
  {
    label: "Admin",
    items: [
      { id: "keys", title: "API keys", icon: KeyRoundIcon, admin: true },
      { id: "settings", title: "Settings", icon: SettingsIcon },
    ],
  },
]
const allItems = nav.flatMap((g) => g.items)

function NavMenu({ page, admin }: { page: string; admin: boolean }) {
  const { setOpenMobile } = useSidebar()
  return (
    <>
      {nav.map((g) => {
        const items = g.items.filter((p) => !p.admin || admin)
        if (!items.length) return null
        return (
          <SidebarGroup key={g.label}>
            <SidebarGroupLabel>{g.label}</SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {items.map((p) => (
                  <SidebarMenuItem key={p.id}>
                    <SidebarMenuButton
                      asChild
                      size="lg"
                      className="md:h-8"
                      isActive={p.id === page || !!p.also?.includes(page)}
                      tooltip={p.title}
                    >
                      <a href={href(p.id)} onClick={() => setOpenMobile(false)}>
                        <p.icon />
                        <span>{p.title}</span>
                      </a>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        )
      })}
    </>
  )
}

function PageFallback() {
  return (
    <div className="flex flex-col gap-4">
      <Skeleton className="h-8 w-48" />
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-24 rounded-xl" />
        ))}
      </div>
      <Skeleton className="h-64 rounded-xl" />
    </div>
  )
}

export function App() {
  const [me, setMe] = useState<Me | null | undefined>(undefined)
  const [cfg, setCfg] = useState<AuthConfig>()
  const route = useRoute()

  useEffect(() => {
    api.me().then(setMe, () => setMe(null))
    api.authConfig().then(setCfg, () => setCfg({ oidc: false, apikey: true, auto_sign_in: false }))
  }, [])

  // Signed in on one of the sign-in pages (e.g. after "back"): go home.
  useEffect(() => {
    if (me && me.role !== "app" && ["/login", "/signed-out", "/signin-error"].includes(location.pathname)) {
      history.replaceState(null, "", "/" + location.hash)
    }
  }, [me])

  if (me === undefined || (!me && !cfg)) {
    return (
      <div className="flex min-h-svh items-center justify-center">
        <Spinner />
      </div>
    )
  }
  if (me === null || me.role === "app") {
    return (
      <>
        {location.pathname === "/signin-error" ? (
          <SigninError cfg={cfg!} />
        ) : location.pathname === "/signed-out" && cfg!.oidc ? (
          <SignedOut cfg={cfg!} />
        ) : (
          <Login cfg={cfg!} onLogin={setMe} />
        )}
        <Toaster />
      </>
    )
  }
  const admin = me.role === "admin"
  const known = allItems.some((p) => (p.id === route.page || p.also?.includes(route.page)) && (!p.admin || admin))
  const page = known ? route.page : "overview"
  const section = allItems.find((p) => p.id === page || p.also?.includes(page))!

  return (
    <TooltipProvider>
      <SidebarProvider>
        <Sidebar collapsible="icon">
          <SidebarHeader>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton size="lg" tooltip="use-my-mac" asChild>
                  <a href={href("overview")}>
                    <span className="flex aspect-square size-8 items-center justify-center rounded-lg bg-sidebar-primary text-sidebar-primary-foreground">
                      <CpuIcon className="size-4" />
                    </span>
                    <span className="flex flex-col leading-tight">
                      <span className="font-semibold">use-my-mac</span>
                      <span className="text-xs text-muted-foreground">Mac compute pool</span>
                    </span>
                  </a>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarHeader>
          <SidebarContent>
            <NavMenu page={page} admin={admin} />
          </SidebarContent>
          <SidebarFooter>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton asChild tooltip="Documentation" size="lg" className="md:h-8">
                  <a href="/docs">
                    <BookOpenIcon />
                    <span>Documentation</span>
                  </a>
                </SidebarMenuButton>
              </SidebarMenuItem>
              <SidebarMenuItem>
                <SidebarMenuButton
                  tooltip="Sign out"
                  size="lg"
                  onClick={() => api.logout().finally(() => location.assign("/signed-out"))}
                >
                  <LogOutIcon />
                  <span className="flex min-w-0 flex-col leading-tight">
                    <span className="truncate">Sign out</span>
                    <span className="truncate text-xs text-muted-foreground">
                      {me.name} · {me.role}
                    </span>
                  </span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarFooter>
          <SidebarRail />
        </Sidebar>
        <SidebarInset className="min-w-0">
          <header className="sticky top-0 z-10 flex h-14 items-center gap-2 border-b bg-background/95 px-3 backdrop-blur supports-[backdrop-filter]:bg-background/80 md:h-12 md:px-4">
            <SidebarTrigger className="max-md:size-10" />
            <Separator orientation="vertical" className="h-4" />
            <Breadcrumb className="min-w-0">
              <BreadcrumbList>
                {route.id && known ? (
                  <>
                    <BreadcrumbItem>
                      <BreadcrumbLink href={href(section.id)}>{section.title}</BreadcrumbLink>
                    </BreadcrumbItem>
                    <BreadcrumbSeparator />
                    <BreadcrumbItem className="min-w-0">
                      <BreadcrumbPage className="truncate font-mono text-xs">{route.id}</BreadcrumbPage>
                    </BreadcrumbItem>
                  </>
                ) : (
                  <BreadcrumbItem>
                    <BreadcrumbPage>{section.title}</BreadcrumbPage>
                  </BreadcrumbItem>
                )}
              </BreadcrumbList>
            </Breadcrumb>
            <div className="ml-auto">
              <ThemeToggle />
            </div>
          </header>
          <main className="mx-auto w-full max-w-[1600px] p-3 pb-[max(1rem,env(safe-area-inset-bottom))] md:p-6">
            <Suspense fallback={<PageFallback />}>
              {page === "overview" && <OverviewPage admin={admin} />}
              {page === "macs" && <MacsPage admin={admin} />}
              {page === "runtimes" && <RuntimesPage admin={admin} />}
              {page === "mac" && route.id && <MacDetailPage key={route.id} id={route.id} admin={admin} />}
              {page === "jobs" && <JobsPage admin={admin} />}
              {page === "job" && route.id && <JobDetailPage key={route.id} id={route.id} admin={admin} />}
              {page === "activity" && <ActivityPage />}
              {page === "keys" && admin && <KeysPage />}
              {page === "settings" && <SettingsPage me={me} />}
            </Suspense>
          </main>
        </SidebarInset>
      </SidebarProvider>
      <Toaster />
    </TooltipProvider>
  )
}

export default App
