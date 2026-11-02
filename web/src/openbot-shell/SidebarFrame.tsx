// Adapted from nightly-labs/openbot ac99196, packages/ui/src/features/sidebar/SidebarFrame.tsx:1-16.
// PolyForm Noncommercial 1.0.0; see /licenses/openbot/index.html and docs/openbot-shell.md.
import type { ComponentPropsWithRef } from "react";

/** The upstream navigation surface, with the existing App landmark, ref, and Chinese heading. */
export function SidebarFrame({ children, ...props }: ComponentPropsWithRef<"aside">) {
  return <aside {...props} className="sidebar panel-edge im-conversations">
    {children}
  </aside>;
}
