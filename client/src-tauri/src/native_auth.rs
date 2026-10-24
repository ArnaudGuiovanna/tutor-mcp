// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Institution authentication through a locally signed, single-use NIP-98
//! challenge. No private key leaves this module; only short-lived sessions are
//! delegated to the MCP transport. Every endpoint stays on the selected origin.
use base64::{engine::general_purpose::STANDARD, Engine};
use nostr::{EventBuilder, JsonUtil, Keys, Kind, Tag};
use reqwest::{Client, Url};
use serde::{de::DeserializeOwned, Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::time::Duration;

#[derive(Clone, Serialize, Deserialize)]
pub struct Institution {
    pub version: u32,
    pub institution_id: String,
    pub name: String,
    pub issuer: String,
    pub mcp_url: String,
    pub identity: IdentityEndpoints,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct IdentityEndpoints {
    pub scheme: String,
    pub challenge_url: String,
    pub session_url: String,
    pub rotate_url: String,
    pub recover_url: String,
    pub logout_url: String,
    pub event_kind: u16,
    pub challenge_ttl_seconds: u32,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Principal {
    pub user_id: String,
    pub tenant_id: String,
    pub membership_id: String,
    #[serde(default)]
    pub learner_id: String,
    #[serde(default)]
    pub roles: Vec<String>,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct RemoteIdentity {
    pub public_key: String,
    pub recovery_public_key: String,
    pub key_version: u64,
}
#[derive(Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "camelCase")]
pub enum NativeCredential {
    NativeIdentity(Session),
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Session {
    pub origin: String,
    pub institution: Institution,
    pub access_token: String,
    pub expires_at: i64,
    pub scope: String,
    pub session_id: String,
    pub identity: RemoteIdentity,
    pub principal: Principal,
    #[serde(default)]
    pub allowed_scopes: Vec<String>,
}
#[derive(Deserialize)]
struct SessionResponse {
    access_token: String,
    token_type: String,
    expires_in: i64,
    scope: String,
    session_id: String,
    identity: RemoteIdentity,
    principal: Principal,
    #[serde(default)]
    allowed_scopes: Vec<String>,
}
#[derive(Deserialize)]
struct Challenge {
    challenge: String,
    expires_at: i64,
    key_version: u64,
    url: String,
    method: String,
    request_body: String,
    payload_hash: String,
    tenant_id: String,
    public_key: String,
    action: String,
    scope: String,
}
#[derive(Deserialize)]
struct RotationResponse {
    rotated: bool,
    identity: RemoteIdentity,
    reauthentication_required: bool,
}

pub fn validate_origin(raw: &str) -> Result<String, String> {
    if raw.is_empty() || raw.len() > 2048 || raw.trim() != raw || raw.chars().any(char::is_control)
    {
        return Err("Adresse de Tutor invalide.".into());
    }
    let url = Url::parse(raw).map_err(|_| "Adresse de Tutor invalide.")?;
    let loopback = url.host_str().is_some_and(|host| {
        host == "localhost"
            || host
                .trim_matches(['[', ']'])
                .parse::<std::net::IpAddr>()
                .is_ok_and(|ip| ip.is_loopback())
    });
    if !(url.scheme() == "https" || (url.scheme() == "http" && loopback))
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !matches!(url.path(), "" | "/")
        || url.port() == Some(0)
    {
        return Err("Utilisez une origine HTTPS, ou HTTP pour un serveur local, sans chemin ni identifiants.".into());
    }
    Ok(url.origin().ascii_serialization())
}
fn endpoint(origin: &str, raw: &str, path: &str) -> Result<Url, String> {
    let url = Url::parse(raw).map_err(|_| "Adresse institutionnelle invalide.")?;
    if raw.len() > 4096
        || raw.trim() != raw
        || raw.chars().any(char::is_control)
        || url.origin().ascii_serialization() != origin
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
        || url.query().is_some()
        || url.path() != path
    {
        return Err("L’institution annonce une adresse inattendue ou hors de son origine.".into());
    }
    Ok(url)
}
fn client() -> Result<Client, String> {
    Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(5))
        .timeout(Duration::from_secs(20))
        .user_agent("Tutor-Client/0.1")
        .build()
        .map_err(|_| "Impossible de préparer la connexion à l’institution.".into())
}
async fn response<T: DeserializeOwned>(mut response: reqwest::Response) -> Result<T, String> {
    if !response.status().is_success() {
        let reason=match response.status().as_u16(){403=>"Clé non autorisée ou permissions insuffisantes : demandez l’accès à votre institution.",409=>"La version de la clé a changé. Reconnectez-vous avec l’identité actuelle.",_=>"L’institution a refusé la demande signée."};
        return Err(format!("{reason} (HTTP {}).", response.status().as_u16()));
    }
    let mut bytes = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|_| "Réponse de l’institution interrompue.")?
    {
        if bytes.len() + chunk.len() > 64 * 1024 {
            return Err("Réponse de l’institution trop volumineuse.".into());
        }
        bytes.extend_from_slice(&chunk);
    }
    serde_json::from_slice(&bytes).map_err(|_| "Réponse institutionnelle invalide.".into())
}
impl Institution {
    pub fn validate(&self, origin: &str) -> Result<(), String> {
        if self.version != 1
            || self.institution_id.is_empty()
            || self.institution_id.len() > 256
            || self.name.is_empty()
            || self.name.len() > 512
            || validate_origin(&self.issuer)? != origin
            || self.identity.scheme != "nostr-nip98"
            || self.identity.event_kind != 27235
            || self.identity.challenge_ttl_seconds != 120
        {
            return Err("Le serveur ne propose pas l’identité native Tutor attendue.".into());
        }
        for (url, path) in [
            (&self.mcp_url, "/mcp"),
            (
                &self.identity.challenge_url,
                "/api/client/v1/identity/challenge",
            ),
            (
                &self.identity.session_url,
                "/api/client/v1/identity/session",
            ),
            (&self.identity.rotate_url, "/api/client/v1/identity/rotate"),
            (
                &self.identity.recover_url,
                "/api/client/v1/identity/recover",
            ),
            (&self.identity.logout_url, "/api/client/v1/identity/logout"),
        ] {
            endpoint(origin, url, path)?;
        }
        Ok(())
    }
}
pub async fn discover(origin: &str) -> Result<Institution, String> {
    let origin = validate_origin(origin)?;
    let institution: Institution = response(
        client()?
            .get(format!("{origin}/.well-known/tutor-institution"))
            .send()
            .await
            .map_err(|_| "L’institution est inaccessible.")?,
    )
    .await?;
    institution.validate(&origin)?;
    Ok(institution)
}
impl Session {
    pub fn validate(&self) -> Result<(), String> {
        let origin = validate_origin(&self.origin)?;
        self.institution.validate(&origin)?;
        if self.access_token.is_empty()
            || self.access_token.len() > 16384
            || self.access_token.chars().any(char::is_control)
            || self.session_id.is_empty()
            || self.principal.tenant_id != self.institution.institution_id
            || self.principal.user_id.is_empty()
            || self.principal.membership_id.is_empty()
            || self.identity.key_version == 0
            || nostr::PublicKey::from_hex(&self.identity.public_key).is_err()
            || nostr::PublicKey::from_hex(&self.identity.recovery_public_key).is_err()
            || self.identity.public_key == self.identity.recovery_public_key
        {
            return Err("La session native contient une identité invalide.".into());
        }
        Ok(())
    }
    pub fn resource(&self) -> &str {
        &self.institution.mcp_url
    }
    pub async fn ensure_access(&mut self, keys: &Keys) -> Result<(), String> {
        self.validate()?;
        if self.identity.public_key != keys.public_key().to_hex() {
            return Err("La session appartient à une autre identité locale.".into());
        }
        if self.expires_at <= chrono::Utc::now().timestamp() + 60 {
            *self = open_session(&self.origin, keys, &self.scope).await?;
        }
        Ok(())
    }
    pub async fn revoke(&self, keys: &Keys) -> Result<(), String> {
        self.validate()?;
        let body = proof_request(
            &self.institution,
            keys,
            "logout",
            &self.scope,
            None,
            Some(&self.session_id),
        )
        .await?;
        let result: Value = submit(
            &self.institution.identity.logout_url,
            &body,
            keys,
            None,
            None,
        )
        .await?;
        if result.get("revoked") != Some(&json!(true))
            || result.get("session_id").and_then(Value::as_str) != Some(self.session_id.as_str())
        {
            return Err("La révocation de session n’a pas été confirmée.".into());
        }
        Ok(())
    }
}

async fn proof_request(
    institution: &Institution,
    keys: &Keys,
    action: &str,
    requested_scope: &str,
    new: Option<&crate::identity::KeyPair>,
    session_id: Option<&str>,
) -> Result<Challenge, String> {
    let origin = validate_origin(&institution.issuer)?;
    institution.validate(&origin)?;
    let mut request = json!({"tenant_id":institution.institution_id,"public_key":keys.public_key().to_hex(),"action":action,"scope":requested_scope});
    if let Some(pair) = new {
        request["new_public_key"] = json!(pair.primary.public_key().to_hex());
        request["new_recovery_public_key"] = json!(pair.recovery.public_key().to_hex());
    }
    if let Some(id) = session_id {
        request["session_id"] = json!(id);
    }
    let challenge: Challenge = response(
        client()?
            .post(&institution.identity.challenge_url)
            .json(&request)
            .send()
            .await
            .map_err(|_| "Impossible de demander le défi d’identité.")?,
    )
    .await?;
    let expected = match action {
        "session" => &institution.identity.session_url,
        "rotate" => &institution.identity.rotate_url,
        "recover" => &institution.identity.recover_url,
        "logout" => &institution.identity.logout_url,
        _ => return Err("Action d’identité inconnue.".into()),
    };
    validate_challenge(&challenge, &request, expected)?;
    Ok(challenge)
}
fn validate_challenge(challenge: &Challenge, request: &Value, url: &str) -> Result<(), String> {
    let now = chrono::Utc::now().timestamp();
    if challenge.url != url
        || challenge.method != "POST"
        || challenge.challenge.is_empty()
        || challenge.challenge.len() > 256
        || challenge.challenge.chars().any(char::is_control)
        || challenge.expires_at <= now
        || challenge.expires_at > now + 125
        || challenge.key_version == 0
        || challenge.request_body.len() > 16 * 1024
        || challenge.tenant_id != request["tenant_id"]
        || challenge.public_key != request["public_key"]
        || challenge.action != request["action"]
        || challenge.payload_hash
            != format!("{:x}", Sha256::digest(challenge.request_body.as_bytes()))
    {
        return Err("Le défi de l’institution ne correspond pas à la demande locale. Aucune signature n’a été envoyée.".into());
    }
    let requested = request["scope"].as_str().unwrap_or("");
    if (!requested.is_empty() && challenge.scope != requested)
        || (requested.is_empty()
            && !["learner:read learner:write", "formation:read"]
                .contains(&challenge.scope.as_str()))
    {
        return Err("L’institution demande des permissions non consenties. Aucune signature n’a été envoyée.".into());
    }
    let body: Value = serde_json::from_str(&challenge.request_body)
        .map_err(|_| "Le corps du défi est invalide.")?;
    for key in [
        "tenant_id",
        "public_key",
        "action",
        "new_public_key",
        "new_recovery_public_key",
        "session_id",
    ] {
        if request.get(key).is_some() && body.get(key) != request.get(key) {
            return Err("Le corps signé a modifié la demande d’identité.".into());
        }
    }
    if body["challenge"] != challenge.challenge
        || body["scope"] != challenge.scope
        || body["key_version"] != challenge.key_version
    {
        return Err("Le corps signé n’est pas lié au défi et à la version annoncés.".into());
    }
    Ok(())
}
fn signed_event(challenge: &Challenge, keys: &Keys) -> Result<String, String> {
    let values = vec![
        vec!["u".into(), challenge.url.clone()],
        vec!["method".into(), "POST".into()],
        vec!["payload".into(), challenge.payload_hash.clone()],
        vec!["challenge".into(), challenge.challenge.clone()],
        vec!["tenant".into(), challenge.tenant_id.clone()],
        vec!["action".into(), challenge.action.clone()],
        vec!["scope".into(), challenge.scope.clone()],
        vec!["version".into(), challenge.key_version.to_string()],
    ];
    let tags = values
        .into_iter()
        .map(Tag::parse)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| "Impossible de préparer la preuve d’identité.")?;
    let event = EventBuilder::new(Kind::Custom(27235), "")
        .tags(tags)
        .sign_with_keys(keys)
        .map_err(|_| "La signature locale a échoué.")?;
    Ok(STANDARD.encode(event.as_json()))
}
async fn submit<T: DeserializeOwned>(
    url: &str,
    challenge: &Challenge,
    keys: &Keys,
    new: Option<&crate::identity::KeyPair>,
    current_recovery: Option<&Keys>,
) -> Result<T, String> {
    if challenge.expires_at <= chrono::Utc::now().timestamp() {
        return Err("Le défi a expiré. Recommencez la demande.".into());
    }
    let mut request = client()?
        .post(url)
        .header("Content-Type", "application/json")
        .header(
            "Authorization",
            format!("Nostr {}", signed_event(challenge, keys)?),
        )
        .body(challenge.request_body.clone());
    if let Some(pair) = new {
        request = request
            .header(
                "X-Tutor-New-Key-Proof",
                signed_event(challenge, &pair.primary)?,
            )
            .header(
                "X-Tutor-New-Recovery-Proof",
                signed_event(challenge, &pair.recovery)?,
            );
    }
    if let Some(recovery) = current_recovery {
        request = request.header(
            "X-Tutor-Recovery-Approval",
            signed_event(challenge, recovery)?,
        );
    }
    response(request.send().await.map_err(|_| {
        "La demande signée n’a pas pu être confirmée. Gardez vos sauvegardes avant de réessayer."
    })?)
    .await
}
pub async fn open_session(origin: &str, keys: &Keys, scope: &str) -> Result<Session, String> {
    if ![
        "",
        "learner:read learner:write",
        "formation:read",
        "formation:read formation:write",
    ]
    .contains(&scope)
    {
        return Err("Permissions de session inconnues.".into());
    }
    let origin = validate_origin(origin)?;
    let institution = discover(&origin).await?;
    let challenge = proof_request(&institution, keys, "session", scope, None, None).await?;
    let result: SessionResponse = submit(
        &institution.identity.session_url,
        &challenge,
        keys,
        None,
        None,
    )
    .await?;
    if result.token_type != "Bearer"
        || result.expires_in <= 0
        || result.expires_in > 1800
        || result.scope != challenge.scope
        || result.identity.public_key != keys.public_key().to_hex()
        || result.identity.key_version != challenge.key_version
    {
        return Err("La session retournée ne correspond pas à la preuve locale.".into());
    }
    let session = Session {
        origin,
        institution,
        access_token: result.access_token,
        expires_at: chrono::Utc::now().timestamp() + result.expires_in,
        scope: result.scope,
        session_id: result.session_id,
        identity: result.identity,
        principal: result.principal,
        allowed_scopes: result.allowed_scopes,
    };
    session.validate()?;
    Ok(session)
}
pub async fn replace_keys(
    institution: &Institution,
    signer: &Keys,
    new: &crate::identity::KeyPair,
    recovery: bool,
    current_recovery: Option<&Keys>,
) -> Result<RemoteIdentity, String> {
    if !recovery && current_recovery.is_none() {
        return Err(
            "La rotation exige l’approbation de votre clé de récupération hors ligne.".into(),
        );
    }
    let action = if recovery { "recover" } else { "rotate" };
    let challenge = proof_request(institution, signer, action, "", Some(new), None).await?;
    let url = if recovery {
        &institution.identity.recover_url
    } else {
        &institution.identity.rotate_url
    };
    let result: RotationResponse =
        submit(url, &challenge, signer, Some(new), current_recovery).await?;
    if !result.rotated
        || !result.reauthentication_required
        || result.identity.public_key != new.primary.public_key().to_hex()
        || result.identity.recovery_public_key != new.recovery.public_key().to_hex()
        || result.identity.key_version != challenge.key_version + 1
    {
        return Err(
            "La nouvelle identité n’a pas été confirmée. Conservez les deux sauvegardes.".into(),
        );
    }
    Ok(result.identity)
}

#[cfg(test)]
mod tests {
    use super::*;
    use nostr::{Event, SecretKey, Timestamp};
    fn request() -> Value {
        json!({"tenant_id":"tenant_interop","public_key":keys().public_key().to_hex(),"action":"session","scope":"learner:read learner:write"})
    }
    fn keys() -> Keys {
        Keys::new(
            SecretKey::from_hex("0000000000000000000000000000000000000000000000000000000000000003")
                .unwrap(),
        )
    }
    fn challenge() -> Challenge {
        let request = request();
        let mut body = request.clone();
        body["challenge"] = json!("A".repeat(43));
        body["key_version"] = json!(1);
        let body = body.to_string();
        Challenge {
            challenge: "A".repeat(43),
            expires_at: chrono::Utc::now().timestamp() + 120,
            key_version: 1,
            url: "https://interop.example/api/client/v1/identity/session".into(),
            method: "POST".into(),
            payload_hash: format!("{:x}", Sha256::digest(body.as_bytes())),
            request_body: body,
            tenant_id: "tenant_interop".into(),
            public_key: keys().public_key().to_hex(),
            action: "session".into(),
            scope: "learner:read learner:write".into(),
        }
    }
    #[test]
    fn signs_exact_nip01_event_and_tags_verified_by_bip340() {
        let challenge = challenge();
        validate_challenge(&challenge, &request(), &challenge.url).unwrap();
        let encoded = signed_event(&challenge, &keys()).unwrap();
        let event = Event::from_json(STANDARD.decode(encoded).unwrap()).unwrap();
        event.verify().unwrap();
        assert_eq!(event.kind, Kind::Custom(27235));
        assert_eq!(event.pubkey, keys().public_key());
        assert!(event.content.is_empty());
        let tags: Vec<_> = event
            .tags
            .iter()
            .map(|tag| tag.as_slice().to_vec())
            .collect();
        assert_eq!(tags.len(), 8);
        assert!(tags.contains(&vec!["payload".into(), challenge.payload_hash.clone()]));
        assert!(tags.contains(&vec!["version".into(), "1".into()]));
    }
    #[test]
    fn rejects_rebound_body_endpoint_signer_scope_and_expiration_before_signing() {
        for change in 0..8 {
            let mut c = challenge();
            match change {
                0 => c.url = "https://attacker.example/session".into(),
                1 => c.public_key = "00".repeat(32),
                2 => c.scope = "formation:read formation:write".into(),
                3 => c.expires_at = 0,
                4 => c.request_body.push(' '),
                5 => c.key_version = 2,
                6 => c.action = "rotate".into(),
                _ => c.tenant_id = "another".into(),
            }
            assert!(validate_challenge(
                &c,
                &request(),
                "https://interop.example/api/client/v1/identity/session"
            )
            .is_err());
        }
        let mut c = challenge();
        c.scope = "formation:read formation:write".into();
        let mut req = request();
        req["scope"] = json!("");
        assert!(validate_challenge(&c, &req, &c.url).is_err());
        for origin in [
            "http://institution.example",
            "https://user:secret@institution.example",
            "https://institution.example/path",
            "https://institution.example?token=secret",
            "javascript:alert(1)",
        ] {
            assert!(validate_origin(origin).is_err());
        }
        assert!(endpoint(
            "https://institution.example",
            "https://attacker.example/mcp",
            "/mcp"
        )
        .is_err());
        assert!(endpoint(
            "https://institution.example",
            "https://institution.example/mcp?access_token=secret",
            "/mcp"
        )
        .is_err());
    }
    #[test]
    fn rust_go_interoperability_fixture() {
        let mut c = challenge();
        c.payload_hash = "0".repeat(64);
        let event =
            Event::from_json(STANDARD.decode(signed_event(&c, &keys()).unwrap()).unwrap()).unwrap();
        let event = EventBuilder::new(event.kind, event.content)
            .tags(event.tags)
            .custom_created_at(Timestamp::from(1700000000))
            .sign_with_keys(&keys())
            .unwrap();
        event.verify().unwrap();
        if let Some(path) = std::env::var_os("TUTOR_INTEROP_FIXTURE") {
            std::fs::write(path, event.as_json()).unwrap();
        }
    }
    fn institution(origin: &str) -> Institution {
        Institution {
            version: 1,
            institution_id: "tenant_interop".into(),
            name: "Institution de test".into(),
            issuer: origin.into(),
            mcp_url: format!("{origin}/mcp"),
            identity: IdentityEndpoints {
                scheme: "nostr-nip98".into(),
                challenge_url: format!("{origin}/api/client/v1/identity/challenge"),
                session_url: format!("{origin}/api/client/v1/identity/session"),
                rotate_url: format!("{origin}/api/client/v1/identity/rotate"),
                recover_url: format!("{origin}/api/client/v1/identity/recover"),
                logout_url: format!("{origin}/api/client/v1/identity/logout"),
                event_kind: 27235,
                challenge_ttl_seconds: 120,
            },
        }
    }
    #[tokio::test]
    async fn real_http_roundtrip_checks_exact_body_and_session_reauthentication_without_refresh() {
        use tokio::{
            io::{AsyncReadExt, AsyncWriteExt},
            net::TcpListener,
        };
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server_origin = origin.clone();
        let calls = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let captured = calls.clone();
        let server = tokio::spawn(async move {
            for _ in 0..6 {
                let (mut stream, _) = listener.accept().await.unwrap();
                let mut bytes = Vec::new();
                let mut block = [0; 2048];
                let (headers, body) = loop {
                    let size = stream.read(&mut block).await.unwrap();
                    assert!(size > 0);
                    bytes.extend_from_slice(&block[..size]);
                    if let Some(index) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                        let headers = String::from_utf8(bytes[..index].to_vec()).unwrap();
                        let length = headers
                            .lines()
                            .find_map(|line| {
                                line.to_ascii_lowercase()
                                    .strip_prefix("content-length: ")
                                    .and_then(|v| v.parse::<usize>().ok())
                            })
                            .unwrap_or(0);
                        if bytes.len() >= index + 4 + length {
                            break (headers, bytes[index + 4..index + 4 + length].to_vec());
                        }
                    }
                };
                let path = headers.split_whitespace().nth(1).unwrap();
                let institution = institution(&server_origin);
                let result = match path {
                    "/.well-known/tutor-institution" => serde_json::to_value(institution).unwrap(),
                    "/api/client/v1/identity/challenge" => {
                        let request: Value = serde_json::from_slice(&body).unwrap();
                        assert_eq!(request["public_key"], keys().public_key().to_hex());
                        assert!(request.get("private_key").is_none());
                        let scope = if request["scope"] == "" {
                            "learner:read learner:write"
                        } else {
                            request["scope"].as_str().unwrap()
                        };
                        let mut payload = request.clone();
                        payload["challenge"] = json!("A".repeat(43));
                        payload["scope"] = json!(scope);
                        payload["key_version"] = json!(1);
                        let body = payload.to_string();
                        json!({"challenge":"A".repeat(43),"expires_at":chrono::Utc::now().timestamp()+120,"key_version":1,"url":institution.identity.session_url,"method":"POST","request_body":body,"payload_hash":format!("{:x}",Sha256::digest(body.as_bytes())),"tenant_id":institution.institution_id,"public_key":request["public_key"],"action":"session","scope":scope})
                    }
                    "/api/client/v1/identity/session" => {
                        let auth = headers
                            .lines()
                            .find_map(|line| line.strip_prefix("authorization: Nostr "))
                            .unwrap();
                        let event = Event::from_json(STANDARD.decode(auth).unwrap()).unwrap();
                        event.verify().unwrap();
                        let hash = format!("{:x}", Sha256::digest(&body));
                        assert!(event
                            .tags
                            .iter()
                            .any(|tag| tag.as_slice() == ["payload", hash.as_str()]));
                        captured.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                        json!({"access_token":"short-lived-test-session","token_type":"Bearer","expires_in":1800,"scope":"learner:read learner:write","session_id":"test-session","identity":{"public_key":keys().public_key().to_hex(),"recovery_public_key":Keys::generate().public_key().to_hex(),"key_version":1},"principal":{"user_id":"user","tenant_id":"tenant_interop","membership_id":"member","learner_id":"learner","roles":["learner"]},"allowed_scopes":["learner:read","learner:write"]})
                    }
                    _ => panic!("unexpected HTTP path"),
                };
                let body = result.to_string();
                let response=format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len());
                stream.write_all(response.as_bytes()).await.unwrap();
            }
        });
        let mut session = open_session(&origin, &keys(), "").await.unwrap();
        assert_eq!(session.scope, "learner:read learner:write");
        session.expires_at = 0;
        session.ensure_access(&keys()).await.unwrap();
        assert_eq!(calls.load(std::sync::atomic::Ordering::SeqCst), 2);
        server.await.unwrap();
    }
    #[tokio::test]
    async fn rotation_proves_active_old_recovery_and_both_new_keys_on_the_same_payload() {
        use tokio::{
            io::{AsyncReadExt, AsyncWriteExt},
            net::TcpListener,
        };
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let institution = institution(&origin);
        let active = keys();
        let recovery = Keys::generate();
        let replacement = crate::identity::KeyPair::generate();
        assert!(
            replace_keys(&institution, &active, &replacement, false, None)
                .await
                .is_err()
        );
        let expected = [
            ("authorization", active.public_key()),
            ("x-tutor-recovery-approval", recovery.public_key()),
            ("x-tutor-new-key-proof", replacement.primary.public_key()),
            (
                "x-tutor-new-recovery-proof",
                replacement.recovery.public_key(),
            ),
        ];
        let next_public = replacement.primary.public_key().to_hex();
        let next_recovery = replacement.recovery.public_key().to_hex();
        let server_origin = origin.clone();
        let server = tokio::spawn(async move {
            for iteration in 0..2 {
                let (mut stream, _) = listener.accept().await.unwrap();
                let mut bytes = Vec::new();
                let mut block = [0; 4096];
                let (headers, body) = loop {
                    let count = stream.read(&mut block).await.unwrap();
                    assert!(count > 0);
                    bytes.extend_from_slice(&block[..count]);
                    if let Some(index) = bytes.windows(4).position(|part| part == b"\r\n\r\n") {
                        let headers = String::from_utf8(bytes[..index].to_vec()).unwrap();
                        let length = headers
                            .lines()
                            .find_map(|line| {
                                line.to_ascii_lowercase()
                                    .strip_prefix("content-length: ")
                                    .and_then(|value| value.parse::<usize>().ok())
                            })
                            .unwrap_or(0);
                        if bytes.len() >= index + 4 + length {
                            break (headers, bytes[index + 4..index + 4 + length].to_vec());
                        }
                    }
                };
                let result = if iteration == 0 {
                    assert!(headers.starts_with("POST /api/client/v1/identity/challenge "));
                    let mut body: Value = serde_json::from_slice(&body).unwrap();
                    assert_eq!(body["action"], "rotate");
                    body["scope"] = json!("formation:read");
                    body["challenge"] = json!("C".repeat(43));
                    body["key_version"] = json!(1);
                    let exact = body.to_string();
                    json!({"challenge":"C".repeat(43),"expires_at":chrono::Utc::now().timestamp()+120,"key_version":1,"url":format!("{server_origin}/api/client/v1/identity/rotate"),"method":"POST","request_body":exact,"payload_hash":format!("{:x}",Sha256::digest(exact.as_bytes())),"tenant_id":"tenant_interop","public_key":body["public_key"],"action":"rotate","scope":"formation:read"})
                } else {
                    assert!(headers.starts_with("POST /api/client/v1/identity/rotate "));
                    let hash = format!("{:x}", Sha256::digest(&body));
                    let mut prior_tags = None;
                    for (name, public) in expected {
                        let raw = headers
                            .lines()
                            .find_map(|line| {
                                line.split_once(':')
                                    .filter(|(key, _)| key.eq_ignore_ascii_case(name))
                                    .map(|(_, value)| value.trim())
                            })
                            .unwrap();
                        let event = Event::from_json(
                            STANDARD
                                .decode(raw.strip_prefix("Nostr ").unwrap_or(raw))
                                .unwrap(),
                        )
                        .unwrap();
                        event.verify().unwrap();
                        assert_eq!(event.pubkey, public);
                        assert_eq!(event.kind, Kind::Custom(27235));
                        assert!(event
                            .tags
                            .iter()
                            .any(|tag| tag.as_slice() == ["payload", hash.as_str()]));
                        let tags: Vec<Vec<String>> = event
                            .tags
                            .iter()
                            .map(|tag| tag.as_slice().to_vec())
                            .collect();
                        if let Some(previous) = &prior_tags {
                            assert_eq!(&tags, previous);
                        }
                        prior_tags = Some(tags);
                    }
                    json!({"rotated":true,"reauthentication_required":true,"identity":{"public_key":next_public,"recovery_public_key":next_recovery,"key_version":2}})
                };
                let body = result.to_string();
                let response=format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len());
                stream.write_all(response.as_bytes()).await.unwrap();
            }
        });
        let result = replace_keys(&institution, &active, &replacement, false, Some(&recovery))
            .await
            .unwrap();
        assert_eq!(result.key_version, 2);
        server.await.unwrap();
    }
}
