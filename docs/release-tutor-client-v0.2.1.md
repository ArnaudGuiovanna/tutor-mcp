# Tutor Desktop 0.2.1

Corrige « Permissions de session inconnues » lors de la connexion avec une clé
publique. La version 0.2.0 demandait `formation:contribute`, puis refusait ce droit
dans sa propre validation locale, avant tout appel au serveur.

Le client accepte maintenant les combinaisons de droits nécessaires au
propriétaire, au formateur et à l’apprenant. Les droits inconnus, les doublons et
les demandes non canoniques sont refusés avant toute signature. Les autorisations
effectives restent contrôlées par le serveur ; le client n’ajoute aucun rôle.

Les tests utilisent la fonction de connexion appelée par l’interface, avec un
serveur HTTP de test qui vérifie les signatures NIP-98, les droits demandés et
la révocation de la session intermédiaire. Ils couvrent aussi le propriétaire
sans profil apprenant et l’absence de délégation administrative automatique.

Installer `Tutor_0.2.1_x64-setup.exe`, puis reconnecter le compte à Tutor.
Les coffres, sauvegardes, clés, conversations et progressions existants sont
conservés. Aucune réimportation ni régénération de clé n’est nécessaire.

Les sommes SHA-256 et les rapports Windows accompagnent cette release.
