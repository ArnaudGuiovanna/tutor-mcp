// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
import type { Snapshot } from './types';

export type OnboardingStep = 1 | 2 | 3 | 'done';
type SetupSnapshot = Pick<Snapshot, 'settings' | 'identity' | 'tutorStatus' | 'conversations'>;

function origin(url: string): string {
  try { return new URL(url.trim()).origin; } catch { return ''; }
}

/**
 * A saved remote address alone does not prove a configured profile: a new
 * setup stores it before the institution admits the key. Only persistent
 * traces of a successful connection to that same server count:
 * - `tutorConnectedOrigin`, written by the native layer after a successful
 *   remote connection (optional: absent from older backends);
 * - a tool permission policy, which requires the institution identity read
 *   from that connected server.
 */
export function remoteConnectionKnown(snapshot: SetupSnapshot): boolean {
  const settings = snapshot.settings;
  const server = origin(settings.tutor.url);
  if (settings.tutor.mode !== 'remote' || !server) return false;
  if (settings.tutorConnectedOrigin && origin(settings.tutorConnectedOrigin) === server) return true;
  const policy = settings.permissionPolicy;
  return !!policy && policy.origin === server && !!policy.institutionId;
}

/**
 * Profiles configured before the guided setup existed must not be blocked by
 * it: an agent was chosen and Tutor is local, already used, or a remote server
 * was already reached successfully.
 */
export function legacySetupComplete(snapshot: SetupSnapshot): boolean {
  const tutor = snapshot.settings.tutor;
  return !!snapshot.settings.targetId
    && (tutor.mode === 'local' || snapshot.conversations.length > 0 || remoteConnectionKnown(snapshot));
}

export function accountReady(snapshot: SetupSnapshot): boolean {
  return snapshot.identity.exists && snapshot.identity.backupConfirmed;
}

export function serverReady(snapshot: SetupSnapshot): boolean {
  const tutor = snapshot.settings.tutor;
  return snapshot.tutorStatus.connected && (tutor.mode === 'local' || !!tutor.url.trim());
}

/**
 * The step is derived from the snapshot only, so an interrupted setup resumes
 * where it stopped without any additional storage. `ignoreCompletion` reopens
 * the setup for an explicit review from the settings.
 */
export function onboardingStep(snapshot: SetupSnapshot, { ignoreCompletion = false }: { ignoreCompletion?: boolean } = {}): OnboardingStep {
  if (!ignoreCompletion && (snapshot.settings.setupCompleted === true || legacySetupComplete(snapshot))) return 'done';
  if (!accountReady(snapshot)) return 1;
  if (!snapshot.settings.targetId) return 2;
  if (!serverReady(snapshot)) return 3;
  return 'done';
}

export type TutorUrlResult = { ok: true; url: string } | { ok: false; error: string };

export const tutorUrlErrors = {
  empty: 'Saisissez l’adresse de votre serveur Tutor.',
  invalid: 'Cette adresse n’est pas une URL valide.',
  scheme: 'Utilisez une adresse qui commence par https://.',
  credentials: 'Retirez l’identifiant et le mot de passe de l’adresse.',
  insecure: 'Une adresse http:// n’est acceptée que pour un serveur sur cet ordinateur (localhost). Utilisez https://.',
} as const;

function loopback(hostname: string): boolean {
  return hostname === 'localhost' || hostname === '[::1]' || /^127(?:\.\d{1,3}){3}$/.test(hostname);
}

/**
 * The native layer accepts only an origin: HTTPS, or HTTP on loopback, without
 * path. A pasted MCP endpoint such as `https://host/mcp` is reduced to its origin.
 */
export function normalizeTutorUrl(raw: string): TutorUrlResult {
  const value = raw.trim();
  if (!value) return { ok: false, error: tutorUrlErrors.empty };
  if (/[\u0000-\u001f\u007f\s]/.test(value) || value.length > 2048) return { ok: false, error: tutorUrlErrors.invalid };
  const withScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(value) ? value : `https://${value}`;
  let url: URL;
  try { url = new URL(withScheme); } catch { return { ok: false, error: tutorUrlErrors.invalid }; }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') return { ok: false, error: tutorUrlErrors.scheme };
  if (!url.hostname || url.port === '0') return { ok: false, error: tutorUrlErrors.invalid };
  if (url.username || url.password) return { ok: false, error: tutorUrlErrors.credentials };
  if (url.protocol === 'http:' && !loopback(url.hostname)) return { ok: false, error: tutorUrlErrors.insecure };
  return { ok: true, url: url.origin };
}

/**
 * A signed session refused for an unknown key (HTTP 401) or an unauthorised
 * one (HTTP 403) means the institution has not admitted this public key yet.
 */
export function awaitingAuthorization(message: string): boolean {
  return message.startsWith('Clé non autorisée') || message === 'L’institution a refusé la demande signée. (HTTP 401).';
}
