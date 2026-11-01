import { useId, useState } from "react";
import type { FormEventHandler, ReactNode } from "react";

export function CreationDisclosure({ label, pending, error, onSubmit, children }: {
  label: string; pending: boolean; error: string; onSubmit: FormEventHandler<HTMLFormElement>; children: ReactNode;
}) {
  const id = useId(), [open, setOpen] = useState(false);
  return <div className="creation-disclosure">
    <button type="button" className="creation-toggle" aria-expanded={open} aria-controls={id} onClick={() => setOpen(previous => !previous)}>
      {open ? "收起" : "展开"}{label}
    </button>
    {!open && (pending || error) ? <p role={error ? "alert" : "status"} aria-label={`${label}状态`}>
      {pending ? "正在创建，请勿重复提交。" : error}
    </p> : null}
    <div id={id} hidden={!open}>
      <form aria-label={label} onSubmit={onSubmit}>{children}</form>
    </div>
  </div>;
}
