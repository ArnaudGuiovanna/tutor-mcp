// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
// Real React UI, local IPC fixtures only. This does not qualify an agent or MCP.
// Start Vite on 1420 and Chrome with --remote-debugging-port=9224 separately.
// node client/scripts/qualify-study-layout.mjs [/tmp/tutor-study-layout]
import { mkdir, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';

const origin = process.env.TUTOR_STUDY_LAYOUT_ORIGIN || process.env.TUTOR_LAYOUT_ORIGIN || 'http://127.0.0.1:1420';
const cdp = process.env.TUTOR_STUDY_LAYOUT_CDP || process.env.TUTOR_LAYOUT_CDP || 'http://127.0.0.1:9224';
for (const value of [origin, cdp]) {
  const url = new URL(value);
  if (!['127.0.0.1', 'localhost'].includes(url.hostname) || !['http:', 'https:'].includes(url.protocol) || url.username || url.password) {
    throw new Error('Local preview and local CDP required.');
  }
}
const output = resolve(process.argv[2] || '/tmp/tutor-study-layout');
await mkdir(output, { recursive: true });

// Only this newly created target receives fixtures; no existing browser tab or
// native client data is touched. IPC calls are handled entirely in this page.
const target = await (await fetch(`${cdp}/json/new?about:blank`, { method: 'PUT' })).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = reject; });
let sequence = 0;
const pending = new Map();
const browserErrors = [];
const captures = [];
const assertions = [];
ws.onmessage = event => {
  const message = JSON.parse(event.data);
  if (message.id) {
    const call = pending.get(message.id);
    if (!call) return;
    pending.delete(message.id); clearTimeout(call.timer);
    if (message.error) call.reject(new Error(JSON.stringify(message.error))); else call.resolve(message.result);
  } else if (message.method === 'Runtime.exceptionThrown') {
    browserErrors.push(message.params.exceptionDetails.text + ' ' + JSON.stringify(message.params.exceptionDetails.exception));
  }
};
ws.onclose = () => {
  for (const call of pending.values()) { clearTimeout(call.timer); call.reject(new Error('CDP target closed.')); }
  pending.clear();
};
function send(method, params = {}) {
  return new Promise((resolve, reject) => {
    const id = ++sequence;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`CDP timeout: ${method}`)); }, 15_000);
    pending.set(id, { resolve, reject, timer });
    ws.send(JSON.stringify({ id, method, params }));
  });
}
async function evaluate(expression) {
  const response = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  if (response.exceptionDetails) throw new Error(JSON.stringify(response.exceptionDetails));
  return response.result.value;
}
async function until(expression) {
  for (let attempt = 0; attempt < 120; attempt++) {
    if (await evaluate(expression)) return;
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error(`Page not ready: ${expression}; browser errors: ${browserErrors.join(' | ')}`);
}
async function check(label, expression) {
  const passed = await evaluate(expression);
  assertions.push({ label, passed: !!passed });
  if (!passed) throw new Error(`Assertion failed: ${label}`);
}
async function press(key) {
  const keys = { Enter: ['Enter', 13], Tab: ['Tab', 9], Escape: ['Escape', 27], ArrowDown: ['ArrowDown', 40], ArrowRight: ['ArrowRight', 39], ' ': ['Space', 32] };
  const [code, virtualKey] = keys[key];
  const text = key === 'Enter' ? '\r' : key === ' ' ? ' ' : undefined;
  await send('Input.dispatchKeyEvent', { type: 'keyDown', key, code, windowsVirtualKeyCode: virtualKey, nativeVirtualKeyCode: virtualKey, ...(text ? { text, unmodifiedText: text } : {}) });
  await send('Input.dispatchKeyEvent', { type: 'keyUp', key, code, windowsVirtualKeyCode: virtualKey, nativeVirtualKeyCode: virtualKey });
}
async function click(expression) {
  const point = await evaluate(`(() => { const element = ${expression}; if (!element || element.disabled) throw new Error('Missing or disabled click target'); element.scrollIntoView({block:'nearest', inline:'nearest'}); const box = element.getBoundingClientRect(); if (!box.width || !box.height) throw new Error('Invisible click target'); return {x:box.left+box.width/2,y:box.top+box.height/2}; })()`);
  await send('Input.dispatchMouseEvent', { type: 'mouseMoved', ...point });
  await send('Input.dispatchMouseEvent', { type: 'mousePressed', ...point, button: 'left', clickCount: 1 });
  await send('Input.dispatchMouseEvent', { type: 'mouseReleased', ...point, button: 'left', clickCount: 1 });
}
const button = name => `[...document.querySelectorAll('button')].find(button => button.textContent.trim() === ${JSON.stringify(name)})`;
const navigation = page => `document.querySelector('.navigation button[aria-label=${JSON.stringify(page)}]')`;
const labelledButton = label => `document.querySelector(${JSON.stringify(`button[aria-label=${JSON.stringify(label)}]`)})`;
const quizQuestionVisible = `(() => {
  const question = document.querySelector('.learning-card.quiz .learning-question p');
  const viewport = document.querySelector('.conversation-messages');
  if (!question || !viewport) return false;
  const content = question.getBoundingClientRect(), frame = viewport.getBoundingClientRect();
  return content.height > 0 && content.top >= Math.max(0, frame.top) - 1 && content.bottom <= Math.min(innerHeight, frame.bottom) + 1;
})()`;

function fixture() {
  const scenario = new URL(location.href).searchParams.get('studyFixture') || 'quiz';
  const storageKey = `tutor-study-layout:${scenario}`;
  const today = new Date(); today.setHours(9, 0, 0, 0);
  const due = days => { const date = new Date(today); date.setDate(date.getDate() + days); return date.toISOString(); };
  const common = { kind: 'personal', revision: '2', status: 'active', audience: 'private', author_id: 'demo-user', can_edit: true, can_learn: true, can_share: true, can_publish: false, can_review: false, free_publication: false };
  const trainings = [
    { ...common, id: 'personal:go', name: 'Go pour construire mes premiers outils', description: 'Comprendre les variables et les fonctions.', domain_id: 'go', progress: [{ concept: 'Variables', mastery: 0.42, last_review: due(-5), next_review: due(-1) }, { concept: 'Fonctions', mastery: 0.51, last_review: due(-3), next_review: due(2) }] },
    { ...common, id: 'personal:english', name: 'Anglais au quotidien', description: 'Raconter les événements de la veille.', domain_id: 'english', progress: [{ concept: 'Parler du passé', mastery: 0.62, last_review: due(-4), next_review: due(0) }] },
    { ...common, id: 'personal:python', name: 'Python et mes tâches quotidiennes', description: 'Automatiser de petites tâches.', domain_id: 'python', progress: [] },
  ];
  const overview = { identity: { tenantId: 'demo-tenant', userId: 'demo-user', membershipId: 'demo-member', learnerId: 'demo-learner', roles: ['learner'] }, context: {}, formations: { items: [] }, availableFormations: { items: [] }, capabilities: { learning: true, canLearn: true, canEnableLearning: false } };
  const context = (training, id, selection = 1) => ({ conversation_id: id, training_id: training.id, domain_id: training.domain_id, revision: training.revision, selection_version: selection, name: training.name });
  const quiz = {
    version: 1, id: 'quiz-variables', kind: 'quiz', trainingId: trainings[0].id, domainId: 'go', concept: 'Variables', sessionId: 'session-go', decisionId: 'decision-go',
    question: 'Quelle valeur contient `x` après ces deux instructions ?\n\n```go\nx := 3\nx += 2\n```',
    attemptId: 'attempt-variables', activityType: 'RECALL_EXERCISE',
    rubricJson: JSON.stringify({ criteria: [{ id: 'result', description: 'Identifier la valeur après addition.', max_score: 1, anchors: [{ score: 0, description: 'Réponse incorrecte.' }, { score: 1, description: 'La valeur 5 est choisie.' }] }], passing_score: 1, answer_key: 'b: 5' }),
    options: [{ id: 'a', text: '3' }, { id: 'b', text: '5' }, { id: 'c', text: '6' }], correctOptionId: 'b',
    explanation: 'La variable contient d’abord 3. L’instruction `x += 2` ajoute 2 à sa valeur : elle contient maintenant **5**.',
  };
  const flashcard = { version: 1, id: 'flashcard-yesterday', kind: 'flashcard', trainingId: trainings[1].id, domainId: 'english', concept: 'Parler du passé', sessionId: 'session-english', decisionId: 'decision-english', question: 'Comment dit-on « hier » en anglais ?', answer: '**Yesterday**\n\n“I finished this book yesterday.”' };
  const activityText = activity => '```tutor-activity\n' + JSON.stringify(activity) + '\n```';
  const conversation = (id, training, activity) => ({
    id, title: training.name, targetId: 'bundled:codex', tutorBinding: 'study-fixture', createdAt: due(-7), updatedAt: due(0), activeTraining: context(training, id),
    agentSessionId: `agent-${id}`, messages: activity ? [{ id: `message-${activity.id}`, role: 'assistant', text: activityText(activity), createdAt: due(0), status: 'complete', trainingContext: context(training, id) }] : [],
  });
  let state = {
    settings: { language: 'fr', targetId: 'bundled:codex', cwd: '/demo', tutor: { mode: 'remote', activity: 'auto', url: 'https://institution.example', command: '', args: [] }, appearance: { theme: 'dark', fontSize: 20, lineHeight: 1.65, reduceMotion: true }, reviewPlans: {} },
    identity: { exists: true, npub: 'npub1demonstration', backupConfirmed: true, needsBackup: false }, profile: { id: 'demo-profile', label: 'Profil de démonstration' }, profiles: [], profileEpoch: 1,
    tutorBinding: 'study-fixture', intents: [], pendingMessage: null,
    conversations: scenario.startsWith('empty') ? [] : [conversation('conversation-go', trainings[0], scenario.startsWith('planning') ? undefined : quiz), conversation('conversation-english', trainings[1], flashcard)],
    activeConversationId: scenario.startsWith('empty') ? undefined : scenario.startsWith('flashcard') || scenario.startsWith('planning') ? 'conversation-english' : 'conversation-go',
    tutorStatus: { connected: true, label: 'Institution de démonstration' }, agentConnected: true,
  };
  const scope = JSON.stringify([state.profile.id, state.identity.npub, 'remote', 'https://institution.example', overview.identity.tenantId, overview.identity.userId]);
  state.settings.reviewPlans[JSON.stringify([scope, trainings[0].id])] = { enabled: true, format: 'quiz' };
  state.settings.reviewPlans[JSON.stringify([scope, trainings[1].id])] = { enabled: true, format: 'flashcard' };
  let calls = [];
  const previous = sessionStorage.getItem(storageKey);
  if (previous) { const restored = JSON.parse(previous); state = restored.state; calls = restored.calls; }
  const persist = () => sessionStorage.setItem(storageKey, JSON.stringify({ state, calls }));
  const snapshot = () => structuredClone(state);
  const callbacks = new Map(), listeners = new Map(); let callbackId = 0, listenerId = 0;
  const emit = (event, payload) => { for (const [id, listener] of listeners) if (listener.event === event) callbacks.get(listener.handler)?.({ event, id, payload: structuredClone(payload) }); };
  const find = id => state.conversations.find(item => item.id === id);
  const payload = (text, tag) => {
    const match = text.match(new RegExp('^```' + tag + '\\s*\\n([\\s\\S]*?)\\n```[ \\t]*$', 'm'));
    return match ? JSON.parse(match[1]) : undefined;
  };
  window.__studyFixture = {
    get state() { return state; }, get calls() { return calls; }, quiz, flashcard, overview,
    theme(value) { state.settings.appearance.theme = value; persist(); emit('client:state', state); },
  };
  window.isTauri = true;
  window.__TAURI_EVENT_PLUGIN_INTERNALS__ = { unregisterListener(_event, id) { listeners.delete(id); } };
  window.__TAURI_INTERNALS__ = {
    metadata: { currentWindow: { label: 'main' }, currentWebview: { label: 'main' } },
    transformCallback(callback) { const id = ++callbackId; callbacks.set(id, callback); return id; },
    unregisterCallback(id) { callbacks.delete(id); },
    invoke: async (command, args = {}) => {
      if (command === 'plugin:event|listen') { const id = ++listenerId; listeners.set(id, { event: args.event, handler: args.handler }); return id; }
      if (command === 'plugin:event|unlisten') { listeners.delete(args.eventId); return; }
      if (command === 'plugin:window|is_maximized') return false;
      if (command === 'plugin:window|is_focused') return true;
      calls.push({ command, args: structuredClone(args) });
      if (command === 'load_state') return snapshot();
      if (command === 'bundled_agents') return [];
      if (command === 'tutor_overview') return structuredClone(overview);
      if (command === 'save_settings') { state.settings = structuredClone(args.settings); persist(); return snapshot(); }
      if (command === 'select_conversation') { state.activeConversationId = args.id; persist(); return snapshot(); }
      if (command === 'queue_prompt') { state.pendingMessage = args.text ? { text: args.text, conversationId: args.conversationId } : null; persist(); return snapshot(); }
      if (command === 'start_conversation') {
        let current = find(args.resumeId);
        if (!current) { current = conversation(`conversation-new-${state.conversations.length}`, trainings[0]); state.conversations.push(current); }
        state.activeConversationId = current.id; persist(); return structuredClone(current);
      }
      if (command === 'training_action') {
        if (args.action === 'list_training_library') return { items: structuredClone(trainings), next_after: '' };
        const current = find(args.input.conversation_id);
        if (args.action === 'get_conversation_context') return structuredClone(current?.activeTraining || { conversation_id: args.input.conversation_id, training_id: '', domain_id: '', revision: '', selection_version: 0, name: '' });
        if (args.action === 'select_conversation_training') {
          const training = trainings.find(item => item.id === args.input.id);
          if (!training || !current || args.input.expected_selection !== (current.activeTraining?.selection_version || 0)) throw new Error('Fixture selection conflict.');
          current.activeTraining = context(training, current.id, (current.activeTraining?.selection_version || 0) + 1); persist(); return structuredClone(current.activeTraining);
        }
        throw new Error(`Unsupported study training fixture: ${args.action}`);
      }
      if (command === 'send_prompt') {
        const current = find(args.conversationId);
        if (!current) throw new Error('Missing study fixture conversation.');
        current.messages.push({ id: `user-${current.messages.length}`, role: 'user', text: args.text, status: 'complete', createdAt: new Date().toISOString(), trainingContext: structuredClone(current.activeTraining) });
        state.pendingMessage = null;
        const request = payload(args.text, 'tutor-study-request');
        if (request) {
          const training = trainings.find(item => item.id === request.trainingId) || trainings.find(item => item.id === current.activeTraining.training_id);
          const template = request.format === 'flashcard' ? flashcard : quiz;
          const activity = { ...template, id: `${template.id}-${current.messages.length}`, trainingId: training.id, domainId: training.domain_id };
          if (activity.attemptId) activity.attemptId += `-${current.messages.length}`;
          current.messages.push({ id: `assistant-${current.messages.length}`, role: 'assistant', text: activityText(activity), status: 'complete', createdAt: new Date().toISOString(), trainingContext: structuredClone(current.activeTraining) });
        }
        current.updatedAt = new Date().toISOString(); persist(); return { stopReason: 'end_turn' };
      }
      throw new Error(`Unsupported study IPC fixture: ${command}`);
    },
  };
}

async function scene(name, width = 1440, height = 960) {
  await send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
  const url = new URL(origin); url.searchParams.set('studyFixture', name);
  await send('Page.navigate', { url: url.href });
  await until('!!document.querySelector("#prompt") && !!window.__studyFixture');
  await evaluate('document.fonts.ready');
}
async function capture(name, width = 1440, height = 960) {
  await send('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
  await new Promise(resolve => setTimeout(resolve, 200));
  const dimensions = await evaluate(`({ width:innerWidth, document:document.documentElement.scrollWidth, theme:document.documentElement.dataset.theme, overflows:[...document.querySelectorAll('main,pre,.message-text,.message-table,.composer,.sidebar,.page-content,.learning-card,.learning-quiz-options,.learning-quiz-option,.learning-flashcard-back,.study-command-menu,.review-plan,.review-plan-controls,.review-plan-dates')].filter(element=>element.clientWidth>0 && element.scrollWidth>element.clientWidth+1).map(element=>({class:element.className,tag:element.tagName,width:element.clientWidth,scroll:element.scrollWidth})) })`);
  const screenshot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
  await writeFile(join(output, `${name}.png`), Buffer.from(screenshot.data, 'base64'));
  captures.push({ name, ...dimensions });
  if (dimensions.document > width + 1 || dimensions.overflows.length) throw new Error(`Horizontal overflow in ${name}: ${JSON.stringify(dimensions)}`);
}

try {
  await send('Page.enable'); await send('Runtime.enable');
  await send('Page.addScriptToEvaluateOnNewDocument', { source: `(${fixture.toString()})();` });

  await scene('planning');
  await click(navigation('Planning'));
  await until('!!document.querySelector(".review-plan.enabled")');
  await check('planning contains two active formation preferences', 'document.querySelectorAll(".review-toggle[aria-checked=true]").length === 2');
  await capture('planning-dark-desktop');
  await evaluate('window.__studyFixture.theme("light")');
  await until('document.documentElement.dataset.theme === "light"');
  await capture('planning-light-desktop');
  await capture('planning-light-360', 360, 800);
  await capture('planning-light-zoom-400', 320, 800);
  await evaluate('window.__studyFixture.theme("dark")');
  await capture('planning-dark-360', 360, 800);
  await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 960, deviceScaleFactor: 1, mobile: false });
  await evaluate('document.querySelector(".review-toggle[aria-label*=Python]").focus()');
  await press(' ');
  await until('document.querySelector(".review-toggle[aria-label*=Python]").getAttribute("aria-checked") === "true"');
  await check('planning switch works with keyboard and preserves focus', 'document.activeElement?.getAttribute("role") === "switch" && document.activeElement?.getAttribute("aria-label")?.includes("Python")');
  await click(labelledButton('Réviser Go pour construire mes premiers outils'));
  await until('!!document.querySelector(".learning-card.quiz")');
  await check('one planning click opens chat and submits one scoped review', `window.__studyFixture.calls.filter(call=>call.command === "send_prompt").length === 1 && window.__studyFixture.calls.find(call=>call.command === "send_prompt").args.text.includes(${JSON.stringify('"intent":"review"')}) && window.__studyFixture.state.activeConversationId === "conversation-go"`);
  await capture('planning-to-chat-dark');

  await scene('quiz');
  await until('!!document.querySelector(".learning-card.quiz")');
  await check('quiz conceals explanation and grading metadata before choice', '!document.querySelector(".learning-feedback") && !document.body.innerText.includes("La variable contient d’abord") && !document.body.innerText.includes("rubricJson") && !document.body.innerText.includes("attempt-variables") && !document.querySelector(".learning-quiz-option.correct")');
  await capture('quiz-before-dark');
  await evaluate('window.__studyFixture.theme("light")');
  await until('document.documentElement.dataset.theme === "light"');
  await capture('quiz-before-light');
  await evaluate('window.__studyFixture.theme("dark"); document.querySelector(".learning-quiz-options input").focus()');
  await press('ArrowRight');
  await until('document.querySelector(".learning-quiz-options input[value=b]").checked');
  await check('quiz answer chosen by keyboard remains ungraded until validation', '!document.querySelector(".learning-feedback") && window.__studyFixture.calls.every(call=>call.command !== "send_prompt")');
  await press('Tab');
  await check('quiz validation is next keyboard action', 'document.activeElement?.textContent.trim() === "Valider ma réponse"');
  await press('Enter');
  await until('!!document.querySelector(".learning-feedback.correct") && !document.querySelector(".learning-card[aria-busy=true]")');
  await check('quiz validation persists exactly one learner answer', 'window.__studyFixture.calls.filter(call=>call.command === "send_prompt").length === 1 && window.__studyFixture.state.conversations.find(item=>item.id === "conversation-go").messages.filter(item=>item.role === "user").length === 1');
  await capture('quiz-after-dark');
  await send('Page.reload');
  await until('!!document.querySelector(".learning-feedback.correct")');
  await check('reload restores chosen quiz response without replay', 'window.__studyFixture.calls.filter(call=>call.command === "send_prompt").length === 1 && ![...document.querySelectorAll(".learning-card button")].some(button=>button.textContent.trim() === "Valider ma réponse")');
  await capture('quiz-after-reload');

  await scene('flashcard');
  await until('!!document.querySelector(".learning-card.flashcard")');
  await check('flashcard hides answer and ratings before reveal', '!document.querySelector(".learning-flashcard-back") && !document.querySelector(".learning-recall") && !document.body.innerText.includes("Yesterday")');
  await capture('flashcard-before-dark');
  await evaluate(`${button('Retourner la carte')}.focus()`);
  await press('Enter');
  await until('!!document.querySelector(".learning-flashcard-back")');
  await check('flashcard keyboard reveal exposes three ratings and focuses answer', 'document.querySelectorAll(".learning-recall-buttons button").length === 3 && document.activeElement?.classList.contains("learning-flashcard-back") && document.body.innerText.includes("Yesterday")');
  await capture('flashcard-after-reveal-dark');
  await evaluate('window.__studyFixture.theme("light")');
  await until('document.documentElement.dataset.theme === "light"');
  await capture('flashcard-after-reveal-light');
  await click(button('Retrouvé avec effort'));
  await until('!!document.querySelector(".learning-feedback.self-report")');
  await check('flashcard self-report does not masquerade as a verified quiz answer', 'document.body.innerText.includes("Votre déclaration reste distincte") && !document.querySelector(".learning-feedback.correct") && window.__studyFixture.calls.filter(call=>call.command === "send_prompt").length === 1');
  await capture('flashcard-after-rating-light');

  await scene('quiz-compact', 360, 800);
  await until('!!document.querySelector(".learning-card.quiz")');
  await until(quizQuestionVisible);
  await check('quiz question is initially visible at 360 px', quizQuestionVisible);
  await capture('quiz-before-360', 360, 800);
  await capture('quiz-before-zoom-400', 320, 800);
  await check('quiz question stays visible at 320 px equivalent 400 percent zoom', quizQuestionVisible);
  await scene('flashcard-compact', 360, 800);
  await until('!!document.querySelector(".learning-card.flashcard")');
  await capture('flashcard-before-360', 360, 800);
  await click(button('Retourner la carte'));
  await until('!!document.querySelector(".learning-flashcard-back")');
  await capture('flashcard-after-360', 360, 800);
  await capture('flashcard-after-zoom-400', 320, 800);

  await scene('empty-menu');
  await click('document.querySelector("#prompt")');
  await send('Input.insertText', { text: '/' });
  await until('!!document.querySelector(".study-command-menu")');
  await capture('slash-menu-dark');
  await capture('slash-menu-360', 360, 800);
  await capture('slash-menu-zoom-400', 320, 800);
  await press('ArrowDown');
  await check('slash menu receives keyboard focus', 'document.activeElement?.getAttribute("role") === "menuitem" && document.activeElement.textContent.includes("Quiz")');
  await press('ArrowDown'); await press('Enter');
  await check('slash menu chooses flashcards without sending or losing input focus', 'document.querySelector("#prompt").value === "/flashcards " && document.activeElement?.id === "prompt" && !document.querySelector(".study-command-menu") && window.__studyFixture.calls.every(call=>call.command !== "send_prompt")');
  if (browserErrors.length) throw new Error(browserErrors.join('\n'));
  await writeFile(join(output, 'study-layout-report.json'), JSON.stringify({ fixtureOnly: true, noAgentOrServerQualification: true, captures, assertions, browserErrors }, null, 2) + '\n');
  await Promise.all([rm(join(output, 'failure.png'), { force: true }), rm(join(output, 'study-layout-failure.json'), { force: true })]);
  process.stdout.write(JSON.stringify({ output, captures: captures.length, assertions: assertions.length, overflow: false, fixtureOnly: true }) + '\n');
} catch (error) {
  try {
    const screenshot = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
    await writeFile(join(output, 'failure.png'), Buffer.from(screenshot.data, 'base64'));
    const body = await evaluate('document.body?.innerText');
    await writeFile(join(output, 'study-layout-failure.json'), JSON.stringify({ fixtureOnly: true, error: String(error), browserErrors, captures, assertions, body }, null, 2) + '\n');
  } catch { /* Preserve the original failure if the target has already closed. */ }
  throw error;
} finally {
  try { await send('Page.close'); } catch { /* Target may already have closed. */ }
  ws.close();
}
