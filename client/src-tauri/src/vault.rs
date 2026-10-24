// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Windows release credentials are encrypted for the current Windows user with
//! DPAPI. The Unix development build uses a private 0600 file, not an encrypted
//! desktop credential store. No credential object crosses the UI bridge.

use crate::oauth::OAuthCredentials;
use base64::{engine::general_purpose::STANDARD, Engine};
use serde::{Deserialize, Serialize};
use std::io::Read;
use std::path::Path;

const FILENAME: &str = "oauth-credentials.json";
const MAX_VAULT_BYTES: u64 = 64 * 1024;

#[derive(Serialize, Deserialize)]
struct Envelope {
    version: u32,
    protection: String,
    payload: String,
}

pub fn save_credentials(directory: &Path, credentials: &OAuthCredentials) -> Result<(), String> {
    credentials.validate()?;
    let mut plain =
        serde_json::to_vec(credentials).map_err(|_| "Impossible de préparer les accès Tutor.")?;
    let protected = protect(&plain);
    plain.fill(0);
    let envelope = Envelope {
        version: 1,
        protection: protection_name().into(),
        payload: STANDARD.encode(protected?),
    };
    let bytes =
        serde_json::to_vec(&envelope).map_err(|_| "Impossible de préparer le coffre Tutor.")?;
    crate::workspace::private_write(&directory.join(FILENAME), &bytes)
}

pub fn load_credentials(directory: &Path) -> Result<Option<OAuthCredentials>, String> {
    let path = directory.join(FILENAME);
    let metadata = match std::fs::symlink_metadata(&path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(_) => return Err("Impossible de lire le coffre Tutor.".into()),
    };
    if !metadata.file_type().is_file() || metadata.len() > MAX_VAULT_BYTES {
        return Err("Le coffre Tutor est invalide. Son contenu a été conservé.".into());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            return Err("Les permissions du coffre Tutor sont trop ouvertes. Protégez le fichier avant de réessayer.".into());
        }
    }
    let file = std::fs::File::open(&path).map_err(|_| "Impossible de lire le coffre Tutor.")?;
    let mut bytes = Vec::new();
    file.take(MAX_VAULT_BYTES + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| "Impossible de lire le coffre Tutor.")?;
    if bytes.len() as u64 > MAX_VAULT_BYTES {
        return Err("Le coffre Tutor dépasse la taille autorisée.".into());
    }
    let envelope: Envelope = serde_json::from_slice(&bytes)
        .map_err(|_| "Le coffre Tutor est corrompu. Son contenu a été conservé.")?;
    if envelope.version != 1 || envelope.protection != protection_name() {
        return Err("Le format du coffre Tutor n’est pas compatible avec cet appareil.".into());
    }
    let protected = STANDARD
        .decode(envelope.payload)
        .map_err(|_| "Le coffre Tutor est corrompu.")?;
    let mut plain = unprotect(&protected)?;
    let decoded = serde_json::from_slice::<OAuthCredentials>(&plain);
    plain.fill(0);
    let credentials = decoded.map_err(|_| "Le coffre Tutor contient des accès invalides.")?;
    credentials.validate()?;
    Ok(Some(credentials))
}

pub fn delete_credentials(directory: &Path) -> Result<(), String> {
    match std::fs::remove_file(directory.join(FILENAME)) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(_) => Err("Impossible de retirer les accès Tutor de cet appareil.".into()),
    }
}

#[cfg(windows)]
fn protection_name() -> &'static str {
    "windows-dpapi-user-v1"
}
#[cfg(not(windows))]
fn protection_name() -> &'static str {
    "unix-development-0600-v1"
}

#[cfg(windows)]
fn crypt(input: &[u8], encrypt: bool) -> Result<Vec<u8>, String> {
    use std::ptr::{null, null_mut};
    use windows_sys::Win32::{
        Foundation::LocalFree,
        Security::Cryptography::{
            CryptProtectData, CryptUnprotectData, CRYPTPROTECT_UI_FORBIDDEN, CRYPT_INTEGER_BLOB,
        },
    };
    let length: u32 = input
        .len()
        .try_into()
        .map_err(|_| "Les accès dépassent la taille du coffre.")?;
    let data = CRYPT_INTEGER_BLOB {
        cbData: length,
        pbData: input.as_ptr() as *mut u8,
    };
    let entropy_bytes = b"Tutor-Client/OAuth/v1";
    let entropy = CRYPT_INTEGER_BLOB {
        cbData: entropy_bytes.len() as u32,
        pbData: entropy_bytes.as_ptr() as *mut u8,
    };
    let mut output = CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: null_mut(),
    };
    // DPAPI owns the allocated output. The current-user mode intentionally
    // omits CRYPTPROTECT_LOCAL_MACHINE; another Windows account cannot decrypt.
    let success = unsafe {
        if encrypt {
            CryptProtectData(
                &data,
                null(),
                &entropy,
                null(),
                null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        } else {
            CryptUnprotectData(
                &data,
                null_mut(),
                &entropy,
                null(),
                null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        }
    };
    if success == 0 {
        return Err(
            "Windows n’a pas pu ouvrir ou protéger le coffre Tutor pour cet utilisateur.".into(),
        );
    }
    if output.pbData.is_null() {
        return Err("Windows a retourné un coffre Tutor invalide.".into());
    }
    // SAFETY: a successful DPAPI call returns cbData initialized bytes in the
    // LocalAlloc buffer; copy before freeing with the matching Windows API.
    let result =
        unsafe { std::slice::from_raw_parts(output.pbData, output.cbData as usize).to_vec() };
    unsafe {
        if !encrypt {
            std::slice::from_raw_parts_mut(output.pbData, output.cbData as usize).fill(0);
        }
        LocalFree(output.pbData.cast());
    }
    Ok(result)
}

#[cfg(windows)]
fn protect(plain: &[u8]) -> Result<Vec<u8>, String> {
    crypt(plain, true)
}
#[cfg(windows)]
fn unprotect(bytes: &[u8]) -> Result<Vec<u8>, String> {
    crypt(bytes, false)
}
#[cfg(not(windows))]
fn protect(plain: &[u8]) -> Result<Vec<u8>, String> {
    Ok(plain.to_vec())
}
#[cfg(not(windows))]
fn unprotect(bytes: &[u8]) -> Result<Vec<u8>, String> {
    Ok(bytes.to_vec())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn credentials() -> OAuthCredentials {
        OAuthCredentials {
            origin: "https://institution.example".into(),
            client_id: "public-client".into(),
            access_token: "test-access-secret".into(),
            refresh_token: "test-refresh-secret".into(),
            expires_at: 2_000_000_000,
            token_endpoint: "https://institution.example/token".into(),
            resource: "https://institution.example/mcp".into(),
            scope: "learner:read learner:write".into(),
        }
    }

    #[test]
    fn credentials_survive_restart_rotation_and_explicit_deletion() {
        let directory = tempfile::tempdir().unwrap();
        assert!(load_credentials(directory.path()).unwrap().is_none());
        let mut original = credentials();
        save_credentials(directory.path(), &original).unwrap();
        let restored = load_credentials(directory.path()).unwrap().unwrap();
        assert_eq!(restored.refresh_token, original.refresh_token);
        assert_eq!(restored.origin, original.origin);
        original.refresh_token = "rotated-secret".into();
        save_credentials(directory.path(), &original).unwrap();
        assert_eq!(
            load_credentials(directory.path())
                .unwrap()
                .unwrap()
                .refresh_token,
            "rotated-secret"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                std::fs::metadata(directory.path().join(FILENAME))
                    .unwrap()
                    .permissions()
                    .mode()
                    & 0o777,
                0o600
            );
        }
        #[cfg(windows)]
        {
            let bytes = std::fs::read(directory.path().join(FILENAME)).unwrap();
            assert!(!String::from_utf8_lossy(&bytes).contains("rotated-secret"));
            let envelope: Envelope = serde_json::from_slice(&bytes).unwrap();
            let cipher = STANDARD.decode(envelope.payload).unwrap();
            assert!(!String::from_utf8_lossy(&cipher).contains("rotated-secret"));
            assert!(!String::from_utf8_lossy(&cipher).contains("test-access-secret"));
        }
        delete_credentials(directory.path()).unwrap();
        delete_credentials(directory.path()).unwrap();
        assert!(load_credentials(directory.path()).unwrap().is_none());
    }

    #[test]
    fn corrupted_or_foreign_format_is_preserved_instead_of_overwritten() {
        let directory = tempfile::tempdir().unwrap();
        for bytes in [
            b"broken-json".to_vec(),
            serde_json::to_vec(&Envelope {
                version: 99,
                protection: protection_name().into(),
                payload: "invalid".into(),
            })
            .unwrap(),
        ] {
            crate::workspace::private_write(&directory.path().join(FILENAME), &bytes).unwrap();
            assert!(load_credentials(directory.path()).is_err());
            assert_eq!(
                std::fs::read(directory.path().join(FILENAME)).unwrap(),
                bytes
            );
        }
    }

    #[test]
    fn invalid_replacement_cannot_destroy_an_existing_connection() {
        let directory = tempfile::tempdir().unwrap();
        let mut value = credentials();
        save_credentials(directory.path(), &value).unwrap();
        value.token_endpoint = "https://attacker.example/token".into();
        assert!(save_credentials(directory.path(), &value).is_err());
        assert_eq!(
            load_credentials(directory.path())
                .unwrap()
                .unwrap()
                .token_endpoint,
            "https://institution.example/token"
        );
    }

    #[cfg(unix)]
    #[test]
    fn unix_development_vault_refuses_world_readable_files_and_symbolic_links() {
        use std::os::unix::fs::{symlink, PermissionsExt};
        let directory = tempfile::tempdir().unwrap();
        save_credentials(directory.path(), &credentials()).unwrap();
        let path = directory.path().join(FILENAME);
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(load_credentials(directory.path()).is_err());
        std::fs::rename(&path, directory.path().join("original")).unwrap();
        symlink(directory.path().join("original"), &path).unwrap();
        assert!(load_credentials(directory.path()).is_err());
    }

    #[cfg(windows)]
    #[test]
    fn windows_dpapi_rejects_modified_ciphertext() {
        let mut protected = protect(b"an-oauth-secret-that-must-be-authenticated").unwrap();
        let position = protected.len() / 2;
        protected[position] ^= 0x20;
        assert!(unprotect(&protected).is_err());
    }
}
