// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
// Runs only against a local preview, with IPC fixtures injected by CDP. No
// fixture is compiled into the client or used for pedagogical qualification.
import { mkdir, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
const origin = process.env.TUTOR_LAYOUT_ORIGIN || 'http://127.0.0.1:1420';
const cdp = process.env.TUTOR_LAYOUT_CDP || 'http://127.0.0.1:9224';
if (!['127.0.0.1','localhost'].includes(new URL(origin).hostname) || !['127.0.0.1','localhost'].includes(new URL(cdp).hostname)) throw new Error('Local preview required');
const output = resolve(process.argv[2] || '/tmp/tutor-layout');
await mkdir(output, { recursive: true });
const target = await (await fetch(`${cdp}/json/new?about:blank`, { method:'PUT' })).json();
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise((resolve,reject) => { ws.onopen=resolve; ws.onerror=reject; });
let next=0;const pending=new Map();const errors=[];
ws.onmessage = event => { const msg=JSON.parse(event.data); if(msg.id) { const call=pending.get(msg.id);pending.delete(msg.id);if(msg.error)call.reject(new Error(JSON.stringify(msg.error)));else call.resolve(msg.result); } else if(msg.method==='Runtime.exceptionThrown') errors.push(msg.params.exceptionDetails.text+' '+JSON.stringify(msg.params.exceptionDetails.exception)); };
const send=(method,params={})=>new Promise((resolve,reject)=>{const id=++next;pending.set(id,{resolve,reject});ws.send(JSON.stringify({id,method,params}));});
const evaluate=async expression=>{const response=await send('Runtime.evaluate',{expression,returnByValue:true,awaitPromise:true});if(response.exceptionDetails)throw new Error(JSON.stringify(response.exceptionDetails));return response.result.value;};
const fixture = () => {
 const training={id:'personal:go',kind:'personal',name:'Go pour construire mes premiers outils',description:'Comprendre le langage et écrire de petits programmes utiles.',domain_id:'go',revision:'2',status:'active',audience:'private',author_id:'demo',can_edit:true,can_learn:true,can_share:true,can_publish:false,can_review:false,free_publication:false,progress:[{concept:'variables',mastery:.42,last_review:'2026-10-25T10:00:00Z'}]};
 const doc={training,modules:[],concepts:[],curriculum:{domain_id:'go',version:2,graph:{concepts:['variables'],prerequisites:{}},concepts:[{id:'c1',key:'variables',label:'Variables et types',description:'Nommer une valeur et comprendre le type de données qu’elle représente.',level:'foundation',status:'active',outcomes:[{id:'o1',statement:'Déclarer une variable et utiliser sa valeur dans un programme.',observable:'Un petit programme Go qui affiche un résultat.'}],criteria:[{id:'cr1',description:'Le programme compile et utilise un type cohérent.',evidence:'Code exécuté et résultat expliqué.'}]}]}};
 const active={conversation_id:'conversation-go',training_id:training.id,domain_id:'go',revision:'2',selection_version:1,name:training.name};
 const state={settings:{targetId:'bundled:codex',cwd:'/demo',tutor:{mode:'remote',activity:'auto',url:'https://institution.example',command:'',args:[]}},identity:{exists:true,npub:'npub1example',backupConfirmed:true,needsBackup:false},profile:{id:'demo',label:'Mon compte'},profiles:[],profileEpoch:1,intents:[],conversations:[{id:'conversation-go',title:'Mes premiers pas en Go',targetId:'bundled:codex',tutorBinding:'demo',createdAt:'2026-10-25T10:00:00Z',updatedAt:'2026-10-25T10:15:00Z',activeTraining:active,messages:[{id:'m1',role:'user',text:'Je veux apprendre le Go pour créer de petits outils.',status:'complete',createdAt:'2026-10-25T10:00:00Z',trainingContext:active},{id:'m2',role:'assistant',text:'Votre formation est enregistrée. Commençons avec un premier programme.\n\n## Donner un nom à une valeur\n\nUne **variable** associe un nom à une valeur. En Go, `:=` permet de déclarer une variable en laissant le langage reconnaître son type.\n\n```go\npackage main\n\nimport "fmt"\n\nfunc main() {\n    prenom := "Camille"\n    fmt.Println("Bonjour", prenom)\n}\n```\n\nÀ votre avis, que va afficher ce programme ? Prenez le temps de le lire avant de répondre.',status:'complete',createdAt:'2026-10-25T10:01:00Z',trainingContext:active}]}],activeConversationId:'conversation-go',tutorStatus:{connected:true,label:'Institution pilote'},agentConnected:true};
 window.__layoutState=state;window.isTauri=true;
 window.__TAURI_EVENT_PLUGIN_INTERNALS__={unregisterListener(){}};
 window.__TAURI_INTERNALS__={transformCallback(){return 1;},unregisterCallback(){},invoke:async(command,args)=>{
  if(command==='load_state'||command==='select_conversation')return structuredClone(state);
  if(command==='bundled_agents')return [];
  if(command==='tutor_overview')return {identity:{tenantId:'demo',userId:'demo',membershipId:'demo',learnerId:'demo',roles:['owner','learner']},context:{},formations:{items:[]},availableFormations:{items:[]},capabilities:{learning:true}};
  if(command==='training_action'){
   if(args.action==='list_training_library')return {items:[training,{...training,id:'personal:python',name:'Python, de l’idée au script',description:'Automatiser les tâches du quotidien.',domain_id:'python'},{...training,id:'formation:astro',kind:'institutional',name:'Observer le ciel',description:'Une invitation à comprendre les constellations.',status:'published',audience:'open',domain_id:undefined,can_edit:false,can_share:false}],next_after:''};
   if(args.action==='get_training_policy')return {personal_learning:true,can_manage:true};
   if(args.action==='get_training_groups')return {items:[],next_after:''};
   return doc;
  }
  if(command.startsWith('plugin:event|'))return 1;
  throw new Error('Unsupported layout fixture command: '+command);
 }};
};
await send('Page.enable');await send('Runtime.enable');
await send('Page.addScriptToEvaluateOnNewDocument',{source:`(${fixture.toString()})();`});
await send('Emulation.setDeviceMetricsOverride',{width:1440,height:960,deviceScaleFactor:1,mobile:false});
await send('Page.navigate',{url:origin});
async function until(expression){for(let attempt=0;attempt<100;attempt++){if(await evaluate(expression))return;await new Promise(resolve=>setTimeout(resolve,100));}throw new Error('Page not ready: '+expression+'; browser errors: '+errors.join(' | '));}
await until('!!document.querySelector(".message-text")');
const report=[];
async function capture(name,width,height){
 await send('Emulation.setDeviceMetricsOverride',{width,height,deviceScaleFactor:1,mobile:false});
 await new Promise(resolve=>setTimeout(resolve,200));
 const dimensions=await evaluate(`({width:innerWidth,document:document.documentElement.scrollWidth,overflows:[...document.querySelectorAll('main,pre,.message-text,.message-table,.composer,.sidebar,.page-content')].filter(el=>el.scrollWidth>el.clientWidth+1).map(el=>({class:el.className,tag:el.tagName,width:el.clientWidth,scroll:el.scrollWidth}))})`);
 if(dimensions.document>width+1||dimensions.overflows.length)throw new Error(name+': '+JSON.stringify(dimensions));
 const image=await send('Page.captureScreenshot',{format:'png',captureBeyondViewport:false});await writeFile(join(output,name+'.png'),Buffer.from(image.data,'base64'));report.push({name,...dimensions});
}
await capture('conversation',1440,960);
await evaluate(`[...document.querySelectorAll('.navigation button')].find(el=>el.textContent==='Formations').click()`);await until('!!document.querySelector(".training-row")');
await capture('formations',1440,960);
await evaluate('document.querySelector(".training-row").click()');await until('!!document.querySelector(".programme-concept")');
await capture('programme',1440,960);
await capture('programme-compact',360,800);
await evaluate('document.querySelector(".navigation button").click()');await until('!!document.querySelector(".message-text")');
await capture('conversation-compact',360,800);
// Equivalent reflow width of a 1280 px window at 400% zoom.
await capture('conversation-zoom-400',320,800);
await send('Emulation.setDeviceMetricsOverride',{width:900,height:760,deviceScaleFactor:1,mobile:false});
const adversarial='```go\nconst long = "'+'x'.repeat(1200)+'";\n```\n\n| Un | Deux | Trois | Quatre | Cinq |\n|---|---|---|---|---|\n| '+Array(5).fill('contenu'.repeat(60)).join(' | ')+' |';
await evaluate(`window.__layoutState.conversations[0].messages[1].text=${JSON.stringify(adversarial)};location.reload()`);await until('!!document.querySelector(".message-text")');
// Reload reinstalls fixtures; mount the adversarial Markdown through its actual
// React component in this local-only CDP execution context.
await evaluate(`(async()=>{const react=await import('/node_modules/.vite/deps/react.js');const createElement=react.createElement||react.default.createElement;const reactDOM=await import('/node_modules/.vite/deps/react-dom_client.js');const createRoot=reactDOM.createRoot||reactDOM.default.createRoot;const {MessageBody}=await import('/src/MessageBody.tsx');createRoot(document.querySelector('.conversation-messages')).render(createElement(MessageBody,{text:${JSON.stringify(adversarial)}}));})()`);
await until('!!document.querySelector(".stacked-table-row")');await capture('long-code-table',360,800);
if(errors.length)throw new Error(errors.join('\n'));
await writeFile(join(output,'layout-report.json'),JSON.stringify({fixtureOnly:true,checks:report},null,2)+'\n');
await send('Page.close');ws.close();process.stdout.write(JSON.stringify({output,checks:report.length,overflow:false})+'\n');
