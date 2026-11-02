import { learningProtocolFences } from './learningActivityModel';

export interface TextAttachment {name:string;content:string}
const maxFileBytes = 32_000;
const maxMessageBytes = 64_000;
export const attachmentAccept = '.txt,.md,.csv,.json,.jsonl,.yaml,.yml,.xml,.html,.css,.js,.jsx,.ts,.tsx,.py,.go,.rs,.java,.c,.h,.cpp,.sh,.sql,.toml,.log';
const supported = new Set(attachmentAccept.split(','));
export async function readTextAttachments(files:File[]):Promise<TextAttachment[]> {
  if(files.length>5)throw new Error('Vous pouvez joindre jusqu’à cinq fichiers texte.');
  return Promise.all(files.map(async file=>{
    const extension = file.name.slice(file.name.lastIndexOf('.')).toLowerCase();
    if(!supported.has(extension))throw new Error('Choisissez un fichier texte ou de code. Les images et PDF ne sont pas pris en charge.');
    if(file.size>maxFileBytes)throw new Error('Chaque fichier peut contenir jusqu’à 32 Ko de texte.');
    const bytes=await file.arrayBuffer();
    let content:string;
    try{content=new TextDecoder('utf-8',{fatal:true}).decode(bytes);}catch{throw new Error('Le fichier doit être un texte encodé en UTF-8.');}
    if(content.includes('\0'))throw new Error('Ce fichier contient des données binaires. Choisissez un fichier texte.');
    return {name:file.name.replace(/[\u0000-\u001f\u007f]/g,'').slice(0,200),content};
  }));
}

/** A quoted envelope preserves the existing ACP text transport and durable outbox. */
export function withAttachments(message:string, files:TextAttachment[]):string {
  if(!files.length)return message;
  if(files.length>5)throw new Error('Vous pouvez joindre jusqu’à cinq fichiers texte.');
  const payload=`${message.trim() || 'Voici les fichiers à étudier.'}\n\n\`\`\`tutor-attachments\n${JSON.stringify({version:1,files})}\n\`\`\``;
  if(new TextEncoder().encode(payload).length>maxMessageBytes)throw new Error('Le message et ses fichiers dépassent 64 Ko. Retirez un fichier ou réduisez le texte.');
  return payload;
}

export function messageAttachments(message:string):TextAttachment[] {
  const fences=learningProtocolFences(message).filter(fence=>fence.tag==='tutor-attachments' && fence.closed);
  if(fences.length!==1)return [];
  try {
    const value=JSON.parse(fences[0].body);
    if(value.version!==1 || !Array.isArray(value.files) || value.files.length>5)return [];
    if(!value.files.every((file:TextAttachment)=>file && typeof file.name==='string' && file.name.length<=200 && typeof file.content==='string' && new TextEncoder().encode(file.content).length<=maxFileBytes))return [];
    return value.files;
  } catch{return [];}
}
