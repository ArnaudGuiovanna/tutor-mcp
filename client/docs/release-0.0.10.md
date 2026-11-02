# Tutor ACP 0.0.10 — workspace et connexions aux harness

Cette préversion Windows réunit la refonte du workspace et les corrections de
connexion ACP. Elle conserve le moteur local, son routage des demandes et les
limites de la [préversion locale 0.0.9](local-semantic-preview.md).

## Interface

- Rail Conversation, Formations, Planning et Quiz/flashcards ; recherche Ctrl K,
  nouvelle conversation Ctrl N, compte et paramètres toujours accessibles.
- Barre latérale masquable, état mémorisé, statut MCP en bas et surcouche sous
  1 100 px. Largeur minimale de la fenêtre : 960 px.
- Accueil clair/sombre, reprise de la dernière formation, choix de formation dans
  le compositeur, envoi Ctrl Entrée et arrêt de la réponse en cours.
- Messages apprenant en bulles, réponses du tuteur sans bulle, activité des outils
  dépliable, quiz au clavier avec correction et flashcards avec autoévaluation.
- Taille par défaut de 14 px ; les tailles de lecture déjà enregistrées restent
  appliquées. Les couleurs du thème clair existant sont conservées.

## Connexions ACP

Le client détecte et lance le harness sélectionné, en natif ou sous WSL, et
réutilise son accès existant. Il ne demande pas de choisir une facturation par clé
ou abonnement. Une authentification explicitement requise passe par l’adaptateur.

Les changements de serveur MCP, d’autorisations ou d’agent rafraîchissent les
sessions sans laisser l’ancien contexte actif. Les reconnexions sont bornées,
une déconnexion volontaire est respectée et un message refusé n’est pas rejoué.
Les limitations temporaires, refus d’accès et quotas signalés par le fournisseur
sont distingués, sans prétendre connaître le solde d’un abonnement.

## Fichiers, partage et titres

Le bouton **+** accepte jusqu’à cinq fichiers texte/code UTF-8, de 32 Ko chacun ;
le message complet reste limité à 64 Ko. Les fichiers sont conservés avec le
message en attente et envoyés à sa soumission. Images, PDF et fichiers binaires
ne sont pas encore pris en charge.

**Partager** présente le texte avant copie ou export Markdown. L’export ne contient
ni configuration de connexion, ni contenu des pièces jointes, ni réponses cachées
aux exercices. Il ne crée pas de lien public hébergé.

Le titre provisoire est remplacé par le titre renvoyé par le harness au premier
échange. Les consultations locales reconnues utilisent le sujet de la demande
confirmée. Aucun appel supplémentaire au modèle n’est lancé pour générer un titre.
Si le harness n’en renvoie pas, le titre provisoire reste disponible.

## Périmètre et vérifications

Le serveur MCP et le modèle local ne sont pas modifiés par cette release. Le
modèle GGUF existant est conservé. L’agent local reste une préversion CPU ; les
activités pédagogiques complètes et l’accélération P2000 restent à intégrer.

L’API n’exposant pas de taux global de formation terminée ni de nombre futur de
questions, l’interface affiche les états disponibles sans inventer de pourcentage,
de chapitre ou de total d’exercice.

La promotion sur main exige les 19 contrôles GitHub requis, dont la construction
et l’installation réelle du client Windows. Les rapports et sommes SHA-256 joints
à la release identifient son commit et son installateur. Ces contrôles ne prouvent
pas l’accès à un abonnement réel Codex/Claude ni les performances sur votre machine.
