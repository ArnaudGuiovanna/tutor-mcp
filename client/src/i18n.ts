import { useSyncExternalStore } from 'react';
import { english } from './messages.en';

export type Language = 'fr' | 'en';
let language: Language = 'fr';
const subscribers = new Set<() => void>();
export function setInterfaceLanguage(next: Language) {
  document.documentElement.lang = next;
  document.title = next==='en'?'Tutor — your learning space':'Tutor — votre espace d’apprentissage';
  if (language === next) return;
  language = next;
  for (const subscriber of subscribers) subscriber();
}
export function currentLanguage() { return language; }
export function t(message: string, values: Record<string, string | number> = {}): string {
  let translated = language === 'en' ? english[message] || message : message;
  if(language==='en'&&!english[message]){
    const exited=/^L’agent s’est arrêté avec une erreur \(code (-?\d+)\)\. Vérifiez sa connexion et son installation\.$/.exec(message);
    if(exited)translated=`The agent stopped with an error (code ${exited[1]}). Check its sign-in and installation.`;
    else if(/ \(HTTP \d{3}\)\.$/.test(message)){const http=/^(.*) \(HTTP (\d{3})\)\.$/.exec(message)!;translated=t(http[1])+` (HTTP ${http[2]}).`;}
    else if(message.startsWith('Initialisation de l’agent : '))translated='Agent initialisation: '+t(message.slice('Initialisation de l’agent : '.length));
  }
  return translated.replace(/\{([A-Za-z0-9_]+)\}/g, (token, key: string) => Object.hasOwn(values, key) ? String(values[key]) : token);
}
export function useTranslation() {
  const selected = useSyncExternalStore(callback => { subscribers.add(callback); return () => { subscribers.delete(callback); }; }, currentLanguage, () => 'fr' as Language);
  return {t, language: selected};
}
export const dateLocale = () => language === 'en' ? 'en-GB' : 'fr-FR';
