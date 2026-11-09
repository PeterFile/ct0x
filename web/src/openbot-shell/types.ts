import type { ComponentPropsWithRef, ReactNode, Ref } from "react";

export type ShellContext = "members" | "tasks" | "history";

/** App owns state, read activity, mutations, visibility, and focus recovery. */
export interface AppShellProps {
  slots: { skip: ReactNode; groups: ReactNode; chat: ReactNode; context: ReactNode };
  groupsProps: ComponentPropsWithRef<"aside">;
  pane: "list" | "chat";
  context: ShellContext | null;
  onList: () => void;
  onChat: () => void;
  onContext: (context: ShellContext) => void;
  contextTriggers: Record<ShellContext, Ref<HTMLButtonElement>>;
}
