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
let typedFailures = false;
const configs = {};
const configOptions = (id) => configs[id] ||= sessions[id]?.fixtureConfig || [
  {id:'model',category:'model',name:'Model',type:'select',currentValue:'fixture-standard',options:[{value:'fixture-standard',name:'Standard'},{value:'fixture-fast',name:'Fast'}]},
  {id:'effort',category:'thought_level',name:'Reasoning',type:'select',currentValue:'medium',options:[{value:'low',name:'Low'},{value:'medium',name:'Medium'},{value:'high',name:'High'}]},
  {id:'fast-mode',category:'model_config',name:'Fast mode',type:'select',currentValue:'off',options:[{value:'off',name:'Off'},{value:'on',name:'On'}]},
];
const send = (value) => process.stdout.write(`${JSON.stringify(value)}\n`);
const result = (request, value) => send({jsonrpc:'2.0', id:request.id, result:value});
const chunk = (sessionId, text) => send({jsonrpc:'2.0',method:'session/update',params:{sessionId,update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text}}}});
readline.createInterface({input:process.stdin}).on('line', (line) => {
  const request = JSON.parse(line);
  const params = request.params || {};
  if (request.method === 'initialize') {
    if (!local) {
      fs.writeFileSync(path.join(process.cwd(), 'fixture-agent-initialize.json'), JSON.stringify(params));
      const air=params.clientCapabilities?._meta?.jetbrains?.air;
      typedFailures=air?.version===1 && air.capabilities?.includes('sessionFailure');
    }
    result(request, local ? {protocolVersion:params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'fixture-tutor',version:'1'}} : {protocolVersion:1,agentCapabilities:{loadSession:true,mcpCapabilities:{http:true}},agentInfo:{name:'fixture-acp',version:'1'},authMethods:[]});
  } else if (request.method === 'tools/list') {
    result(request,{tools:[{name:'get_next_activity',description:'Fixture next activity',inputSchema:{type:'object'}}]});
  } else if (request.method === 'tools/call') {
    const value = params.name === 'get_learner_context' ? {learner:{id:'learner-1'},domains:[{domain_id:'domain-1',name:'Astronomie',concept_count:1}]} : {items:[]};
    result(request,{content:[{type:'text',text:JSON.stringify(value)}],structuredContent:value});
  } else if (request.method === 'session/new') {
    const sessionId = `session-${Object.keys(sessions).length + 1}`;
    sessions[sessionId] = params;
    fs.writeFileSync(sessionFile, JSON.stringify(sessions));
    result(request,{sessionId,configOptions:configOptions(sessionId)});
  } else if (request.method === 'session/load') {
    if (!sessions[params.sessionId]) return send({jsonrpc:'2.0',id:request.id,error:{code:-32602,message:'Unknown session'}});
    sessions[params.sessionId] = {...sessions[params.sessionId],...params};
    fs.writeFileSync(sessionFile, JSON.stringify(sessions));
    chunk(params.sessionId,'ANCIEN TEXTE À NE PAS RÉAJOUTER');
    result(request,{configOptions:configOptions(params.sessionId)});
  } else if (request.method === 'session/set_config_option') {
    const options=configOptions(params.sessionId);
    const option=options.find(option=>option.id===params.configId);
    if(!option || !option.options.some(choice=>choice.value===params.value)) return send({jsonrpc:'2.0',id:request.id,error:{code:-32602,message:'Unsupported config'}});
    option.currentValue=params.value;
    sessions[params.sessionId].fixtureConfig=options;
    fs.writeFileSync(sessionFile,JSON.stringify(sessions));
    result(request,{configOptions:options});
  } else if (request.method === 'session/prompt') {
    const text = params.prompt.map((part) => part.text || '').join('');
    sessions[params.sessionId].lastPrompt=text;fs.writeFileSync(sessionFile,JSON.stringify(sessions));
    waiting = request;
    const experience=text.match(/EXPERIENCE_PERMISSION:([A-Z_]+)$/)?.[1];
    const typed=text.match(/EXPERIENCE_TYPED:([A-Z_]+)$/)?.[1];
    if (typed) {
      if(!typedFailures) return send({jsonrpc:'2.0',id:request.id,error:{code:-32602,message:'Fixture requires typed failure capability'}});
      const failure={id:'incident-1',revision:1,category:'limit',severity:typed==='WARNING'?'warning':'error',title:'private-provider-token',details:'private-provider-token',actions:['retry']};
      const metadata=(value)=>({_meta:{jetbrains:{air:{version:1,sessionFailure:value}}}});
      const update=(sessionId,value)=>send({jsonrpc:'2.0',method:'session/update',params:{sessionId,update:{sessionUpdate:'session_info_update',...metadata(value)}}});
      if(typed==='RESULT_ERROR') {
        chunk(params.sessionId,'Réponse partielle.');
        result(request,{stopReason:'end_turn',...metadata(failure)});
      } else {
        if(typed==='MALFORMED')failure.severity='invalid';
        update(typed==='STALE'?'old-session':params.sessionId,failure);
        if(typed==='WARNING' || typed==='CLEARED_ERROR')update(params.sessionId,null);
        chunk(params.sessionId,typed==='WARNING'||typed==='STALE'?'Réponse récupérée.':'private-provider-token');
        result(request,{stopReason:'end_turn'});
      }
      waiting=undefined;
    } else if (text.endsWith('EXPERIENCE_HTTP')) {
      const server=sessions[params.sessionId].mcpServers[0];
      const url=new URL(server.url);
      const payload=JSON.stringify({jsonrpc:'2.0',id:'experience-http',method:'tools/call',params:{name:'get_next_activity',arguments:{}}});
      const headers=Object.fromEntries(server.headers.map(item=>[item.name,item.value]));
      Object.assign(headers,{'Content-Type':'application/json','Content-Length':Buffer.byteLength(payload)});
      const call=require(url.protocol==='https:'?'node:https':'node:http').request(url,{method:'POST',headers},response=>{
        let body='';response.setEncoding('utf8');response.on('data',piece=>body+=piece);response.on('end',()=>{
          chunk(params.sessionId,JSON.parse(body).result.isError?'Server denied tool.':'Tool completed.');
          result(request,{stopReason:'end_turn'});waiting=undefined;
        });
      });
      call.on('error',()=>{chunk(params.sessionId,'Fixture HTTP failed.');result(request,{stopReason:'end_turn'});waiting=undefined;});
      call.end(payload);
    } else if (experience) {
      const correlated=['CORRELATED','STALE','COMPLETED','QUESTION'].includes(experience);
      const toolCall={toolCallId:'experience-call',title:'This title is deliberately not an authorization identity'};
      if (experience==='CLAUDE') toolCall._meta={claudeCode:{mcpServer:{name:'tutor'},toolName:'mcp__tutor__get_next_activity'}};
      else if(!correlated) toolCall.rawInput={server:'tutor',tool:experience==='UNKNOWN'?'unknown_tool':'get_next_activity',arguments:{}};
      if(correlated) {
        const sessionId=experience==='STALE'?'old-session':params.sessionId;
        send({jsonrpc:'2.0',method:'session/update',params:{sessionId,update:{sessionUpdate:'tool_call',toolCallId:'experience-call',status:'pending',rawInput:{server:'tutor',tool:'get_next_activity',arguments:{}}}}});
        if(experience==='COMPLETED') send({jsonrpc:'2.0',method:'session/update',params:{sessionId,update:{sessionUpdate:'tool_call_update',toolCallId:'experience-call',status:'completed'}}});
      }
      const options=[{optionId:'allow',name:'Allow once',kind:'allow_once'}];
      if(experience!=='NO_REJECT')options.push({optionId:'reject',name:'Reject once',kind:'reject_once'});
      send({jsonrpc:'2.0',id:'permission-experience',method:'session/request_permission',params:{sessionId:params.sessionId,toolCall,options,_meta:{is_mcp_tool_approval:experience!=='QUESTION'}}});
    } else if (text.endsWith('PERMISSION')) {
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
  } else if (request.id === 'permission-experience' && request.result && waiting) {
    sessions[waiting.params.sessionId].experienceOutcome=request.result.outcome;
    fs.writeFileSync(sessionFile,JSON.stringify(sessions));
    chunk(waiting.params.sessionId,request.result.outcome.outcome==='cancelled'?'Cancelled.':request.result.outcome.optionId==='allow'?'Allowed.':'Rejected.');
    result(waiting,{stopReason:'end_turn'});waiting=undefined;
  } else if (request.id === 'permission-1' && request.result && waiting) {
    chunk(waiting.params.sessionId,request.result.outcome.optionId === 'allow' ? 'Autorisé.' : 'Refusé.');
    result(waiting,{stopReason:'end_turn'});
    waiting = undefined;
  } else if (request.id !== undefined) result(request,{});
});
