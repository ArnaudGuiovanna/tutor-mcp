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
const fixedLocalModel = fs.existsSync(path.join(process.cwd(), 'fixture-local-model'));
const localModel = fixedLocalModel ? require('../../local-agent/model-profile.json').id : null;
const configOptions = (id) => configs[id] ||= sessions[id]?.fixtureConfig || (fixedLocalModel ? [
  {id:'model',category:'model',name:'Model',type:'select',currentValue:localModel,options:[{value:localModel,name:'Qwen local'}]},
] : [
  {id:'model',category:'model',name:'Model',type:'select',currentValue:'fixture-standard',options:[{value:'fixture-standard',name:'Standard'},{value:'fixture-fast',name:'Fast'}]},
  {id:'effort',category:'thought_level',name:'Reasoning',type:'select',currentValue:'medium',options:[{value:'low',name:'Low'},{value:'medium',name:'Medium'},{value:'high',name:'High'}]},
  {id:'fast-mode',category:'model_config',name:'Fast mode',type:'select',currentValue:'off',options:[{value:'off',name:'Off'},{value:'on',name:'On'}]},
]);
const send = (value) => process.stdout.write(`${JSON.stringify(value)}\n`);
const result = (request, value) => send({jsonrpc:'2.0', id:request.id, result:value});
const chunk = (sessionId, text) => send({jsonrpc:'2.0',method:'session/update',params:{sessionId,update:{sessionUpdate:'agent_message_chunk',content:{type:'text',text}}}});
readline.createInterface({input:process.stdin}).on('line', async (line) => {
  const request = JSON.parse(line);
  const params = request.params || {};
  // Metadata only: qualifications can count work without retaining prompts,
  // provider credentials or MCP headers. Each test owns its fresh cwd.
  fs.appendFileSync(path.join(process.cwd(), 'fixture-rpc.jsonl'), JSON.stringify({
    local, method:request.method, tool:params.name, sessionId:params.sessionId,
    promptBytes:request.method==='session/prompt'?Buffer.byteLength(JSON.stringify(params.prompt)):undefined,
  })+'\n');
  const delayFile=path.join(process.cwd(),'fixture-delays.json');
  const delays=fs.existsSync(delayFile)?JSON.parse(fs.readFileSync(delayFile,'utf8')):{};
  const delay=request.method==='tools/call'?delays.toolMs:request.method==='session/load'?delays.loadMs:request.method==='session/new'?delays.newMs:request.method==='session/prompt'?delays.promptMs:0;
  if(Number.isFinite(delay)&&delay>0) await new Promise(resolve=>setTimeout(resolve,Math.min(delay,10000)));
  if (request.method === 'initialize') {
    if (!local) {
      fs.writeFileSync(path.join(process.cwd(), 'fixture-agent-initialize.json'), JSON.stringify(params));
      const air=params.clientCapabilities?._meta?.jetbrains?.air;
      typedFailures=air?.version===1 && air.capabilities?.includes('sessionFailure');
    }
    result(request, local ? {protocolVersion:params.protocolVersion,capabilities:{tools:{}},serverInfo:{name:'fixture-tutor',version:'1'}} : {protocolVersion:1,agentCapabilities:{loadSession:true,mcpCapabilities:{http:true}},agentInfo:{name:'fixture-acp',version:'1'},authMethods:[]});
    if (!local && fs.existsSync(path.join(process.cwd(), 'fixture-auth-status.json'))) {
      send({jsonrpc:'2.0',method:'_auth/status_update',params:{authStatus:JSON.parse(fs.readFileSync(path.join(process.cwd(),'fixture-auth-status.json'),'utf8'))}});
    }
  } else if (request.method === 'tools/list') {
    if(fs.existsSync('fixture-pedagogy.json')) return result(request,{tools:['prepare_pedagogical_turn','prepare_assessment_attempt','submit_assessment_attempt','record_interaction','record_learning_event','claim_memory_maintenance','commit_memory_maintenance'].map(name=>({name,inputSchema:{type:'object'}}))});
    result(request,{tools:[{name:'get_next_activity',description:'Fixture next activity',inputSchema:{type:'object'}}]});
  } else if (request.method === 'tools/call') {
    if(fs.existsSync('fixture-pedagogy.json')) {
      const fixture=JSON.parse(fs.readFileSync('fixture-pedagogy.json','utf8'));
      if(params.name==='claim_memory_maintenance') return result(request,{structuredContent:fixture.batch || {status:'idle'}});
      if(params.name==='commit_memory_maintenance') {fs.writeFileSync('fixture-maintenance-committed',JSON.stringify(params));return result(request,{structuredContent:{ok:true,completed:true}});}
      if(params.name==='get_conversation_context') return result(request,{structuredContent:{...fixture.packet.conversation,conversation_id:params.arguments.conversation_id}});
      const native=['prepare_pedagogical_turn','prepare_assessment_attempt','submit_assessment_attempt','record_interaction','record_learning_event'];
      if(native.includes(params.name)) {
        fs.appendFileSync('fixture-pedagogy-log.jsonl',JSON.stringify(params)+'\n');
        if(!params._meta?.['tutor/conversation_id']) return result(request,{isError:true,content:[{type:'text',text:'Missing native binding'}]});
        const args=params.arguments;
        let value;
        if(params.name==='prepare_pedagogical_turn') value={...fixture.packet,mode:args.mode,conversation:{...fixture.packet.conversation,conversation_id:args.conversation_id}};
        if(params.name==='prepare_assessment_attempt') {fs.writeFileSync('fixture-prepared-attempt.json',JSON.stringify(args));value={attempt_id:'attempt-1'};}
        if(params.name==='submit_assessment_attempt') {fs.writeFileSync('fixture-submitted-attempt.json',JSON.stringify(args));value={attempt_id:args.attempt_id,status:'submitted'};}
        if(params.name==='record_interaction') {
          if(fs.existsSync('fixture-fail-commit')) return result(request,{isError:true});
          if(args.attempt_id&&!fs.existsSync('fixture-submitted-attempt.json')) return result(request,{isError:true});
          value={updated:true,assessment_attempt_id:args.attempt_id,assessment_status:'evaluated',derived_success:args.success};
        }
        if(params.name==='record_learning_event') value={event:{event_key:args.event_key},replayed:false,model_updated:false};
        return result(request,{structuredContent:value});
      }
    }
    if(params.name==='get_conversation_context') {
      while(fs.existsSync(path.join(process.cwd(),'fixture-hold-context-refresh'))) await new Promise(resolve=>setTimeout(resolve,5));
    }
    const value = params.name === 'get_learner_context' ? {learner_id:'learner-1',learner:{id:'learner-1'},domains:[{domain_id:'domain-1',name:'Astronomie',concept_count:1}]} : {items:[]};
    result(request,{content:[{type:'text',text:JSON.stringify(value)}],structuredContent:value});
  } else if (request.method === 'session/new') {
    const sessionId = `session-${Object.keys(sessions).length + 1}`;
    sessions[sessionId] = params;
    fs.writeFileSync(sessionFile, JSON.stringify(sessions));
    result(request,{sessionId,configOptions:configOptions(sessionId)});
  } else if (request.method === 'session/load' || (fixedLocalModel && request.method === '_tutor/rebind')) {
    if (!sessions[params.sessionId]) return send({jsonrpc:'2.0',id:request.id,error:{code:-32602,message:'Unknown session'}});
    sessions[params.sessionId] = {...sessions[params.sessionId],...params};
    fs.writeFileSync(sessionFile, JSON.stringify(sessions));
    if (request.method === 'session/load') {
      const count=Number.isInteger(delays.replayChunks)?Math.max(0,Math.min(delays.replayChunks,10000)):1;
      for(let i=0;i<count;i++) chunk(params.sessionId,'ANCIEN TEXTE À NE PAS RÉAJOUTER');
    }
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
    sessions[params.sessionId].lastPrompt=text;sessions[params.sessionId].promptCount=(sessions[params.sessionId].promptCount||0)+1;fs.writeFileSync(sessionFile,JSON.stringify(sessions));
    waiting = request;
    if(fs.existsSync('fixture-pedagogy.json')) {
      const fixture=JSON.parse(fs.readFileSync('fixture-pedagogy.json','utf8'));
      chunk(params.sessionId,fixture.output);
      while(fs.existsSync('fixture-hold-generation')) await new Promise(resolve=>setTimeout(resolve,5));
      result(request,{stopReason:fixture.stopReason || 'end_turn'});waiting=undefined;return;
    }
    const experience=text.match(/EXPERIENCE_PERMISSION:([A-Z_]+)$/)?.[1];
    const typed=text.match(/EXPERIENCE_TYPED:([A-Z_]+)$/)?.[1];
    const rpcFailure=text.match(/EXPERIENCE_RPC:([A-Z_]+)$/)?.[1];
    if(text.endsWith('WORKSPACE_TITLE_BLOCK')) {
      chunk(params.sessionId,'Votre profil est chargé.\n\n```tutor-conversation-title\n{"title":"Mon profil d’apprenant"}\n```');
      result(request,{stopReason:'end_turn'});waiting=undefined;
    } else if(text.endsWith('WORKSPACE_TITLE_EVENT')) {
      send({jsonrpc:'2.0',method:'session/update',params:{sessionId:'stale-session',update:{sessionUpdate:'session_info_update',title:'Titre périmé'}}});
      send({jsonrpc:'2.0',method:'session/update',params:{sessionId:params.sessionId,update:{sessionUpdate:'session_info_update',title:'Comprendre les boucles'}}});
      chunk(params.sessionId,'Une boucle répète une action.');result(request,{stopReason:'end_turn'});waiting=undefined;
    } else if (rpcFailure) {
      const codexErrorInfo={RATE:{httpConnectionFailed:{httpStatusCode:429}},QUOTA:'usageLimitExceeded',AUTH:'unauthorized',UNKNOWN:'other'}[rpcFailure];
      const errorKind={CLAUDE_RATE:'rate_limit',CLAUDE_BILLING:'billing_error'}[rpcFailure];
      send({jsonrpc:'2.0',id:request.id,error:{code:-32603,message:'private-provider-token',data:{message:'private-provider-token',codexErrorInfo,errorKind}}});
      waiting=undefined;
    } else if (typed) {
      if(!typedFailures) return send({jsonrpc:'2.0',id:request.id,error:{code:-32602,message:'Fixture requires typed failure capability'}});
      const failure={id:'incident-1',revision:1,category:'limit',severity:typed==='WARNING'?'warning':'error',title:'private-provider-token',details:'private-provider-token',actions:['retry']};
      if(typed==='NO_CAUSE') failure.actions=[];
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
    } else if (text.endsWith('CONTEXT_REFRESH')) {
      fs.writeFileSync(path.join(process.cwd(),'fixture-hold-context-refresh'),'hold');
      send({jsonrpc:'2.0',method:'session/update',params:{sessionId:params.sessionId,update:{sessionUpdate:'tool_call',toolCallId:'changed-training',status:'completed',rawInput:{server:'tutor',tool:'select_conversation_training',arguments:{}}}}});
      chunk(params.sessionId,'Formation changée.');
      result(request,{stopReason:'end_turn'});
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
