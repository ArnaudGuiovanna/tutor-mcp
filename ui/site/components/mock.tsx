import { Kbd, Meter, Wordmark } from './ui';

function I({ d }: { d: string }) {
  return <svg viewBox="0 0 16 16" aria-hidden="true"><path d={d} fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinejoin="round" strokeLinecap="round" /></svg>;
}
const chat = 'M2.5 4.2c0-.9.7-1.7 1.7-1.7h7.6c.9 0 1.7.8 1.7 1.7v5.1c0 .9-.8 1.7-1.7 1.7H6.3L3.6 13.2v-2.3c-.6-.2-1.1-.9-1.1-1.6z';
const book = 'M3 3.2c0-.6.5-1 1-1h8.5v10H4a1 1 0 0 0-1 1zm0 10c0 .5.5 1 1 1h8.5';
const users = 'M8.3 5.5a2.3 2.3 0 1 1-4.6 0 2.3 2.3 0 0 1 4.6 0zM1.8 13c.4-2.2 2.1-3.5 4.2-3.5s3.8 1.3 4.2 3.5M10.5 3.4a2.2 2.2 0 0 1 0 4.2M12 9.7c1.2.4 2 1.6 2.2 3.3';
const gear = 'M8 5.8a2.2 2.2 0 1 0 0 4.4 2.2 2.2 0 0 0 0-4.4zM8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4';

/* The ACP client as it ships: Conversation page, Courses, account and settings. Labels match the client. */
export function ClientWindow() {
  return (
    <div className="frame" aria-label="Tutor ACP, conversation page" role="img">
      <div className="win">
        <aside className="nav">
          <div className="brand"><Wordmark size={15} still /></div>
          <div className="ni on"><I d={chat} />Conversation</div>
          <div className="ni"><I d={book} />Formations</div>
          <div className="newc">+ New conversation</div>
          <div className="hh">Conversations</div>
          <div className="hi on">Pick up Go for beginners<small>today</small></div>
          <div className="hi">Reviewing maps<small>yesterday</small></div>
          <div className="hi">SQL for analytics plan<small>Sep 12</small></div>
          <div className="foot"><div className="ni"><I d={users} />My account</div><div className="ni"><I d={gear} />Settings</div></div>
        </aside>
        <div className="mw">
          <div className="tbar"><b>Conversation</b><span className="s">At your own pace</span><span className="ready"><i />Ready</span>
            <span className="wc" aria-hidden="true"><span><svg viewBox="0 0 10 10"><path d="M0 5h10" stroke="currentColor" /></svg></span><span><svg viewBox="0 0 10 10"><rect x=".5" y=".5" width="9" height="9" fill="none" stroke="currentColor" /></svg></span><span><svg viewBox="0 0 10 10"><path d="M0 0l10 10M10 0 0 10" stroke="currentColor" /></svg></span></span>
          </div>
          <div className="body"><div className="cv">
            <div className="msgs">
              <div><div className="lab">You</div><div className="say">Pick up my Go course.</div></div>
              <div>
                <div className="lab">Tutor <span className="pill mono" style={{ height: 18, fontSize: 10.5 }}>get_next_activity ✓</span></div>
                <div className="say">Back to <b>Go for beginners</b>, where you left off: slices.
                  <ul>
                    <li><b>Current estimate:</b> 0.41. The transfer probe unlocks at 0.85.</li>
                    <li><b>Review due:</b> maps, last seen 6 days ago.</li>
                    <li><b>Next:</b> a 10-minute review, then an exercise on <code>append</code>.</li>
                  </ul>
                </div>
                <div style={{ marginTop: 14 }}><Meter label="Slices · P(mastery)" value={0.41} /></div>
              </div>
            </div>
            <div className="cmp">
              <div className="ph">I want to learn Go…<span className="cursor" /></div>
              <div className="ft"><small>Ctrl + Enter to send · AI can make mistakes.</small><span className="btn btn-blue">Send <Kbd>Ctrl ⏎</Kbd></span></div>
            </div>
          </div></div>
        </div>
      </div>
    </div>
  );
}
