import { t } from './i18n';
import { useEffect, useRef, useState } from 'react';
import type { ClientBridge } from './bridge';
import type { FormationReview, FormationVersion } from './types';
import { errorMessage, statusLabel } from './model';
import { MessageBody } from './MessageBody';

export function FormationReviewPanel({ formationId, api, canPublish, busy, onClose, onDiscuss, onPublish, onClone }: { formationId: string; api: ClientBridge; canPublish: boolean; busy: boolean; onClose: () => void; onDiscuss: (review: FormationReview) => void; onPublish: (versionId: string, key: string) => Promise<boolean>; onClone: (versionId: string, key: string) => Promise<FormationVersion | undefined> }) {
  const [versions, setVersions] = useState<FormationVersion[]>([]);
  const [versionId, setVersionId] = useState('');
  const [review, setReview] = useState<FormationReview | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [confirmed, setConfirmed] = useState(false);
  const [published, setPublished] = useState(false);
  const requestKey = useRef(crypto.randomUUID());
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    let live = true; setLoading(true); setError('');
    api.formationVersions(formationId).then((page) => {
      if (!live) return;
      if (page.items.some((version) => version.formation_id !== formationId)) throw new Error('Les versions ne correspondent pas à la formation sélectionnée.');
      setVersions(page.items); setVersionId(page.items[0]?.id || '');
      if (!page.items.length) setError('Cette formation ne contient pas encore de version.');
    }).catch((reason) => { if (live) setError(errorMessage(reason)); }).finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
  }, [api, formationId]);
  useEffect(() => {
    if (!versionId) return;
    let live = true; setLoading(true); setError(''); setReview(null); setConfirmed(false); setPublished(false); requestKey.current = crypto.randomUUID();
    api.formationReview(versionId).then((value) => {
      if (!live) return;
      if (value.detail.formation.id !== formationId || value.detail.version.id !== versionId) throw new Error('La version retournée ne correspond pas à la formation sélectionnée.');
      setReview(value); heading.current?.focus();
    }).catch((reason) => { if (live) setError(errorMessage(reason)); }).finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
  }, [api, formationId, versionId]);
  const detail = review?.detail;
  const alreadyPublished = detail?.version.status === 'published';
  async function publish() {
    if (!review || !confirmed || !canPublish) return;
    if (await onPublish(versionId, requestKey.current)) { setPublished(true); setConfirmed(false); setVersions(items=>items.map(version=>version.id===versionId?{...version,status:'published'}:version)); setReview({ ...review, publishable: false, catalog_available: true, detail: { ...review.detail, version: { ...review.detail.version, status: 'published' } } }); }
  }
  async function clone() {
    if (!review) return;
    const version = await onClone(versionId, crypto.randomUUID());
    if (version) { setVersions((items) => [version, ...items]); setVersionId(version.id); }
  }
  return <section className="card formation-review" aria-label={t("Examen de la formation")}>
    <div className="section-heading"><h2 ref={heading} tabIndex={-1}>{detail?.formation.name || t('Examiner la formation')}</h2><button className="button secondary" disabled={busy} onClick={onClose}>{t("Fermer l’examen")}</button></div>
    {versions.length > 0 && <label className="field">{t("Version à examiner")}<select value={versionId} disabled={loading || busy} onChange={(event) => setVersionId(event.target.value)}>{versions.map((version) => <option key={version.id} value={version.id}>{t("Version")} {version.version} — {statusLabel(version.status)}</option>)}</select></label>}
    {loading && <p role="status">{t("Lecture de la version et vérification du programme…")}</p>}
    {error && <p role="alert" className="notice error">{error}</p>}
    {detail && <>
      <p><strong>{t("État réel :")} {statusLabel(detail.version.status)}</strong> {t("· Version")} {detail.version.version} · {detail.concepts.length} {t("concepts")}</p>
      <MessageBody text={detail.formation.description} openLink={api.openExternalLink} />
      <h3>{t("Programme")}</h3>
      {detail.modules.map((module) => <section className="review-module" key={module.id}><h4>{module.title}</h4>{detail.concepts.filter((concept) => concept.module_stable_key === module.stable_key).map((concept) => <article className="review-concept" key={concept.id}><h5>{concept.label}</h5>{concept.description && <MessageBody text={concept.description} openLink={api.openExternalLink} />}{!!concept.outcomes?.length && <><h6>{t("Objectifs observables")}</h6><ul>{concept.outcomes.map((outcome) => <li key={outcome.id}>{outcome.statement}{outcome.observable && <p>{t("À observer :")} {outcome.observable}</p>}</li>)}</ul></>}{!!concept.criteria?.length && <><h6>{t("Critères de validation")}</h6><ul>{concept.criteria.map((criterion) => <li key={criterion.id}>{criterion.description}{criterion.evidence && <p>{t("Preuve attendue :")} {criterion.evidence}</p>}</li>)}</ul></>}</article>)}</section>)}
      {!alreadyPublished && <section className="publication-checks" aria-label={t("Vérifications avant publication")}><h3>{t("Avant publication")}</h3>{review.publishable ? <p>{t("Le programme peut être publié. Vérifiez son contenu avant de confirmer.")}</p> : <ul>{review.issues.map((issue, index) => <li key={index}>{issue}</li>)}</ul>}</section>}
      {(published || review.catalog_available) && <p role="status" className="notice success">{t("Cette version est publiée dans le catalogue de l’institution. Chaque membre peut choisir de s’y inscrire ; personne n’a été inscrit automatiquement.")}</p>}
      <div className="button-row"><button className="button secondary" disabled={busy} onClick={() => onDiscuss(review)}>{t("Concevoir avec mon agent")}</button>{alreadyPublished && canPublish && <button className="button secondary" disabled={busy} onClick={() => void clone()}>{t("Préparer une nouvelle version")}</button>}</div>
      {canPublish && detail.formation.status!=='archived' && !review.catalog_available && (review.publishable || alreadyPublished) && <section className="publication-confirmation"><h3>{alreadyPublished ? t("Ouvrir cette version aux membres") : t("Valider et publier")}</h3><p>{t("La version")} {detail.version.version} {t("sera proposée à tous les membres de cette institution. Les inscriptions restent volontaires. Le contenu publié sera conservé comme une version immuable.")}</p><label className="confirmation"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />{t("J’ai examiné cette version et je confirme sa publication dans le catalogue de mon institution.")}</label><button className="button primary" disabled={busy || !confirmed} onClick={() => void publish()}>{alreadyPublished ? t("Rendre accessible aux membres") : t("Publier dans mon institution")}</button></section>}
    </>}
  </section>;
}

export function NewFormation({ busy, onCreate, onCancel }: { busy: boolean; onCreate: (name: string, description: string, key: string) => void; onCancel: () => void }) {
  const [name, setName] = useState(''); const [description, setDescription] = useState('');
  const key = useRef(crypto.randomUUID());
  return <form className="card new-formation" aria-label={t("Créer une formation institutionnelle")} onSubmit={(event) => { event.preventDefault(); if (name.trim()) onCreate(name.trim(), description.trim(), key.current); }}><h2>{t("Créer une formation pour votre institution")}</h2><label className="field">{t("Titre de la formation")}<input value={name} onChange={(event) => setName(event.target.value)} maxLength={500} disabled={busy} required /></label><label className="field">{t("Public et objectif")}<textarea value={description} onChange={(event) => setDescription(event.target.value)} maxLength={5000} disabled={busy} placeholder={t("À qui s’adresse cette formation et que doit-elle permettre de comprendre ou de faire ?")} /></label><p>{t("Un brouillon sera créé. Vous pourrez construire le programme avec votre agent, l’examiner, puis décider de sa publication.")}</p><div className="button-row"><button className="button primary" disabled={busy || !name.trim()}>{t("Créer le brouillon")}</button><button type="button" className="button secondary" disabled={busy} onClick={onCancel}>{t("Annuler")}</button></div></form>;
}
