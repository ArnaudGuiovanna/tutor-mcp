// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
//! No secrets in command arguments or output. Passphrases/nsec are read from
//! stdin, one line per value. Pipe from a secret manager or a private terminal.
use std::{
    io::{self, BufRead},
    path::PathBuf,
};
use tutor_client::identity::IdentityStore;
use zeroize::Zeroizing;
fn run() -> Result<(), String> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.first().map(String::as_str) != Some("identity") {
        return Err("Usage : tutor-client-identity identity generate|exportpub|backup|import --directory DOSSIER [--file SAUVEGARDE] [--nsec]. Secrets via stdin, jamais en arguments.".into());
    }
    let action = args.get(1).ok_or("Sous-commande identité manquante.")?;
    let option = |name: &str| {
        args.iter()
            .position(|arg| arg == name)
            .and_then(|index| args.get(index + 1))
            .map(PathBuf::from)
    };
    let directory =
        option("--directory").ok_or("--directory est obligatoire (coffre local privé).")?;
    let mut store = IdentityStore::load(&directory)?;
    let mut input = io::stdin().lock();
    let mut secret = || -> Result<Zeroizing<String>, String> {
        let mut value = Zeroizing::new(String::new());
        input
            .read_line(&mut value)
            .map_err(|_| "Impossible de lire le secret sur stdin.")?;
        while value.ends_with(['\r', '\n']) {
            value.pop();
        }
        if value.is_empty() {
            return Err("Secret vide sur stdin.".into());
        }
        Ok(value)
    };
    match action.as_str() {
        "generate" => {
            store.create(None)?;
            if let Some(path) = option("--file") {
                let phrase = secret()?;
                store.backup(&path, &phrase)?;
                store.confirm_backup()?;
            }
        }
        "exportpub" => {
            store.active()?;
        }
        "backup" => {
            let path = option("--file").ok_or("--file est obligatoire.")?;
            let phrase = secret()?;
            store.backup(&path, &phrase)?;
            store.confirm_backup()?;
        }
        "import" => {
            if args.iter().any(|arg| arg == "--nsec") {
                let nsec = secret()?;
                store.create(Some(nsec.to_string()))?;
            } else {
                let path = option("--file")
                    .ok_or("--file est obligatoire pour restaurer la sauvegarde.")?;
                let phrase = secret()?;
                store.restore(&path, &phrase)?;
            }
        }
        _ => return Err("Sous-commande identité inconnue.".into()),
    }
    println!(
        "{}",
        serde_json::to_string(&store.public())
            .map_err(|_| "Impossible de présenter les clés publiques.")?
    );
    Ok(())
}
fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
