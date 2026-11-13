// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
import type { JsonRecord, PromptResult } from './types';

/**
 * Why connection, Tutor and agent settings controls are disabled while a model
 * turn runs. The native side refuses the same operations at once, each with
 * its own message (« Interrompez la réponse en cours avant … »), including a
 * voluntary disconnection: the learner stops the response first.
 */
export const turnLockReason = 'Interrompez la réponse en cours pour modifier la connexion ou les réglages de l’agent.';

/** Short follow-up sent by « Continuer » after a turn reached its action limit. */
export const turnLimitFollowUp = 'Continue là où tu t’es arrêté, en reprenant ta dernière étape.';

/** A turn interrupted by its action budget, attached to its conversation and, once known, its assistant message. */
export interface TurnLimit { conversationId: string; limit: number; enforcedBy?: 'client' | 'agent'; messageId?: string }

export function turnLimitFromEvent(event: JsonRecord): TurnLimit | undefined {
  if (event.type !== 'turn_limit' || typeof event.conversationId !== 'string' || !event.conversationId) return;
  const limit = typeof event.limit === 'number' && Number.isFinite(event.limit) && event.limit > 0 ? event.limit : undefined;
  if (!limit) return;
  const enforcedBy = event.enforcedBy === 'client' || event.enforcedBy === 'agent' ? event.enforcedBy : undefined;
  return { conversationId: event.conversationId, limit, enforcedBy };
}

export function turnLimitFromResult(conversationId: string, result: PromptResult): TurnLimit | undefined {
  if (result.stopReason !== 'max_turn_requests') return;
  const limit = typeof result.turnLimit === 'number' && result.turnLimit > 0 ? result.turnLimit : undefined;
  return limit ? { conversationId, limit } : undefined;
}

/** Turn identity the interface accepts session events for. */
export interface TurnSessionState {
  conversationId: string;
  /** The native turn (assistant message) first named by a tagged event of this turn. */
  turnId?: string;
}

/**
 * The native backend tags every harness event of the running turn with its
 * conversation (`conversationId`) and turn (`turnId`), whichever session
 * (MCP or private) runs it. A session event without that tag belongs to no
 * running turn — a replayed history, a background session, or a late chunk of
 * a turn already finished or cancelled — and never feeds the current one.
 */
export function acceptTurnEvent(event: JsonRecord, turn: TurnSessionState): boolean {
  const conversationId = typeof event.conversationId === 'string' ? event.conversationId : '';
  if (!conversationId || conversationId !== turn.conversationId) return false;
  const turnId = typeof event.turnId === 'string' ? event.turnId : '';
  if (turnId) {
    if (turn.turnId && turn.turnId !== turnId) return false;
    turn.turnId = turnId;
  }
  return true;
}

/** Whether a turn-level event (`turn_error`, `turn_limit`) concerns this turn; untagged events are accepted. */
export function eventForTurn(event: JsonRecord, conversationId: string | undefined, turnId?: string): boolean {
  if (typeof event.conversationId === 'string' && event.conversationId && conversationId && event.conversationId !== conversationId) return false;
  return !(typeof event.turnId === 'string' && event.turnId && turnId && event.turnId !== turnId);
}
