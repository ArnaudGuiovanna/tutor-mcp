import { describe, expect, it } from 'vitest';
import { awaitingAuthorization, legacySetupComplete, normalizeTutorUrl, onboardingStep, tutorUrlErrors } from '../../src/onboardingModel';
import type { Snapshot } from '../../src/types';
import { conversation, initialState } from './clientFixture';

function fresh(): Snapshot {
  const state = initialState();
  state.settings.targetId = '';
  state.settings.tutor = { mode: 'remote', url: '', command: '', args: [] };
  return state;
}

describe('étape de la configuration guidée', () => {
  it('se dérive de l’état : compte, puis harness, puis serveur connecté', () => {
    const state = fresh();
    expect(onboardingStep(state)).toBe(1);
    state.identity = { exists: true, backupConfirmed: false, needsBackup: true, backupExported: true };
    expect(onboardingStep(state)).toBe(1);
    state.identity = { exists: true, backupConfirmed: true, needsBackup: false };
    expect(onboardingStep(state)).toBe(2);
    state.settings.targetId = 'bundled:claude';
    expect(onboardingStep(state)).toBe(3);
    state.settings.tutor.url = 'https://tutor.example.org';
    // During the setup (completion ignored), a saved but unconnected address still asks for step 3.
    expect(onboardingStep(state, { ignoreCompletion: true })).toBe(3);
    state.tutorStatus = { connected: true, label: 'Institution' };
    expect(onboardingStep(state, { ignoreCompletion: true })).toBe('done');
  });

  it('est terminée définitivement par setupCompleted, sauf lors d’une relance explicite', () => {
    const state = fresh(); state.settings.setupCompleted = true;
    expect(onboardingStep(state)).toBe('done');
    expect(onboardingStep(state, { ignoreCompletion: true })).toBe(1);
  });

  it('considère terminés les profils existants : agent choisi et Tutor local ou conversation existante', () => {
    expect(onboardingStep(initialState())).toBe('done');
    const remote = fresh(); remote.settings.targetId = 'bundled:codex';
    expect(onboardingStep(remote)).toBe(1);
    remote.conversations = [conversation];
    expect(onboardingStep(remote)).toBe('done');
    const local = fresh(); local.settings.tutor.mode = 'local';
    expect(onboardingStep(local)).toBe(1);
  });

  it('reprend une configuration neuve interrompue après l’enregistrement de l’adresse distante', () => {
    // Key saved, agent chosen, address saved, institution not yet reached (awaiting authorisation).
    const interrupted = fresh(); interrupted.settings.targetId = 'bundled:claude'; interrupted.settings.tutor.url = 'https://tutor.example.org';
    interrupted.identity = { exists: true, backupConfirmed: true, needsBackup: false };
    expect(legacySetupComplete(interrupted)).toBe(false);
    expect(onboardingStep(interrupted)).toBe(3);
    // A connection to another server proves nothing for this one.
    interrupted.settings.tutorConnectedOrigin = 'https://elsewhere.example';
    expect(onboardingStep(interrupted)).toBe(3);
    interrupted.settings.permissionPolicy = { origin: 'https://elsewhere.example', institutionId: 'tenant-1', agentId: 'bundled:claude', defaultDecision: 'ask', tools: {} };
    expect(onboardingStep(interrupted)).toBe(3);
  });

  it('considère configuré un profil distant déjà connecté avec succès, même sans conversation ni sauvegarde confirmée', () => {
    const remote = fresh(); remote.settings.targetId = 'bundled:claude'; remote.settings.tutor.url = 'https://tutor.example.org';
    remote.identity = { exists: true, backupConfirmed: false, needsBackup: true };
    remote.settings.tutorConnectedOrigin = 'https://tutor.example.org';
    expect(legacySetupComplete(remote)).toBe(true);
    expect(onboardingStep(remote)).toBe('done');
    // Older backends: a permission policy for this institution also proves a past connection.
    const policy = fresh(); policy.settings.targetId = 'bundled:claude'; policy.settings.tutor.url = 'https://tutor.example.org/';
    policy.settings.permissionPolicy = { origin: 'https://tutor.example.org', institutionId: 'tenant-1', agentId: 'bundled:claude', defaultDecision: 'ask', tools: {} };
    expect(legacySetupComplete(policy)).toBe(true);
    policy.settings.permissionPolicy.institutionId = '';
    expect(legacySetupComplete(policy)).toBe(false);
    // An address alone, without an agent, is not a configured profile.
    const addressOnly = fresh(); addressOnly.settings.tutor.url = 'https://tutor.example.org'; addressOnly.settings.tutorConnectedOrigin = 'https://tutor.example.org';
    expect(legacySetupComplete(addressOnly)).toBe(false);
    const blank = fresh(); blank.settings.targetId = 'bundled:claude'; blank.settings.tutor.url = '   ';
    expect(legacySetupComplete(blank)).toBe(false);
  });
});

describe('normalisation de l’adresse Tutor', () => {
  it.each([
    ['https://tutor.example.org', 'https://tutor.example.org'],
    ['  tutor.example.org  ', 'https://tutor.example.org'],
    ['https://tutor.example.org/mcp', 'https://tutor.example.org'],
    ['https://Tutor.Example.org:8443/mcp/?x=1#frag', 'https://tutor.example.org:8443'],
    ['http://localhost:8080/mcp', 'http://localhost:8080'],
    ['http://127.0.0.1:3000', 'http://127.0.0.1:3000'],
    ['http://[::1]:3000/', 'http://[::1]:3000'],
    ['https://école.example', 'https://xn--cole-9oa.example'],
  ])('%s → %s', (raw, expected) => {
    expect(normalizeTutorUrl(raw)).toEqual({ ok: true, url: expected });
  });

  it('donne un message distinct pour chaque refus', () => {
    expect(normalizeTutorUrl('   ')).toEqual({ ok: false, error: tutorUrlErrors.empty });
    expect(normalizeTutorUrl('https://exa mple.org')).toEqual({ ok: false, error: tutorUrlErrors.invalid });
    expect(normalizeTutorUrl('https://')).toEqual({ ok: false, error: tutorUrlErrors.invalid });
    expect(normalizeTutorUrl('ftp://tutor.example.org')).toEqual({ ok: false, error: tutorUrlErrors.scheme });
    expect(normalizeTutorUrl('https://lea:secret@tutor.example.org')).toEqual({ ok: false, error: tutorUrlErrors.credentials });
    expect(normalizeTutorUrl('http://tutor.example.org')).toEqual({ ok: false, error: tutorUrlErrors.insecure });
    expect(new Set(Object.values(tutorUrlErrors)).size).toBe(Object.keys(tutorUrlErrors).length);
  });
});

it('reconnaît une clé pas encore autorisée par l’institution', () => {
  expect(awaitingAuthorization('Clé non autorisée ou permissions insuffisantes : demandez l’accès à votre institution. (HTTP 403).')).toBe(true);
  expect(awaitingAuthorization('L’institution a refusé la demande signée. (HTTP 401).')).toBe(true);
  expect(awaitingAuthorization('L’institution a refusé la demande signée. (HTTP 500).')).toBe(false);
  expect(awaitingAuthorization('L’institution est inaccessible.')).toBe(false);
});
