// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
// Deterministic subprocess fixture. Never imported into the application bundle.
const fs = require('node:fs');
const path = require('node:path');
const readline = require('node:readline');
if (process.argv.includes('--self-test')) {
  process.stdout.write(`${JSON.stringify({fixture:'tutor-client-backend',cwd:process.cwd()})}\n`);
  process.exit(0);
}
const local = process.argv.includes('--local');
const sessionFile = path.join(process.cwd(), 'fixture-agent-sessions.json');
let sessions = fs.existsSync(sessionFile) ? JSON.parse(fs.readFileSync(sessionFile, 'utf8')) : {};
let waiting;
const send = (value) => process.stdout.write(`${JSON.stringify(value)}\n`);
const result = (request, value) => send({jsonrpc:'2.0', id:request.id, result:value});
const chunk = (sessionId, text) => send({jsonrpc:'2.0',method:'session/update',params:{sessionId,update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text}}}});
readline.createInterface({input:process.stdin}).on('line', (line) => {
  const request = JSON.parse(line);
  const params = request.params || {};
  if (request.method === 'initialize') {
    result(request, local ? {protocolVersion:params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'fixture-tutor',version:'1'}} : {protocolVersion:1,agentCapabilities:{loadSession:true,mcpCapabilities:{http:true}},agentInfo:{name:'fixture-acp',version:'1'},authMethods:[]});
  } else if (request.method === 'tools/call') {
    const value = params.name === 'get_learner_context' ? {learner:{id:'learner-1'},domains:[{domain_id:'domain-1',name:'Astronomie',concept_count:1}]} : {items:[]};
    result(request,{content:[{type:'text',text:JSON.stringify(value)}],structuredContent:value});
  } else if (request.method === 'session/new') {
    const sessionId = `session-${Object.keys(sessions).length + 1}`;
    sessions[sessionId] = params;
    fs.writeFileSync(sessionFile, JSON.stringify(sessions));
    result(request,{sessionId});
  } else if (request.method === 'session/load') {
    if (!sessions[params.sessionId]) return send({jsonrpc:'2.0',id:request.id,error:{code:-32602,message:'Unknown session'}});
    chunk(params.sessionId,'ANCIEN TEXTE À NE PAS RÉAJOUTER');
    result(request,{});
  } else if (request.method === 'session/prompt') {
    const text = params.prompt.map((part) => part.text || '').join('');
    waiting = request;
    if (text.endsWith('PERMISSION')) {
      send({jsonrpc:'2.0',id:'permission-1',method:'session/request_permission',params:{sessionId:params.sessionId,toolCall:{toolCallId:'t1',title:'Utiliser Tutor'},options:[{optionId:'allow',name:'Autoriser',kind:'allow_once'},{optionId:'reject',name:'Refuser',kind:'reject_once'}]}});
    } else if (text.endsWith('WAIT')) {
      chunk(params.sessionId,'Réponse commencée.');
    } else if (text.endsWith('CRASH')) {
      chunk(params.sessionId,'Réponse interrompue.');
      process.stdout.write('', () => process.exit(42));
    } else {
      // Consecutive chunks and response exercise the backend's event barrier.
      chunk(params.sessionId,'Bonjour, ');
      chunk(params.sessionId,'élève.');
      result(request,{stopReason:'end_turn'});
    }
  } else if (request.method === 'session/cancel' && waiting) {
    result(waiting,{stopReason:'cancelled'});
    waiting = undefined;
  } else if (request.id === 'permission-1' && request.result && waiting) {
    chunk(waiting.params.sessionId,request.result.outcome.optionId === 'allow' ? 'Autorisé.' : 'Refusé.');
    result(waiting,{stopReason:'end_turn'});
    waiting = undefined;
  } else if (request.id !== undefined) result(request,{});
});
