"use client";

import * as React from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { PanelLeftClose, PanelLeftOpen } from "lucide-react";
import { BrandMark } from "@/components/shell/brand-mark";
import { useAuth } from "@/components/shell/auth-provider";
import { Button } from "@/components/ui/button";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
  SidebarSeparator,
  useSidebar,
} from "@/components/ui/sidebar";
import { FOOTER_ITEMS, NAV_GROUPS, navItemFor } from "@/lib/nav";

// Row geometry for the rail: a taller row and a one-step-larger icon so the
// glyph anchors its label. Applied per row rather than forking the primitive.
const NAV_ROW = "h-9 px-2 py-1 [&>svg]:size-5";
// Section headings a full step below the rows, in the shared micro-caps.
const NAV_GROUP_LABEL = "eyebrow h-6 text-sidebar-foreground/60";

function NavToggle() {
  const { toggleSidebar, state } = useSidebar();
  const collapsed = state === "collapsed";
  const label = collapsed ? "Expand sidebar" : "Collapse sidebar";
  return (
    <Button
      variant="ghost"
      size="icon"
      className="size-8 text-muted-foreground group-data-[collapsible=icon]:mx-auto"
      onClick={toggleSidebar}
      title={label}
    >
      {collapsed ? <PanelLeftOpen /> : <PanelLeftClose />}
      <span className="sr-only">{label}</span>
    </Button>
  );
}

// Grouped nav on the shadcn sidebar primitive: brand + collapse toggle at the
// top, the sections from lib/nav, and settings pinned at the bottom. Who is
// signed in lives in the topbar avatar, so it is said once rather than twice.
export function AppSidebar() {
  const pathname = usePathname();
  const active = navItemFor(pathname);
  const { me } = useAuth();
  // Which organisation this session is acting as. Once one person can belong to two, "Admin
  // console" is the one thing on screen that never tells you which one you are looking at.
  const org = me?.user?.org_name;
  // An entry gated on a permission shows only to somebody who holds it. Until /api/me has
  // answered there is nothing to check against, and the rail shows everything rather than
  // flickering an entry in a moment later.
  const perms = me?.user?.permissions;
  const visible = (item: { permission?: string }) =>
    !item.permission || !perms || perms[item.permission] === true;

  const { isMobile, setOpenMobile } = useSidebar();
  React.useEffect(() => {
    if (isMobile) setOpenMobile(false);
  }, [pathname, isMobile, setOpenMobile]);

  return (
    <Sidebar collapsible="icon" className="h-dvh">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem className="flex items-center gap-1">
            <SidebarMenuButton
              size="lg"
              asChild
              className="flex-1 group-data-[collapsible=icon]:hidden"
            >
              <Link href="/">
                <BrandMark />
                <div className="grid flex-1 text-left leading-tight">
                  <span className="truncate text-sm font-semibold">attest_tag</span>
                  <span className="truncate text-xs text-muted-foreground">
                    {org || "Admin console"}
                  </span>
                </div>
              </Link>
            </SidebarMenuButton>
            <NavToggle />
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        {NAV_GROUPS.map((group) => (
          <SidebarGroup key={group.label} className="py-1">
            {group.items.length > 1 || group.label !== group.items[0]?.label ? (
              <SidebarGroupLabel className={NAV_GROUP_LABEL}>{group.label}</SidebarGroupLabel>
            ) : null}
            <SidebarMenu>
              {group.items.filter(visible).map((item) => (
                <SidebarMenuItem key={item.href}>
                  <SidebarMenuButton
                    asChild
                    isActive={item.href === active?.href}
                    tooltip={item.label}
                    className={NAV_ROW}
                  >
                    <Link href={item.href}>
                      <item.icon />
                      <span>{item.label}</span>
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroup>
        ))}
      </SidebarContent>

      <SidebarFooter>
        {/* A rule above each of these rather than only above the pair: they are pinned here for
            different reasons, and one rule would read as a single two-row section. */}
        {FOOTER_ITEMS.map((item) => (
          <React.Fragment key={item.href}>
            <SidebarSeparator />
            <SidebarMenu>
              <SidebarMenuItem>
                <SidebarMenuButton
                  asChild
                  isActive={item.href === active?.href}
                  tooltip={item.label}
                  className={NAV_ROW}
                >
                  <Link href={item.href}>
                    <item.icon />
                    <span>{item.label}</span>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            </SidebarMenu>
          </React.Fragment>
        ))}
      </SidebarFooter>

      <SidebarRail />
    </Sidebar>
  );
}
