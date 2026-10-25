// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Local BIP340 identity. Only the active key enters the device vault. The
//! distinct recovery key lives in memory until backup confirmation, then only
//! in the user's portable NIP-49 backup. Neither key is returned over IPC.
use nostr::{
    nips::nip49::{EncryptedSecretKey, KeySecurity},
    FromBech32, Keys, SecretKey, ToBech32,
};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use zeroize::{Zeroize, Zeroizing};

const VAULT: &str = "native-identity.json";
const BACKUP_LIMIT: u64 = 16 * 1024;
const LOG_N: u8 = 16; // Standard NIP-49 scrypt, 64 MiB per encrypted key.

#[derive(Default, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PublicIdentity {
    pub exists: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub npub: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub recovery_npub: Option<String>,
    pub backup_confirmed: bool,
    pub backup_exported: bool,
    pub needs_backup: bool,
}

#[derive(Serialize, Deserialize)]
struct StoredIdentity {
    version: u32,
    secret: String,
    recovery_public_key: String,
    backup_confirmed: bool,
}
impl Drop for StoredIdentity {
    fn drop(&mut self) {
        self.secret.zeroize();
    }
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PortableBackup {
    format: String,
    version: u32,
    created_at: String,
    pub npub: String,
    pub recovery_npub: String,
    identity_ncryptsec: String,
    recovery_ncryptsec: String,
}

pub struct KeyPair {
    pub primary: Keys,
    pub recovery: Keys,
}
impl KeyPair {
    pub fn generate() -> Self {
        Self {
            primary: Keys::generate(),
            recovery: Keys::generate(),
        }
    }
    pub fn backup(&self, path: &Path, password: &str) -> Result<(), String> {
        if password.chars().count() < 12 || password.len() > 4096 {
            return Err("La phrase de sauvegarde doit contenir au moins 12 caractères (4 096 octets maximum).".into());
        }
        if path.exists() {
            return Err("Ce fichier existe déjà. Choisissez un nouveau fichier afin de préserver vos sauvegardes.".into());
        }
        let encrypt = |keys: &Keys| -> Result<String, String> {
            EncryptedSecretKey::new(keys.secret_key(), password, LOG_N, KeySecurity::Medium)
                .map_err(|_| "Impossible de chiffrer la sauvegarde NIP-49.")?
                .to_bech32()
                .map_err(|_| "Impossible d’encoder la sauvegarde NIP-49.".into())
        };
        let backup = PortableBackup {
            format: "tutor-identity-nip49".into(),
            version: 1,
            created_at: crate::workspace::now(),
            npub: npub(&self.primary),
            recovery_npub: npub(&self.recovery),
            identity_ncryptsec: encrypt(&self.primary)?,
            recovery_ncryptsec: encrypt(&self.recovery)?,
        };
        let bytes = serde_json::to_vec_pretty(&backup)
            .map_err(|_| "Impossible de préparer la sauvegarde.")?;
        // Reserve without following an existing symlink or overwriting a backup.
        let mut options = std::fs::OpenOptions::new();
        options.write(true).create_new(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        use std::io::Write;
        let mut file=options.open(path).map_err(|_| "Impossible de créer le fichier de sauvegarde. Choisissez un nouveau fichier accessible.")?;
        file.write_all(&bytes).and_then(|_| file.sync_all()).map_err(|_| "La sauvegarde n’a pas pu être écrite complètement. Conservez la clé active et recommencez dans un nouveau fichier.")?;
        let readback =
            std::fs::read(path).map_err(|_| "Impossible de relire la sauvegarde écrite.")?;
        if readback != bytes {
            return Err("La vérification du fichier de sauvegarde a échoué.".into());
        }
        Ok(())
    }
    pub fn restore(path: &Path, password: &str) -> Result<Self, String> {
        let metadata =
            std::fs::symlink_metadata(path).map_err(|_| "Impossible de lire la sauvegarde.")?;
        if !metadata.file_type().is_file() || metadata.len() > BACKUP_LIMIT || password.len() > 4096
        {
            return Err("Sauvegarde invalide ou trop volumineuse.".into());
        }
        use std::io::Read;
        let mut bytes = Vec::new();
        std::fs::File::open(path)
            .map_err(|_| "Impossible de lire la sauvegarde.")?
            .take(BACKUP_LIMIT + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| "Impossible de lire la sauvegarde.")?;
        if bytes.len() as u64 > BACKUP_LIMIT {
            return Err("Sauvegarde trop volumineuse.".into());
        }
        let backup: PortableBackup = serde_json::from_slice(&bytes)
            .map_err(|_| "Le fichier n’est pas une sauvegarde Tutor NIP-49.")?;
        if backup.version != 1 || backup.format != "tutor-identity-nip49" {
            return Err("Version de sauvegarde non reconnue.".into());
        }
        let decrypt = |text: &str| -> Result<Keys, String> {
            let encrypted =
                EncryptedSecretKey::from_bech32(text).map_err(|_| "Sauvegarde NIP-49 invalide.")?;
            if !(16..=18).contains(&encrypted.log_n()) {
                return Err(
                    "Coût de chiffrement NIP-49 hors des limites acceptées (16 à 18).".into(),
                );
            }
            encrypted
                .decrypt_with_max_log_n(password, 18)
                .map(Keys::new)
                .map_err(|_| "Phrase de sauvegarde incorrecte ou sauvegarde altérée.".into())
        };
        let pair = Self {
            primary: decrypt(&backup.identity_ncryptsec)?,
            recovery: decrypt(&backup.recovery_ncryptsec)?,
        };
        if npub(&pair.primary) != backup.npub
            || npub(&pair.recovery) != backup.recovery_npub
            || pair.primary.public_key() == pair.recovery.public_key()
        {
            return Err(
                "Les clés de la sauvegarde ne correspondent pas aux identités annoncées.".into(),
            );
        }
        Ok(pair)
    }
}

pub fn npub(keys: &Keys) -> String {
    keys.public_key().to_bech32().expect("valid public key")
}

pub struct IdentityStore {
    directory: PathBuf,
    primary: Option<Keys>,
    recovery_public: Option<nostr::PublicKey>,
    pending_recovery: Option<Keys>,
    backup_confirmed: bool,
    exported: bool,
}
impl IdentityStore {
    pub fn load(directory: &Path) -> Result<Self, String> {
        let stored = crate::vault::load::<StoredIdentity>(directory, VAULT)?;
        let mut value = Self {
            directory: directory.into(),
            primary: None,
            recovery_public: None,
            pending_recovery: None,
            backup_confirmed: false,
            exported: false,
        };
        if let Some(stored) = stored {
            if stored.version != 1 {
                return Err(
                    "Version du coffre d’identité inconnue. Le fichier a été conservé.".into(),
                );
            }
            let primary = Keys::parse(&stored.secret)
                .map_err(|_| "La clé locale est invalide. Le coffre a été conservé.")?;
            let recovery = nostr::PublicKey::from_hex(&stored.recovery_public_key)
                .map_err(|_| "La clé publique de récupération est invalide.")?;
            if primary.public_key() == recovery {
                return Err(
                    "Les clés d’identité et de récupération doivent être distinctes.".into(),
                );
            }
            value.primary = Some(primary);
            value.recovery_public = Some(recovery);
            value.backup_confirmed = stored.backup_confirmed;
        }
        Ok(value)
    }
    pub fn public(&self) -> PublicIdentity {
        PublicIdentity {
            exists: self.primary.is_some(),
            npub: self.primary.as_ref().map(npub),
            recovery_npub: self.recovery_public.and_then(|key| key.to_bech32().ok()),
            backup_confirmed: self.backup_confirmed,
            backup_exported: self.exported,
            needs_backup: self.primary.is_some() && !self.backup_confirmed,
        }
    }
    fn persist(
        &self,
        primary: &Keys,
        recovery: nostr::PublicKey,
        confirmed: bool,
    ) -> Result<(), String> {
        let value = StoredIdentity {
            version: 1,
            secret: primary.secret_key().to_secret_hex(),
            recovery_public_key: recovery.to_hex(),
            backup_confirmed: confirmed,
        };
        crate::vault::save(&self.directory, VAULT, &value)
    }
    pub fn create(&mut self, nsec: Option<String>) -> Result<PublicIdentity, String> {
        let nsec = nsec.map(Zeroizing::new);
        if self.primary.is_some() {
            return Err("Une identité existe déjà sur cet appareil. Utilisez la rotation ou la récupération pour la remplacer.".into());
        }
        let primary = if let Some(text) = nsec {
            if !text.starts_with("nsec1") {
                return Err("Importez une clé privée Nostr au format nsec.".into());
            }
            Keys::new(SecretKey::from_bech32(&text).map_err(|_| "Clé nsec invalide.")?)
        } else {
            Keys::generate()
        };
        let recovery = Keys::generate();
        self.persist(&primary, recovery.public_key(), false)?;
        self.recovery_public = Some(recovery.public_key());
        self.pending_recovery = Some(recovery);
        self.primary = Some(primary);
        self.backup_confirmed = false;
        self.exported = false;
        Ok(self.public())
    }
    pub fn active(&self) -> Result<Keys, String> {
        if !self.backup_confirmed {
            return Err("Exportez et confirmez votre sauvegarde hors ligne avant de vous connecter à une institution.".into());
        }
        self.primary
            .clone()
            .ok_or("Créez ou restaurez votre identité locale.".into())
    }
    pub fn backup(&mut self, path: &Path, password: &str) -> Result<(), String> {
        let primary = self
            .primary
            .clone()
            .ok_or("Créez d’abord votre identité locale.")?;
        if self.backup_confirmed {
            return Err("La clé de récupération est désormais hors ligne. Conservez votre sauvegarde existante ; une nouvelle paire sera créée lors d’une rotation.".into());
        }
        // If restarted before the first backup, there is no registered identity
        // yet (active() is gated). Replace only the unregistered recovery key.
        if self.pending_recovery.is_none() {
            let recovery = Keys::generate();
            self.persist(&primary, recovery.public_key(), false)?;
            self.recovery_public = Some(recovery.public_key());
            self.pending_recovery = Some(recovery);
        }
        KeyPair {
            primary,
            recovery: self.pending_recovery.clone().unwrap(),
        }
        .backup(path, password)?;
        self.exported = true;
        Ok(())
    }
    pub fn confirm_backup(&mut self) -> Result<PublicIdentity, String> {
        if !self.exported {
            return Err("Exportez d’abord votre sauvegarde dans un fichier et conservez sa phrase séparément.".into());
        }
        self.persist(
            self.primary.as_ref().unwrap(),
            self.recovery_public.unwrap(),
            true,
        )?;
        self.backup_confirmed = true;
        self.pending_recovery = None;
        self.exported = false;
        Ok(self.public())
    }
    pub fn restore(&mut self, path: &Path, password: &str) -> Result<PublicIdentity, String> {
        let pair = KeyPair::restore(path, password)?;
        if self
            .primary
            .as_ref()
            .is_some_and(|keys| keys.public_key() != pair.primary.public_key())
        {
            return Err("Une autre identité est active. La restauration ne remplace pas son coffre. Utilisez la récupération de l’institution pour révoquer les anciens accès.".into());
        }
        self.install(&pair)?;
        Ok(self.public())
    }
    pub fn install(&mut self, pair: &KeyPair) -> Result<(), String> {
        self.persist(&pair.primary, pair.recovery.public_key(), true)?;
        self.primary = Some(pair.primary.clone());
        self.recovery_public = Some(pair.recovery.public_key());
        self.pending_recovery = None;
        self.backup_confirmed = true;
        self.exported = false;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn nip49_official_vector() {
        let text="ncryptsec1qgg9947rlpvqu76pj5ecreduf9jxhselq2nae2kghhvd5g7dgjtcxfqtd67p9m0w57lspw8gsq6yphnm8623nsl8xn9j4jdzz84zm3frztj3z7s35vpzmqf6ksu8r89qk5z2zxfmu5gv8th8wclt0h4p";
        let key = EncryptedSecretKey::from_bech32(text)
            .unwrap()
            .decrypt("nostr")
            .unwrap();
        assert_eq!(
            key.to_secret_hex(),
            "3501454135014541350145413501453fefb02227e449e57cf4d3a3ce05378683"
        );
    }
    #[test]
    fn backup_confirmation_restart_and_recovery_secret_stays_off_device() {
        let directory = tempfile::tempdir().unwrap();
        let backup = directory.path().join("offline.json");
        let mut store = IdentityStore::load(directory.path()).unwrap();
        let public = store.create(None).unwrap();
        assert_ne!(public.npub, public.recovery_npub);
        assert!(store.active().is_err());
        assert!(store.confirm_backup().is_err());
        store.backup(&backup, "une phrase vraiment privée").unwrap();
        assert!(store.public().backup_exported);
        assert!(!store.public().backup_confirmed);
        store.confirm_backup().unwrap();
        assert!(!store.public().backup_exported);
        let pair = KeyPair::restore(&backup, "une phrase vraiment privée").unwrap();
        assert_eq!(npub(&store.active().unwrap()), npub(&pair.primary));
        assert!(store.pending_recovery.is_none());
        let stored: serde_json::Value = crate::vault::load(directory.path(), VAULT)
            .unwrap()
            .unwrap();
        assert!(stored.get("recovery_secret").is_none());
        assert!(!stored
            .to_string()
            .contains(&pair.recovery.secret_key().to_secret_hex()));
        let restarted = IdentityStore::load(directory.path()).unwrap();
        assert!(restarted.active().is_ok());
        assert!(KeyPair::restore(&backup, "mauvaise phrase ici").is_err());
        assert!(pair.backup(&backup, "une phrase vraiment privée").is_err());
        let other = tempfile::tempdir().unwrap();
        let mut restored = IdentityStore::load(other.path()).unwrap();
        restored
            .restore(&backup, "une phrase vraiment privée")
            .unwrap();
        assert_eq!(restored.public().npub, public.npub);
    }
    #[test]
    fn an_existing_backup_confirms_the_same_identity_after_restart() {
        let directory = tempfile::tempdir().unwrap();
        let backup = directory.path().join("offline.json");
        let mut original = IdentityStore::load(directory.path()).unwrap();
        let public = original.create(None).unwrap();
        original
            .backup(&backup, "une phrase vraiment privée")
            .unwrap();
        drop(original);

        let mut restarted = IdentityStore::load(directory.path()).unwrap();
        assert!(!restarted.public().backup_confirmed);
        assert!(!restarted.public().backup_exported);
        assert!(restarted.restore(&backup, "une mauvaise phrase").is_err());
        assert!(restarted.active().is_err());
        restarted
            .restore(&backup, "une phrase vraiment privée")
            .unwrap();
        assert_eq!(restarted.public().npub, public.npub);
        assert_eq!(restarted.public().recovery_npub, public.recovery_npub);
        assert!(restarted.public().backup_confirmed);
        assert!(IdentityStore::load(directory.path())
            .unwrap()
            .active()
            .is_ok());
    }
    #[test]
    fn corrupt_import_or_different_backup_does_not_replace_active_identity() {
        let directory = tempfile::tempdir().unwrap();
        let mut store = IdentityStore::load(directory.path()).unwrap();
        let original = store.create(None).unwrap();
        let pair = KeyPair::generate();
        let backup = directory.path().join("other.json");
        pair.backup(&backup, "une longue phrase secrète").unwrap();
        assert!(store.restore(&backup, "une longue phrase secrète").is_err());
        assert_eq!(store.public().npub, original.npub);
        assert!(store.create(Some("nsec1invalid".into())).is_err());
        assert_eq!(
            IdentityStore::load(directory.path()).unwrap().public().npub,
            original.npub
        );
    }
}

#[cfg(test)]
mod additional_tests {
    use super::*;
    #[test]
    fn hostile_nip49_cost_is_rejected_without_allocating_unbounded_memory() {
        let cipher=EncryptedSecretKey::from_bech32("ncryptsec1qgg9947rlpvqu76pj5ecreduf9jxhselq2nae2kghhvd5g7dgjtcxfqtd67p9m0w57lspw8gsq6yphnm8623nsl8xn9j4jdzz84zm3frztj3z7s35vpzmqf6ksu8r89qk5z2zxfmu5gv8th8wclt0h4p").unwrap();
        let mut raw = cipher.as_vec();
        raw[1] = 30;
        let hostile = EncryptedSecretKey::from_slice(&raw)
            .unwrap()
            .to_bech32()
            .unwrap();
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("hostile.json");
        let value = PortableBackup {
            format: "tutor-identity-nip49".into(),
            version: 1,
            created_at: String::new(),
            npub: "unused".into(),
            recovery_npub: "unused".into(),
            identity_ncryptsec: hostile.clone(),
            recovery_ncryptsec: hostile,
        };
        std::fs::write(&path, serde_json::to_vec(&value).unwrap()).unwrap();
        assert!(KeyPair::restore(&path, "nostr")
            .err()
            .unwrap()
            .contains("Coût"));
    }
    #[test]
    fn failed_identity_write_keeps_the_existing_primary_and_confirmation() {
        let directory = tempfile::tempdir().unwrap();
        let mut store = IdentityStore::load(directory.path()).unwrap();
        let pair = KeyPair::generate();
        store.install(&pair).unwrap();
        let original = store.public();
        let path = directory.path().join(VAULT);
        let previous = directory.path().join("identity.previous");
        std::fs::rename(&path, &previous).unwrap();
        std::fs::create_dir(&path).unwrap();
        assert!(store.install(&KeyPair::generate()).is_err());
        assert_eq!(store.public().npub, original.npub);
        assert!(store.public().backup_confirmed);
        std::fs::remove_dir(&path).unwrap();
        std::fs::rename(previous, path).unwrap();
        assert_eq!(
            IdentityStore::load(directory.path()).unwrap().public().npub,
            original.npub
        );
    }
}
