// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Windows release credentials are encrypted for the current Windows user with
//! DPAPI. The Unix development build uses a private 0600 file, not an encrypted
//! desktop credential store. No credential object crosses the UI bridge.

use base64::{engine::general_purpose::STANDARD, Engine};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use std::io::Read;
use std::path::Path;
use zeroize::Zeroizing;

const MAX_VAULT_BYTES: u64 = 64 * 1024;

#[derive(Serialize, Deserialize)]
struct Envelope {
    version: u32,
    protection: String,
    payload: String,
}

pub fn save<T: Serialize>(directory: &Path, name: &str, value: &T) -> Result<(), String> {
    let plain = Zeroizing::new(
        serde_json::to_vec(value).map_err(|_| "Impossible de préparer le coffre Tutor.")?,
    );
    let protected = protect(&plain);
    let envelope = Envelope {
        version: 1,
        protection: protection_name().into(),
        payload: STANDARD.encode(protected?),
    };
    let bytes =
        serde_json::to_vec(&envelope).map_err(|_| "Impossible de préparer le coffre Tutor.")?;
    crate::workspace::private_write(&directory.join(name), &bytes)
}

pub fn load<T: DeserializeOwned>(directory: &Path, name: &str) -> Result<Option<T>, String> {
    let path = directory.join(name);
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
    let plain = Zeroizing::new(unprotect(&protected)?);
    let value = serde_json::from_slice::<T>(&plain).map_err(|_| {
        "Le coffre Tutor contient des données invalides. Son contenu a été conservé."
    })?;
    Ok(Some(value))
}

pub fn delete(directory: &Path, name: &str) -> Result<(), String> {
    match std::fs::remove_file(directory.join(name)) {
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
    let entropy_bytes = b"Tutor-Client/Native-Identity/v1";
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
    #[test]
    fn private_roundtrip_corruption_and_replacement() {
        let directory = tempfile::tempdir().unwrap();
        let name = "identity-test.json";
        save(directory.path(), name, &"secret-primary").unwrap();
        assert_eq!(
            load::<String>(directory.path(), name).unwrap().unwrap(),
            "secret-primary"
        );
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                std::fs::metadata(directory.path().join(name))
                    .unwrap()
                    .permissions()
                    .mode()
                    & 0o777,
                0o600
            );
        }
        #[cfg(windows)]
        {
            let env: Envelope =
                serde_json::from_slice(&std::fs::read(directory.path().join(name)).unwrap())
                    .unwrap();
            assert!(
                !String::from_utf8_lossy(&STANDARD.decode(env.payload).unwrap())
                    .contains("secret-primary")
            );
        }
        crate::workspace::private_write(&directory.path().join(name), b"corrupt").unwrap();
        assert!(load::<String>(directory.path(), name).is_err());
        assert_eq!(
            std::fs::read(directory.path().join(name)).unwrap(),
            b"corrupt"
        );
        delete(directory.path(), name).unwrap();
        assert!(load::<String>(directory.path(), name).unwrap().is_none());
    }
    #[cfg(unix)]
    #[test]
    fn rejects_world_readable_and_symlink() {
        use std::os::unix::fs::{symlink, PermissionsExt};
        let directory = tempfile::tempdir().unwrap();
        let name = "identity.json";
        save(directory.path(), name, &"secret").unwrap();
        let path = directory.path().join(name);
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        assert!(load::<String>(directory.path(), name).is_err());
        std::fs::rename(&path, directory.path().join("original")).unwrap();
        symlink(directory.path().join("original"), &path).unwrap();
        assert!(load::<String>(directory.path(), name).is_err());
    }
    #[cfg(windows)]
    #[test]
    fn dpapi_rejects_modified_ciphertext() {
        let mut cipher = protect(b"native-primary-key").unwrap();
        let index = cipher.len() / 2;
        cipher[index] ^= 1;
        assert!(unprotect(&cipher).is_err());
    }
}
