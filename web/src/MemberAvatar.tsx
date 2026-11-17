import { useEffect, useRef, useState } from "react";
import type { Member } from "./api";
import "./member-avatar.css";

export function MemberAvatar({ member, onRecipient }: { member: Member; onRecipient: () => void }) {
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null), action = useRef<HTMLButtonElement>(null);
  const eligible = member.status === "active";
  useEffect(() => { if (open) action.current?.focus(); }, [open]);
  return <div className="member-avatar-menu" onBlur={event => {
    if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false);
  }}>
    <button type="button" className="member-avatar" ref={trigger} disabled={!eligible}
      aria-label={`成员头像 ${member.name} · ${member.role} · ${member.id}`} title={`${member.name} · ${member.role} · ${member.id}`}
      aria-haspopup="menu" aria-expanded={open && eligible}
      onClick={() => setOpen(value => !value)}
      onContextMenu={event => { event.preventDefault(); if (eligible) setOpen(true); }}
      onKeyDown={event => {
        if (event.key === "ContextMenu" || (event.shiftKey && event.key === "F10") || event.key === "ArrowDown") {
          event.preventDefault(); if (eligible) setOpen(true);
        }
      }}>{[...member.name][0] || "·"}</button>
    {open && eligible ? <div role="menu" aria-label={`${member.name} · ${member.id}`} onKeyDown={event => {
      if (event.key === "Escape") { event.preventDefault(); setOpen(false); trigger.current?.focus(); }
    }}>
      <p className="member-avatar-identity" role="presentation">{member.name} · {member.role}<br />{member.id}</p>
      <button type="button" role="menuitem" ref={action} onClick={() => { setOpen(false); onRecipient(); }}>设为消息接收者</button>
    </div> : null}
  </div>;
}
