import { useState } from "react";
import type { ReactNode } from "react";
import type { Group } from "./api";
import { Groups } from "./Groups";
import { Members } from "./Members";

function EmptyPanel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="empty-panel">
      <h3>{title}</h3>
      <p>{children}</p>
    </div>
  );
}

export function App() {
  const [selected, setSelected] = useState<Group | null>(null);
  return (
    <div className="app">
      <a className="skip-link" href="#discussion">跳至讨论区</a>
      <header className="masthead">
        <div className="identity">
          <span className="brand-mark" aria-hidden="true" />
          <div>
            <h1>ct0x</h1>
            <p className="eyebrow">协作工作台</p>
          </div>
        </div>
        <p className="preview-label">群组工作台 · 讨论功能尚未开放</p>
      </header>

      <div className="workspace">
        <aside className="column groups" aria-labelledby="groups-heading">
          <h2 className="column-heading" id="groups-heading">群组</h2>
          <Groups selectedId={selected?.id} onSelect={setSelected} />
          <p className="column-note">一处共同的语境。</p>
        </aside>

        <main className="column discussion" id="discussion" tabIndex={-1}
          aria-labelledby="discussion-heading">
          <h2 className="column-heading" id="discussion-heading">讨论</h2>
          <div className="discussion-intro">
            <p className="eyebrow accent">让想法在此相遇</p>
            {selected ? <EmptyPanel title={selected.name}>{selected.description || "此群组尚无说明。"}</EmptyPanel>
              : <p className="manifesto">先有讨论，<br />再有分工。</p>}
            <EmptyPanel title="讨论区尚未启用">
              消息尚未载入，发送功能尚未开放。
            </EmptyPanel>
          </div>
          <p className="discussion-note">消息发送功能尚未开放。</p>
        </main>

        <aside className="column members" aria-labelledby="members-heading">
          <h2 className="column-heading" id="members-heading">成员</h2>
          <Members groupId={selected?.id} />
          <p className="column-note">不同视角，同席讨论。</p>
        </aside>
      </div>
    </div>
  );
}
