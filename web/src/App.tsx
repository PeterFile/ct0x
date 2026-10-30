import type { ReactNode } from "react";

function EmptyPanel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="empty-panel">
      <h3>{title}</h3>
      <p>{children}</p>
    </div>
  );
}

export function App() {
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
        <p className="preview-label">界面预览 · 尚未接入本地服务</p>
      </header>

      <div className="workspace">
        <aside className="column groups" aria-labelledby="groups-heading">
          <h2 className="column-heading" id="groups-heading">群组</h2>
          <EmptyPanel title="尚未载入群组">
            接入服务后，才能创建或选择群组。
          </EmptyPanel>
          <p className="column-note">一处共同的语境。</p>
        </aside>

        <main className="column discussion" id="discussion" tabIndex={-1}
          aria-labelledby="discussion-heading">
          <h2 className="column-heading" id="discussion-heading">讨论</h2>
          <div className="discussion-intro">
            <p className="eyebrow accent">让想法在此相遇</p>
            <p className="manifesto">先有讨论，<br />再有分工。</p>
            <EmptyPanel title="讨论区尚未启用">
              当前仅提供界面预览，未读取任何群组、成员或消息。
            </EmptyPanel>
          </div>
          <p className="discussion-note">消息发送与成员操作尚未开放。</p>
        </main>

        <aside className="column members" aria-labelledby="members-heading">
          <h2 className="column-heading" id="members-heading">成员</h2>
          <EmptyPanel title="尚未载入成员">
            成员信息将在接入服务并选择群组后显示。
          </EmptyPanel>
          <p className="column-note">不同视角，同席讨论。</p>
        </aside>
      </div>
    </div>
  );
}
