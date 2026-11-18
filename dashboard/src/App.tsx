import { useEffect, useState } from "react"
import { CpuIcon, KeyRoundIcon, ListChecksIcon, LogOutIcon, MoonIcon, SettingsIcon, SunIcon } from "lucide-react"

import { useTheme } from "@/components/theme-provider"
import { Button } from "@/components/ui/button"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar"
import { Separator } from "@/components/ui/separator"
import { Spinner } from "@/components/ui/spinner"
import { Toaster } from "@/components/ui/sonner"
import { TooltipProvider } from "@/components/ui/tooltip"
import { api, type AuthConfig, type Me } from "@/lib/api"
import { JobsPage } from "@/pages/jobs"
import { KeysPage } from "@/pages/keys"
import { Login, SignedOut, SigninError } from "@/pages/login"
import { MacsPage } from "@/pages/macs"
import { SettingsPage } from "@/pages/settings"

type Page = "macs" | "jobs" | "keys" | "settings"

const pages: { id: Page; title: string; icon: React.ComponentType; admin?: boolean }[] = [
  { id: "macs", title: "Macs", icon: CpuIcon },
  { id: "jobs", title: "Jobs", icon: ListChecksIcon },
  { id: "keys", title: "API keys", icon: KeyRoundIcon, admin: true },
  { id: "settings", title: "Settings", icon: SettingsIcon },
]

function ThemeToggle() {
  const { theme, setTheme } = useTheme()
  const dark = theme === "dark"
  return (
    <Button variant="ghost" size="icon-sm" aria-label="Toggle theme" onClick={() => setTheme(dark ? "light" : "dark")}>
      {dark ? <SunIcon /> : <MoonIcon />}
    </Button>
  )
}

export function App() {
  const [me, setMe] = useState<Me | null | undefined>(undefined)
  const [cfg, setCfg] = useState<AuthConfig>()
  const [page, setPage] = useState<Page>(() => {
    const h = location.hash.slice(1) as Page
    return pages.some((p) => p.id === h) ? h : "macs"
  })

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

  useEffect(() => {
    location.hash = page
  }, [page])

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
  const current = pages.find((p) => p.id === page)!

  return (
    <TooltipProvider>
      <SidebarProvider>
        <Sidebar collapsible="icon">
          <SidebarHeader>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton size="lg" tooltip="use-my-mac">
                  <CpuIcon />
                  <div className="flex flex-col leading-tight">
                    <span className="font-semibold">use-my-mac</span>
                    <span className="text-xs text-muted-foreground">Mac compute pool</span>
                  </div>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarHeader>
          <SidebarContent>
            <SidebarGroup>
              <SidebarGroupContent>
                <SidebarMenu>
                  {pages
                    .filter((p) => !p.admin || admin)
                    .map((p) => (
                      <SidebarMenuItem key={p.id}>
                        <SidebarMenuButton isActive={p.id === page} tooltip={p.title} onClick={() => setPage(p.id)}>
                          <p.icon />
                          <span>{p.title}</span>
                        </SidebarMenuButton>
                      </SidebarMenuItem>
                    ))}
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          </SidebarContent>
          <SidebarFooter>
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton
                  tooltip="Sign out"
                  onClick={() => api.logout().finally(() => location.assign("/signed-out"))}
                >
                  <LogOutIcon />
                  <span className="truncate">Sign out {me.name}</span>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </SidebarFooter>
        </Sidebar>
        <SidebarInset>
          <header className="flex h-12 items-center gap-2 border-b px-4">
            <SidebarTrigger />
            <Separator orientation="vertical" className="h-4" />
            <h1 className="text-sm font-medium">{current.title}</h1>
            <div className="ml-auto">
              <ThemeToggle />
            </div>
          </header>
          <main className="p-4 md:p-6">
            {page === "macs" && <MacsPage admin={admin} />}
            {page === "jobs" && <JobsPage canCancel={admin} />}
            {page === "keys" && admin && <KeysPage />}
            {page === "settings" && <SettingsPage me={me} />}
          </main>
        </SidebarInset>
      </SidebarProvider>
      <Toaster />
    </TooltipProvider>
  )
}

export default App
