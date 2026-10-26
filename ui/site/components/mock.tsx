import { Kbd, Meter, Wordmark } from './ui';

function I({ d }: { d: string }) {
  return <svg viewBox="0 0 16 16" aria-hidden="true"><path d={d} fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinejoin="round" strokeLinecap="round" /></svg>;
}
const chat = 'M2.5 4.2c0-.9.7-1.7 1.7-1.7h7.6c.9 0 1.7.8 1.7 1.7v5.1c0 .9-.8 1.7-1.7 1.7H6.3L3.6 13.2v-2.3c-.6-.2-1.1-.9-1.1-1.6z';
const book = 'M3 3.2c0-.6.5-1 1-1h8.5v10H4a1 1 0 0 0-1 1zm0 10c0 .5.5 1 1 1h8.5';
const users = 'M8.3 5.5a2.3 2.3 0 1 1-4.6 0 2.3 2.3 0 0 1 4.6 0zM1.8 13c.4-2.2 2.1-3.5 4.2-3.5s3.8 1.3 4.2 3.5M10.5 3.4a2.2 2.2 0 0 1 0 4.2M12 9.7c1.2.4 2 1.6 2.2 3.3';
const gear = 'M8 5.8a2.2 2.2 0 1 0 0 4.4 2.2 2.2 0 0 0 0-4.4zM8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4';

/* An illustrative view of the shipped client, not an interactive or live session. */
export function ClientWindow() {
  return (
    <div className="frame client-preview" aria-label="Illustrative Tutor ACP conversation: resume a course, review a concept and practise the next activity. Example learning data." role="img">
      <div className="win">
        <aside className="nav">
          <div className="brand"><Wordmark size={15} still /></div>
          <div className="ni on"><I d={chat} />Conversation</div>
          <div className="ni"><I d={book} />Courses</div>
          <div className="newc">+ New conversation</div>
          <div className="hh">Conversations</div>
          <div className="hi on">Pick up Go for beginners<small>Today</small></div>
          <div className="hi">Reviewing maps<small>Yesterday</small></div>
          <div className="hi">Designing a SQL course<small>Last week</small></div>
          <div className="foot"><div className="ni"><I d={users} />My account</div><div className="ni"><I d={gear} />Settings</div></div>
        </aside>
        <div className="mw">
          <div className="tbar"><b>Conversation</b><span className="s">Go for beginners</span><span className="ready"><i />Ready</span>
            <span className="wc" aria-hidden="true"><span><svg viewBox="0 0 10 10"><path d="M0 5h10" stroke="currentColor" /></svg></span><span><svg viewBox="0 0 10 10"><rect x=".5" y=".5" width="9" height="9" fill="none" stroke="currentColor" /></svg></span><span><svg viewBox="0 0 10 10"><path d="M0 0l10 10M10 0 0 10" stroke="currentColor" /></svg></span></span>
          </div>
          <div className="body"><div className="cv">
            <div className="msgs">
              <div><div className="lab">You</div><div className="say">Let’s continue my Go course. I have 15 minutes.</div></div>
              <div>
                <div className="lab">Tutor <span className="pill" style={{ height: 20, fontSize: 10.5 }}>Course resumed</span></div>
                <div className="say">Back to <b>Go for beginners</b>, where you left off: slices.
                  <ul>
                    <li><b>Review:</b> maps, last practised 6 days ago.</li>
                    <li><b>Next:</b> a short review, then an exercise on <code>append</code>.</li>
                    <li><b>Your pace:</b> we can stop after this activity and continue another day.</li>
                  </ul>
                </div>
                <div style={{ marginTop: 14 }}><Meter label="Slices · mastery estimate" value={0.41} /></div>
                <p className="mock-reading-note">An estimate is not assessment evidence.</p>
              </div>
            </div>
            <div className="cmp">
              <div className="ph">Let’s start with the review.<span className="cursor" /></div>
              <div className="ft"><small>Ctrl + Enter to send · AI can make mistakes.</small><span className="btn btn-blue">Send <Kbd>Ctrl ⏎</Kbd></span></div>
            </div>
          </div></div>
        </div>
      </div>
    </div>
  );
}

const exampleCourses = [
  { name: 'Go for beginners', description: 'From your first function to an HTTP service.', version: 'Version 2', tag: 'Available to members' },
  { name: 'Data literacy', description: 'Ask better questions of the data around you.', version: 'Version 1', tag: 'Available to members' },
  { name: 'Team onboarding', description: 'A shared foundation for a new role.', version: 'Version 3', tag: 'By invitation' },
];

export function InstitutionPreview() {
  return (
    <div className="institution-preview" role="img" aria-label="Illustrative institution catalogue with three example member-created courses. Courses have published versions and can be offered to all members or by invitation.">
      <div className="institution-preview-header"><div><span className="fig">Example institution</span><h3>Learning, together.</h3></div><span className="pill">Shared catalogue</span></div>
      <div className="institution-members-line"><I d={users} /><span>Programmes created by your community</span></div>
      <div className="institution-course-list">
        {exampleCourses.map((course) => <div className="institution-course" key={course.name}><div className="institution-course-head"><I d={book} /><b>{course.name}</b><span className="fig">{course.version}</span></div><p>{course.description}</p><div className="row"><span className="pill">{course.tag}</span><span className="small muted">Published</span></div></div>)}
      </div>
      <p className="mock-example-note">Illustrative catalogue. Members choose their courses; progress stays individual.</p>
    </div>
  );
}
