// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! Native public-client OAuth: browser authorization, PKCE S256 and a bounded
//! loopback callback. This module never opens a browser, logs an URL, or returns
//! provider response bodies to the frontend.

use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use rand::RngCore;
use reqwest::{Client, Response, Url};
use serde::{de::DeserializeOwned, Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    time::{timeout, Instant},
};

const BODY_LIMIT: usize = 8 * 1024;
const CALLBACK_PATH: &str = "/oauth/callback";
const LOGIN_TIMEOUT: Duration = Duration::from_secs(300);
const STATIC_CLIENT_PORT: u16 = 47823;

#[derive(Clone, Serialize, Deserialize)]
pub struct OAuthCredentials {
    pub origin: String,
    pub client_id: String,
    pub access_token: String,
    pub refresh_token: String,
    pub expires_at: i64,
    pub token_endpoint: String,
    pub resource: String,
    pub scope: String,
}

#[derive(Deserialize)]
struct Metadata {
    issuer: String,
    authorization_endpoint: String,
    token_endpoint: String,
    #[serde(default)]
    registration_endpoint: Option<String>,
    #[serde(default)]
    revocation_endpoint: Option<String>,
    #[serde(default)]
    scopes_supported: Vec<String>,
    #[serde(default)]
    code_challenge_methods_supported: Vec<String>,
    #[serde(default)]
    token_endpoint_auth_methods_supported: Vec<String>,
    #[serde(default)]
    authorization_response_iss_parameter_supported: bool,
}

#[derive(Deserialize)]
struct Registration {
    client_id: String,
    #[serde(default)]
    client_secret: String,
    #[serde(default)]
    token_endpoint_auth_method: String,
    #[serde(default)]
    redirect_uris: Vec<String>,
}

#[derive(Deserialize)]
struct Tokens {
    access_token: String,
    token_type: String,
    expires_in: i64,
    #[serde(default)]
    refresh_token: String,
    #[serde(default)]
    scope: Option<String>,
}

pub struct PendingLogin {
    listener: TcpListener,
    client: Client,
    origin: String,
    client_id: String,
    token_endpoint: String,
    redirect_uri: String,
    state: String,
    verifier: String,
    scope: String,
    deadline: Instant,
}

/// Tutor's native connection selects an institution by origin, not by an
/// arbitrary discovery URL. Remote credentials never leave that origin.
pub fn normalize_origin(raw: &str) -> Result<String, String> {
    if raw.is_empty() || raw.len() > 2048 || raw.trim() != raw || raw.chars().any(char::is_control)
    {
        return Err("Adresse de Tutor invalide.".into());
    }
    let url = Url::parse(raw).map_err(|_| "Adresse de Tutor invalide.")?;
    let loopback = url.host_str().is_some_and(|host| {
        host == "localhost"
            || host
                .trim_start_matches('[')
                .trim_end_matches(']')
                .parse::<std::net::IpAddr>()
                .is_ok_and(|address| address.is_loopback())
    });
    if !((url.scheme() == "https") || (url.scheme() == "http" && loopback))
        || url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || !matches!(url.path(), "" | "/")
        || url.port() == Some(0)
    {
        return Err("Utilisez une origine HTTPS, ou HTTP uniquement pour un serveur local, sans chemin ni identifiants.".into());
    }
    Ok(url.origin().ascii_serialization())
}

pub fn validate_origin(raw: &str) -> Result<String, String> {
    normalize_origin(raw)
}

fn endpoint(origin: &str, raw: &str) -> Result<Url, String> {
    let url = Url::parse(raw).map_err(|_| "Point de connexion OAuth invalide.")?;
    if url.origin().ascii_serialization() != origin
        || !url.username().is_empty()
        || url.password().is_some()
        || url.fragment().is_some()
        || raw.chars().any(char::is_control)
        || raw.trim() != raw
    {
        return Err("Le serveur annonce un point OAuth hors de son origine.".into());
    }
    Ok(url)
}

fn client() -> Result<Client, String> {
    Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .connect_timeout(Duration::from_secs(10))
        .timeout(Duration::from_secs(20))
        .user_agent("Tutor-Client/0.1")
        .build()
        .map_err(|_| "Impossible de préparer la connexion OAuth.")
        .map_err(String::from)
}

async fn json_response<T: DeserializeOwned>(mut response: Response) -> Result<T, String> {
    if !response.status().is_success() {
        return Err(format!(
            "Le serveur a refusé la connexion OAuth (HTTP {}).",
            response.status().as_u16()
        ));
    }
    if response
        .content_length()
        .is_some_and(|size| size > BODY_LIMIT as u64)
    {
        return Err("La réponse OAuth dépasse la taille autorisée.".into());
    }
    let mut bytes = Vec::new();
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|_| "Lecture de la réponse OAuth impossible.")?
    {
        if bytes.len().saturating_add(chunk.len()) > BODY_LIMIT {
            return Err("La réponse OAuth dépasse la taille autorisée.".into());
        }
        bytes.extend_from_slice(&chunk);
    }
    serde_json::from_slice(&bytes)
        .map_err(|_| "Le serveur a retourné une réponse OAuth invalide.".into())
}

async fn metadata(client: &Client, origin: &str) -> Result<Metadata, String> {
    let response = client
        .get(format!("{origin}/.well-known/oauth-authorization-server"))
        .send()
        .await
        .map_err(|_| "Impossible de découvrir les paramètres OAuth de Tutor.")?;
    let metadata: Metadata = json_response(response).await?;
    if metadata.issuer != origin {
        return Err("L’émetteur OAuth ne correspond pas à l’institution choisie.".into());
    }
    endpoint(origin, &metadata.authorization_endpoint)?;
    endpoint(origin, &metadata.token_endpoint)?;
    for optional in [
        &metadata.registration_endpoint,
        &metadata.revocation_endpoint,
    ]
    .into_iter()
    .flatten()
    {
        endpoint(origin, optional)?;
    }
    Ok(metadata)
}

fn random_secret() -> String {
    let mut bytes = [0_u8; 32];
    rand::rng().fill_bytes(&mut bytes);
    URL_SAFE_NO_PAD.encode(bytes)
}

fn challenge(verifier: &str) -> String {
    URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()))
}

fn selected_scope(metadata: &Metadata) -> Result<String, String> {
    let supports = |scope: &str| metadata.scopes_supported.iter().any(|value| value == scope);
    if supports("learner:read") && supports("learner:write") {
        Ok("learner:read learner:write".into())
    } else if supports("learner") {
        Ok("learner".into())
    } else {
        Err("Le serveur n’annonce pas les autorisations nécessaires à l’apprentissage.".into())
    }
}

pub async fn begin_login(
    origin: &str,
    client_id: Option<&str>,
    initial_access_token: Option<&str>,
) -> Result<(String, PendingLogin), String> {
    let origin = normalize_origin(origin)?;
    let client = client()?;
    let metadata = metadata(&client, &origin).await?;
    if !metadata
        .code_challenge_methods_supported
        .iter()
        .any(|value| value == "S256")
        || !metadata
            .token_endpoint_auth_methods_supported
            .iter()
            .any(|value| value == "none")
        || !metadata.authorization_response_iss_parameter_supported
    {
        return Err("Le serveur ne prend pas en charge le parcours natif PKCE avec validation de l’émetteur.".into());
    }
    let scope = selected_scope(&metadata)?;
    let supplied_client = client_id.filter(|value| !value.is_empty());
    if supplied_client
        .is_some_and(|value| value.len() > 2048 || value.chars().any(char::is_control))
    {
        return Err("Identifiant de client OAuth invalide.".into());
    }
    let port = if supplied_client.is_some() {
        STATIC_CLIENT_PORT
    } else {
        0
    };
    let listener = TcpListener::bind(("127.0.0.1", port)).await.map_err(|_| {
        if supplied_client.is_some() {
            "Le port de retour OAuth 47823 est occupé. Fermez la connexion en cours puis réessayez."
        } else {
            "Impossible d’ouvrir le retour OAuth local."
        }
    })?;
    let port = listener
        .local_addr()
        .map_err(|_| "Impossible de lire le port OAuth local.")?
        .port();
    let redirect_uri = format!("http://127.0.0.1:{port}{CALLBACK_PATH}");
    let client_id = match supplied_client {
        Some(value) => value.to_owned(),
        None => {
            let registration_endpoint = metadata.registration_endpoint.as_deref().ok_or("Cette institution exige un identifiant de client OAuth préenregistré. Renseignez-le dans la configuration avancée.")?;
            let mut request =
                client
                    .post(endpoint(&origin, registration_endpoint)?)
                    .json(&serde_json::json!({
                        "client_name": "Tutor — application personnelle",
                        "application_type": "native",
                        "redirect_uris": [&redirect_uri],
                        "grant_types": ["authorization_code", "refresh_token"],
                        "response_types": ["code"],
                        "token_endpoint_auth_method": "none"
                    }));
            if let Some(token) = initial_access_token.filter(|value| !value.is_empty()) {
                if token.len() > 4096 || !token.bytes().all(|byte| byte.is_ascii_graphic()) {
                    return Err("Jeton d’enregistrement invalide.".into());
                }
                request = request.bearer_auth(token);
            }
            let response = request
                .send()
                .await
                .map_err(|_| "Impossible d’enregistrer le client auprès de Tutor.")?;
            let registration: Registration = json_response(response).await?;
            if registration.client_id.is_empty()
                || registration.client_id.len() > 2048
                || registration.client_id.chars().any(char::is_control)
                || !registration.client_secret.is_empty()
                || (!registration.token_endpoint_auth_method.is_empty()
                    && registration.token_endpoint_auth_method != "none")
                || registration.redirect_uris != [redirect_uri.clone()]
            {
                return Err("Le serveur a retourné une inscription OAuth incompatible avec ce client public.".into());
            }
            registration.client_id
        }
    };
    let verifier = random_secret();
    let state = random_secret();
    let mut authorization = endpoint(&origin, &metadata.authorization_endpoint)?;
    let reserved = [
        "response_type",
        "client_id",
        "redirect_uri",
        "scope",
        "state",
        "code_challenge",
        "code_challenge_method",
        "resource",
    ];
    if authorization
        .query_pairs()
        .any(|(name, _)| reserved.contains(&name.as_ref()))
    {
        return Err("Le point d’autorisation contient des paramètres OAuth ambigus.".into());
    }
    authorization
        .query_pairs_mut()
        .append_pair("response_type", "code")
        .append_pair("client_id", &client_id)
        .append_pair("redirect_uri", &redirect_uri)
        .append_pair("scope", &scope)
        .append_pair("state", &state)
        .append_pair("code_challenge", &challenge(&verifier))
        .append_pair("code_challenge_method", "S256")
        .append_pair("resource", &format!("{origin}/mcp"));
    Ok((
        authorization.into(),
        PendingLogin {
            listener,
            client,
            origin,
            client_id,
            token_endpoint: metadata.token_endpoint,
            redirect_uri,
            state,
            verifier,
            scope,
            deadline: Instant::now() + LOGIN_TIMEOUT,
        },
    ))
}

fn same_secret(expected: &str, actual: &str) -> bool {
    if expected.len() != actual.len() {
        return false;
    }
    expected
        .bytes()
        .zip(actual.bytes())
        .fold(0_u8, |different, (left, right)| different | (left ^ right))
        == 0
}

enum Callback {
    Code(String),
    Denied,
    Ignore,
}

impl PendingLogin {
    pub async fn finish(self) -> Result<OAuthCredentials, String> {
        let remaining = self.deadline.saturating_duration_since(Instant::now());
        timeout(remaining, self.wait_and_exchange())
            .await
            .map_err(|_| {
                "La connexion Tutor a expiré. Relancez-la depuis l’application.".to_string()
            })?
    }

    async fn wait_and_exchange(&self) -> Result<OAuthCredentials, String> {
        loop {
            let (mut stream, peer) = self
                .listener
                .accept()
                .await
                .map_err(|_| "Le retour OAuth local a été interrompu.")?;
            if !peer.ip().is_loopback() {
                continue;
            }
            let callback = timeout(Duration::from_secs(3), self.callback(&mut stream)).await;
            match callback {
                Ok(Ok(Callback::Code(code))) => return self.exchange(&code).await,
                Ok(Ok(Callback::Denied)) => {
                    return Err(
                        "La connexion Tutor a été refusée ou annulée dans le navigateur.".into(),
                    )
                }
                _ => continue,
            }
        }
    }

    async fn callback(&self, stream: &mut TcpStream) -> Result<Callback, String> {
        let mut bytes = Vec::new();
        let mut chunk = [0_u8; 1024];
        loop {
            let read = stream
                .read(&mut chunk)
                .await
                .map_err(|_| "Retour OAuth illisible.")?;
            if read == 0 {
                return Ok(Callback::Ignore);
            }
            bytes.extend_from_slice(&chunk[..read]);
            if bytes.len() > BODY_LIMIT {
                respond(stream, 431, "Requête trop longue.").await;
                return Ok(Callback::Ignore);
            }
            if bytes.windows(4).any(|bytes| bytes == b"\r\n\r\n") {
                break;
            }
        }
        let request = match std::str::from_utf8(&bytes) {
            Ok(value) => value,
            Err(_) => {
                respond(stream, 400, "Requête invalide.").await;
                return Ok(Callback::Ignore);
            }
        };
        let mut lines = request.split("\r\n");
        let line: Vec<_> = lines.next().unwrap_or("").split(' ').collect();
        if line.len() != 3
            || line[0] != "GET"
            || !matches!(line[2], "HTTP/1.0" | "HTTP/1.1")
            || !line[1].starts_with('/')
            || line[1].starts_with("//")
        {
            respond(stream, 400, "Retour OAuth invalide.").await;
            return Ok(Callback::Ignore);
        }
        let redirect = Url::parse(&self.redirect_uri).map_err(|_| "Retour OAuth invalide.")?;
        let expected_host = format!("127.0.0.1:{}", redirect.port().unwrap_or(80));
        let mut hosts = Vec::new();
        for header in lines.take_while(|line| !line.is_empty()) {
            let Some((name, value)) = header.split_once(':') else {
                respond(stream, 400, "En-tête invalide.").await;
                return Ok(Callback::Ignore);
            };
            if name.eq_ignore_ascii_case("host") {
                hosts.push(value.trim());
            }
            if name.eq_ignore_ascii_case("transfer-encoding")
                || (name.eq_ignore_ascii_case("content-length") && value.trim() != "0")
            {
                respond(stream, 400, "Corps de requête refusé.").await;
                return Ok(Callback::Ignore);
            }
        }
        if hosts != [expected_host.as_str()] {
            respond(stream, 400, "Hôte de retour invalide.").await;
            return Ok(Callback::Ignore);
        }
        let callback = match Url::parse(&format!(
            "{}{}",
            redirect.origin().ascii_serialization(),
            line[1]
        )) {
            Ok(url) if url.path() == CALLBACK_PATH && url.fragment().is_none() => url,
            _ => {
                respond(stream, 404, "Page introuvable.").await;
                return Ok(Callback::Ignore);
            }
        };
        let parameters: Vec<_> = callback.query_pairs().collect();
        if ["state", "iss", "code", "error"].iter().any(|name| {
            parameters
                .iter()
                .filter(|(key, _)| key.as_ref() == *name)
                .count()
                > 1
        }) {
            respond(stream, 400, "Paramètres de retour ambigus.").await;
            return Ok(Callback::Ignore);
        }
        let unique = |name: &str| {
            let values: Vec<_> = parameters
                .iter()
                .filter(|(key, _)| key == name)
                .map(|(_, value)| value.as_ref())
                .collect();
            if values.len() == 1 {
                Some(values[0])
            } else {
                None
            }
        };
        if !unique("state").is_some_and(|state| same_secret(&self.state, state))
            || unique("iss") != Some(self.origin.as_str())
        {
            respond(
                stream,
                400,
                "Ce retour ne correspond pas à la connexion en cours.",
            )
            .await;
            return Ok(Callback::Ignore);
        }
        let has_error = parameters.iter().any(|(name, _)| name == "error");
        let code = unique("code");
        if has_error && code.is_none() && unique("error").is_some() {
            respond(
                stream,
                200,
                "Connexion annulée. Vous pouvez revenir dans Tutor.",
            )
            .await;
            return Ok(Callback::Denied);
        }
        let Some(code) = code.filter(|code| {
            !code.is_empty() && code.len() <= 2048 && !code.chars().any(char::is_control)
        }) else {
            respond(stream, 400, "Code de retour invalide.").await;
            return Ok(Callback::Ignore);
        };
        if has_error {
            respond(stream, 400, "Retour OAuth ambigu.").await;
            return Ok(Callback::Ignore);
        }
        respond(
            stream,
            200,
            "Autorisation reçue. Revenez dans Tutor pour vérifier la connexion.",
        )
        .await;
        Ok(Callback::Code(code.to_owned()))
    }

    async fn exchange(&self, code: &str) -> Result<OAuthCredentials, String> {
        let resource = format!("{}/mcp", self.origin);
        let response = self
            .client
            .post(endpoint(&self.origin, &self.token_endpoint)?)
            .form(&[
                ("grant_type", "authorization_code"),
                ("client_id", self.client_id.as_str()),
                ("code", code),
                ("code_verifier", self.verifier.as_str()),
                ("redirect_uri", self.redirect_uri.as_str()),
                ("resource", resource.as_str()),
            ])
            .send()
            .await
            .map_err(|_| "Impossible de terminer la connexion Tutor.")?;
        let tokens: Tokens = json_response(response).await?;
        let mut credentials = OAuthCredentials {
            origin: self.origin.clone(),
            client_id: self.client_id.clone(),
            access_token: String::new(),
            refresh_token: String::new(),
            expires_at: 0,
            token_endpoint: self.token_endpoint.clone(),
            resource,
            scope: self.scope.clone(),
        };
        credentials.apply_tokens(tokens, true)?;
        Ok(credentials)
    }
}

async fn respond(stream: &mut TcpStream, status: u16, message: &str) {
    let reason = match status {
        200 => "OK",
        404 => "Not Found",
        431 => "Request Header Fields Too Large",
        _ => "Bad Request",
    };
    let body = format!("<!doctype html><html lang=\"fr\"><meta charset=\"utf-8\"><title>Tutor</title><p>{message}</p></html>");
    let response = format!("HTTP/1.1 {status} {reason}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nCache-Control: no-store\r\nReferrer-Policy: no-referrer\r\nContent-Security-Policy: default-src 'none'; frame-ancestors 'none'\r\nConnection: close\r\n\r\n{body}", body.len());
    let _ = stream.write_all(response.as_bytes()).await;
    let _ = stream.shutdown().await;
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_secs().min(i64::MAX as u64) as i64)
        .unwrap_or(0)
}

fn valid_token(token: &str) -> bool {
    !token.is_empty()
        && token.len() <= BODY_LIMIT
        && token.bytes().all(|byte| byte.is_ascii_graphic())
}

impl OAuthCredentials {
    pub fn validate(&self) -> Result<(), String> {
        if normalize_origin(&self.origin)? != self.origin
            || self.resource != format!("{}/mcp", self.origin)
            || self.client_id.is_empty()
            || self.client_id.len() > 2048
            || self.client_id.chars().any(char::is_control)
            || !valid_token(&self.access_token)
            || (!self.refresh_token.is_empty() && !valid_token(&self.refresh_token))
            || self.scope.is_empty()
            || self.scope.len() > 1024
            || self.scope.chars().any(char::is_control)
        {
            return Err(
                "Les accès Tutor enregistrés sont invalides. Reconnectez-vous à votre institution."
                    .into(),
            );
        }
        endpoint(&self.origin, &self.token_endpoint)?;
        Ok(())
    }

    fn apply_tokens(&mut self, tokens: Tokens, initial: bool) -> Result<(), String> {
        if !tokens.token_type.eq_ignore_ascii_case("bearer")
            || !valid_token(&tokens.access_token)
            || tokens.expires_in <= 0
            || tokens.expires_in > 366 * 24 * 60 * 60
            || (initial && !valid_token(&tokens.refresh_token))
            || (!tokens.refresh_token.is_empty() && !valid_token(&tokens.refresh_token))
        {
            return Err("Le serveur a retourné des accès OAuth invalides.".into());
        }
        let scope = tokens.scope.unwrap_or_else(|| self.scope.clone());
        let allowed: Vec<_> = self.scope.split_ascii_whitespace().collect();
        if scope.split_ascii_whitespace().next().is_none()
            || scope
                .split_ascii_whitespace()
                .any(|scope| !allowed.contains(&scope))
        {
            return Err("Le serveur a retourné des autorisations OAuth inattendues.".into());
        }
        self.access_token = tokens.access_token;
        if !tokens.refresh_token.is_empty() {
            self.refresh_token = tokens.refresh_token;
        }
        self.expires_at = now()
            .checked_add(tokens.expires_in)
            .ok_or("Expiration OAuth invalide.")?;
        self.scope = scope;
        self.validate()
    }

    pub async fn ensure_access(&mut self) -> Result<(), String> {
        self.validate()?;
        if self.expires_at > now().saturating_add(60) {
            return Ok(());
        }
        if self.refresh_token.is_empty() {
            return Err("Votre connexion Tutor a expiré. Identifiez-vous à nouveau.".into());
        }
        let response = client()?
            .post(endpoint(&self.origin, &self.token_endpoint)?)
            .form(&[
                ("grant_type", "refresh_token"),
                ("client_id", self.client_id.as_str()),
                ("refresh_token", self.refresh_token.as_str()),
                ("resource", self.resource.as_str()),
                ("scope", self.scope.as_str()),
            ])
            .send()
            .await
            .map_err(|_| "Impossible de renouveler la connexion Tutor.")?;
        let tokens: Tokens = json_response(response).await?;
        let mut next = self.clone();
        next.apply_tokens(tokens, false)?;
        *self = next;
        Ok(())
    }

    pub async fn revoke(&self) -> Result<(), String> {
        self.validate()?;
        let client = client()?;
        let metadata = metadata(&client, &self.origin).await?;
        let endpoint_url = metadata.revocation_endpoint.as_deref().ok_or("Le serveur n’annonce pas de révocation OAuth à distance. Les accès doivent être retirés depuis votre institution.")?;
        let (token, hint) = if self.refresh_token.is_empty() {
            (&self.access_token, "access_token")
        } else {
            (&self.refresh_token, "refresh_token")
        };
        let response = client
            .post(endpoint(&self.origin, endpoint_url)?)
            .form(&[
                ("client_id", self.client_id.as_str()),
                ("token", token.as_str()),
                ("token_type_hint", hint),
            ])
            .send()
            .await
            .map_err(|_| "Impossible de révoquer les accès auprès de Tutor.")?;
        if !response.status().is_success() {
            return Err("Le serveur n’a pas confirmé la révocation OAuth.".into());
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{json, Value};
    use std::sync::{Arc, Mutex};

    #[derive(Clone)]
    struct Request {
        path: String,
        headers: String,
        body: String,
    }
    struct Server {
        origin: String,
        requests: Arc<Mutex<Vec<Request>>>,
        task: tokio::task::JoinHandle<()>,
    }
    impl Drop for Server {
        fn drop(&mut self) {
            self.task.abort();
        }
    }

    async fn serve(
        handler: impl Fn(&Request, &str) -> (u16, Value) + Send + Sync + 'static,
    ) -> Server {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server_origin = origin.clone();
        let requests = Arc::new(Mutex::new(Vec::new()));
        let captured = requests.clone();
        let handler = Arc::new(handler);
        let task = tokio::spawn(async move {
            loop {
                let (mut stream, _) = listener.accept().await.unwrap();
                let captured = captured.clone();
                let origin = server_origin.clone();
                let handler = handler.clone();
                tokio::spawn(async move {
                    let mut bytes = Vec::new();
                    let mut buffer = [0_u8; 1024];
                    let header_end;
                    loop {
                        let count = stream.read(&mut buffer).await.unwrap();
                        if count == 0 {
                            return;
                        }
                        bytes.extend_from_slice(&buffer[..count]);
                        if let Some(position) =
                            bytes.windows(4).position(|slice| slice == b"\r\n\r\n")
                        {
                            header_end = position + 4;
                            break;
                        }
                        assert!(bytes.len() < 32 * 1024);
                    }
                    let headers = String::from_utf8(bytes[..header_end].to_vec()).unwrap();
                    let length = headers
                        .lines()
                        .filter_map(|line| line.split_once(':'))
                        .find(|(name, _)| name.eq_ignore_ascii_case("content-length"))
                        .map(|(_, value)| value.trim().parse::<usize>().unwrap())
                        .unwrap_or(0);
                    while bytes.len() < header_end + length {
                        let count = stream.read(&mut buffer).await.unwrap();
                        if count == 0 {
                            break;
                        }
                        bytes.extend_from_slice(&buffer[..count]);
                    }
                    let request = Request {
                        path: headers
                            .lines()
                            .next()
                            .unwrap()
                            .split_whitespace()
                            .nth(1)
                            .unwrap()
                            .into(),
                        headers,
                        body: String::from_utf8(bytes[header_end..].to_vec()).unwrap(),
                    };
                    captured.lock().unwrap().push(request.clone());
                    let (status, value) = handler(&request, &origin);
                    let body = serde_json::to_string(&value).unwrap();
                    let extra = if status == 302 {
                        format!("Location: {origin}/redirected\r\n")
                    } else {
                        String::new()
                    };
                    let response = format!("HTTP/1.1 {status} OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n{extra}\r\n{body}", body.len());
                    stream.write_all(response.as_bytes()).await.unwrap();
                });
            }
        });
        Server {
            origin,
            requests,
            task,
        }
    }

    fn metadata_value(origin: &str) -> Value {
        json!({ "issuer": origin, "authorization_endpoint": format!("{origin}/authorize"), "token_endpoint": format!("{origin}/token"), "registration_endpoint": format!("{origin}/register"), "scopes_supported": ["learner:read", "learner:write"], "code_challenge_methods_supported": ["S256"], "token_endpoint_auth_methods_supported": ["none"], "authorization_response_iss_parameter_supported": true })
    }

    fn form(body: &str) -> std::collections::HashMap<String, String> {
        Url::parse(&format!("http://localhost/?{body}"))
            .unwrap()
            .query_pairs()
            .map(|(key, value)| (key.into_owned(), value.into_owned()))
            .collect()
    }

    fn registered(body: &str) -> Value {
        let value: Value = serde_json::from_str(body).unwrap();
        assert_eq!(value["application_type"], "native");
        assert_eq!(value["token_endpoint_auth_method"], "none");
        json!({ "client_id": "public-client", "token_endpoint_auth_method": "none", "redirect_uris": value["redirect_uris"] })
    }

    fn token_value(refresh: &str) -> Value {
        json!({ "access_token": "fresh-access-token", "refresh_token": refresh, "token_type": "bearer", "expires_in": 3600, "scope": "learner:read learner:write" })
    }

    fn callback_url(authorization: &str, origin: &str, code: &str) -> Url {
        let params = form(Url::parse(authorization).unwrap().query().unwrap());
        let mut callback = Url::parse(&params["redirect_uri"]).unwrap();
        callback
            .query_pairs_mut()
            .append_pair("code", code)
            .append_pair("state", &params["state"])
            .append_pair("iss", origin);
        callback
    }

    #[test]
    fn pkce_matches_rfc7636_and_secrets_have_256_bits_of_input() {
        assert_eq!(
            challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"),
            "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
        );
        let one = random_secret();
        let two = random_secret();
        assert_eq!(URL_SAFE_NO_PAD.decode(&one).unwrap().len(), 32);
        assert_ne!(one, two);
        assert_eq!(one.len(), 43);
        assert!(same_secret(&one, &one));
        assert!(!same_secret(&one, &two));
        assert!(!same_secret(&one, ""));
    }

    #[test]
    fn origins_and_endpoints_cannot_redirect_credentials_to_other_hosts() {
        assert_eq!(
            normalize_origin("https://Tutor.example/").unwrap(),
            "https://tutor.example"
        );
        assert!(normalize_origin("http://127.0.0.1:8080").is_ok());
        assert!(normalize_origin("http://127.0.0.2:8080").is_ok());
        assert!(normalize_origin("http://[::1]:8080").is_ok());
        for invalid in [
            "http://institution.example",
            "https://user:password@institution.example",
            "https://institution.example/mcp",
            "https://institution.example?x=1",
            "https://institution.example#x",
            "file:///tmp/auth",
            " https://institution.example",
            "https://institution.example\n",
            "http://127.0.0.1:0",
        ] {
            assert!(normalize_origin(invalid).is_err(), "accepted {invalid}");
        }
        for invalid in [
            "http://institution.example/token",
            "https://other.example/token",
            "https://institution.example:8443/token",
            "https://user@institution.example/token",
            "https://institution.example/token#fragment",
        ] {
            assert!(
                endpoint("https://institution.example", invalid).is_err(),
                "accepted {invalid}"
            );
        }
    }

    #[tokio::test]
    async fn callback_rejects_wrong_state_issuer_method_and_duplicates_then_accepts_valid_return() {
        let server = serve(|request, origin| match request.path.as_str() {
            "/.well-known/oauth-authorization-server" => (200, metadata_value(origin)),
            "/register" => (200, registered(&request.body)),
            "/token" => (200, token_value("initial-refresh")),
            _ => (404, json!({})),
        })
        .await;
        let (authorization, pending) = begin_login(&server.origin, None, None).await.unwrap();
        let verifier = pending.verifier.clone();
        let params = form(Url::parse(&authorization).unwrap().query().unwrap());
        assert_eq!(params["code_challenge_method"], "S256");
        assert_eq!(params["code_challenge"], challenge(&verifier));
        assert_ne!(params["state"], verifier);
        let valid = callback_url(&authorization, &server.origin, "one-time-code");
        let finish = tokio::spawn(pending.finish());
        let client = client().unwrap();
        for (name, value) in [("state", "wrong"), ("iss", "https://other.example")] {
            let mut invalid = valid.clone();
            let pairs: Vec<(String, String)> = valid
                .query_pairs()
                .map(|(key, item)| {
                    let replacement = if name == key {
                        value.into()
                    } else {
                        item.into_owned()
                    };
                    (key.into_owned(), replacement)
                })
                .collect();
            invalid.query_pairs_mut().clear().extend_pairs(pairs);
            assert_eq!(client.get(invalid).send().await.unwrap().status(), 400);
            assert!(!finish.is_finished());
        }
        assert_eq!(
            client.post(valid.clone()).send().await.unwrap().status(),
            400
        );
        assert_eq!(
            client
                .get(valid.clone())
                .header("Host", "attacker.example")
                .send()
                .await
                .unwrap()
                .status(),
            400
        );
        let mut missing_issuer = valid.clone();
        let missing_issuer_pairs: Vec<_> = valid
            .query_pairs()
            .filter(|(name, _)| name != "iss")
            .map(|(name, value)| (name.into_owned(), value.into_owned()))
            .collect();
        missing_issuer
            .query_pairs_mut()
            .clear()
            .extend_pairs(missing_issuer_pairs);
        assert_eq!(
            client.get(missing_issuer).send().await.unwrap().status(),
            400
        );
        assert_eq!(
            client
                .get(valid.clone())
                .header("X-Padding", "x".repeat(BODY_LIMIT))
                .send()
                .await
                .unwrap()
                .status(),
            431
        );
        let mut duplicated = valid.clone();
        duplicated.query_pairs_mut().append_pair("code", "second");
        assert_eq!(client.get(duplicated).send().await.unwrap().status(), 400);
        let mut wrong_path = valid.clone();
        wrong_path.set_path("/unexpected");
        assert_eq!(client.get(wrong_path).send().await.unwrap().status(), 404);
        assert_eq!(client.get(valid).send().await.unwrap().status(), 200);
        let credentials = finish.await.unwrap().unwrap();
        assert_eq!(credentials.access_token, "fresh-access-token");
        assert_eq!(credentials.refresh_token, "initial-refresh");
        assert_eq!(credentials.resource, format!("{}/mcp", server.origin));
        let requests = server.requests.lock().unwrap();
        let exchange = requests
            .iter()
            .find(|request| request.path == "/token")
            .unwrap();
        let exchange = form(&exchange.body);
        assert_eq!(exchange["code_verifier"], verifier);
        assert_eq!(exchange["grant_type"], "authorization_code");
        assert_eq!(exchange["resource"], credentials.resource);
        assert_eq!(
            requests
                .iter()
                .filter(|request| request.path == "/token")
                .count(),
            1
        );
    }

    #[tokio::test]
    async fn metadata_and_dcr_are_bounded_and_fail_closed_without_following_redirects() {
        for scenario in [
            "wrong-issuer",
            "foreign-token",
            "redirect",
            "large-body",
            "missing-pkce",
            "private-client",
        ] {
            let server = serve(move |request, origin| {
                if request.path == "/register" {
                    let mut registration = registered(&request.body);
                    if scenario == "private-client" {
                        registration["client_secret"] = json!("unexpected-secret");
                    }
                    return (200, registration);
                }
                let mut value = metadata_value(origin);
                match scenario {
                    "wrong-issuer" => value["issuer"] = json!("https://other.example"),
                    "foreign-token" => {
                        value["token_endpoint"] = json!("https://other.example/token")
                    }
                    "redirect" => return (302, json!({})),
                    "large-body" => value["padding"] = json!("x".repeat(BODY_LIMIT)),
                    "missing-pkce" => value["code_challenge_methods_supported"] = json!(["plain"]),
                    _ => {}
                }
                (200, value)
            })
            .await;
            assert!(
                begin_login(&server.origin, None, None).await.is_err(),
                "accepted {scenario}"
            );
            assert!(!server
                .requests
                .lock()
                .unwrap()
                .iter()
                .any(|request| request.path == "/redirected"));
        }
    }

    #[tokio::test]
    async fn dcr_initial_access_token_is_header_only_and_never_part_of_authorization_url() {
        let server = serve(|request, origin| match request.path.as_str() {
            "/.well-known/oauth-authorization-server" => (200, metadata_value(origin)),
            "/register" => (200, registered(&request.body)),
            _ => (404, json!({})),
        })
        .await;
        let (authorization, pending) =
            begin_login(&server.origin, None, Some("registration-secret"))
                .await
                .unwrap();
        assert!(!authorization.contains("registration-secret"));
        let requests = server.requests.lock().unwrap();
        let registration = requests
            .iter()
            .find(|request| request.path == "/register")
            .unwrap();
        assert!(registration
            .headers
            .to_ascii_lowercase()
            .contains("authorization: bearer registration-secret"));
        assert!(!registration.body.contains("registration-secret"));
        assert!(!requests
            .iter()
            .find(|request| request.path.contains("well-known"))
            .unwrap()
            .headers
            .contains("registration-secret"));
        drop(pending);
    }

    #[tokio::test]
    async fn refresh_rotates_credentials_and_does_not_mutate_them_on_invalid_response() {
        let server = serve(|request, _| {
            let input = form(&request.body);
            assert_eq!(input["grant_type"], "refresh_token");
            assert_eq!(input["refresh_token"], "old-refresh");
            assert!(!request
                .headers
                .to_ascii_lowercase()
                .contains("authorization:"));
            (200, token_value("rotated-refresh"))
        })
        .await;
        let mut credentials = OAuthCredentials {
            origin: server.origin.clone(),
            client_id: "public-client".into(),
            access_token: "old-access".into(),
            refresh_token: "old-refresh".into(),
            expires_at: 0,
            token_endpoint: format!("{}/token", server.origin),
            resource: format!("{}/mcp", server.origin),
            scope: "learner:read learner:write".into(),
        };
        credentials.ensure_access().await.unwrap();
        assert_eq!(credentials.refresh_token, "rotated-refresh");
        assert_eq!(credentials.access_token, "fresh-access-token");
        assert!(credentials.expires_at > now() + 3500);
        credentials.ensure_access().await.unwrap();
        assert_eq!(server.requests.lock().unwrap().len(), 1);

        let invalid = serve(|_, _| (200, json!({ "access_token": "provider-secret", "refresh_token": "new-refresh", "expires_in": 3600, "token_type": "bearer", "scope": "admin" }))).await;
        credentials.origin = invalid.origin.clone();
        credentials.token_endpoint = format!("{}/token", invalid.origin);
        credentials.resource = format!("{}/mcp", invalid.origin);
        credentials.expires_at = 0;
        let result = credentials.ensure_access().await;
        assert!(result.is_err());
        assert!(!result.err().unwrap().contains("provider-secret"));
        assert_eq!(credentials.refresh_token, "rotated-refresh");
        assert_eq!(credentials.access_token, "fresh-access-token");
    }

    #[tokio::test]
    async fn callback_denial_is_bound_to_the_expected_state_and_issuer() {
        let server = serve(|request, origin| {
            if request.path == "/register" {
                (200, registered(&request.body))
            } else {
                (200, metadata_value(origin))
            }
        })
        .await;
        let (authorization, pending) = begin_login(&server.origin, None, None).await.unwrap();
        let mut denial = callback_url(&authorization, &server.origin, "unused");
        let pairs: Vec<_> = denial
            .query_pairs()
            .filter(|(name, _)| name != "code")
            .map(|(key, value)| (key.into_owned(), value.into_owned()))
            .collect();
        denial
            .query_pairs_mut()
            .clear()
            .extend_pairs(pairs)
            .append_pair("error", "access_denied")
            .append_pair("error_description", "provider-secret");
        let finish = tokio::spawn(pending.finish());
        assert_eq!(
            client().unwrap().get(denial).send().await.unwrap().status(),
            200
        );
        let result = finish.await.unwrap();
        assert!(result.is_err());
        assert!(!result.err().unwrap().contains("provider-secret"));
        assert!(!server
            .requests
            .lock()
            .unwrap()
            .iter()
            .any(|request| request.path == "/token"));
    }

    #[tokio::test]
    async fn a_pre_registered_public_client_uses_the_documented_fixed_callback_without_dcr() {
        let server = serve(|request, origin| {
            assert_eq!(request.path, "/.well-known/oauth-authorization-server");
            let mut value = metadata_value(origin);
            value
                .as_object_mut()
                .unwrap()
                .remove("registration_endpoint");
            (200, value)
        })
        .await;
        assert!(begin_login(&server.origin, None, None).await.is_err());
        let (authorization, pending) = begin_login(&server.origin, Some("configured-client"), None)
            .await
            .unwrap();
        let params = form(Url::parse(&authorization).unwrap().query().unwrap());
        assert_eq!(params["client_id"], "configured-client");
        assert_eq!(
            params["redirect_uri"],
            "http://127.0.0.1:47823/oauth/callback"
        );
        assert_eq!(
            pending.listener.local_addr().unwrap().port(),
            STATIC_CLIENT_PORT
        );
        assert!(server
            .requests
            .lock()
            .unwrap()
            .iter()
            .all(|request| request.path != "/register"));
    }

    #[tokio::test]
    async fn an_abandoned_login_has_a_bounded_deadline_and_releases_its_listener() {
        let server = serve(|request, origin| {
            if request.path == "/register" {
                (200, registered(&request.body))
            } else {
                (200, metadata_value(origin))
            }
        })
        .await;
        let (_, mut pending) = begin_login(&server.origin, None, None).await.unwrap();
        let address = pending.listener.local_addr().unwrap();
        pending.deadline = Instant::now() + Duration::from_millis(20);
        let result = pending.finish().await;
        assert!(result.is_err());
        assert!(TcpListener::bind(address).await.is_ok());
    }

    #[tokio::test]
    async fn refresh_does_not_follow_redirects_or_echo_provider_secrets() {
        let server = serve(|request, _| {
            if request.path == "/token" {
                (302, json!({ "access_token": "provider-secret" }))
            } else {
                (200, token_value("should-never-be-issued"))
            }
        })
        .await;
        let mut credentials = OAuthCredentials {
            origin: server.origin.clone(),
            client_id: "client".into(),
            access_token: "old-access".into(),
            refresh_token: "old-refresh".into(),
            expires_at: 0,
            token_endpoint: format!("{}/token", server.origin),
            resource: format!("{}/mcp", server.origin),
            scope: "learner:read learner:write".into(),
        };
        let result = credentials.ensure_access().await;
        assert!(result.is_err());
        assert!(!result.err().unwrap().contains("provider-secret"));
        assert_eq!(credentials.refresh_token, "old-refresh");
        assert_eq!(server.requests.lock().unwrap().len(), 1);
        credentials.token_endpoint = "https://attacker.example/token".into();
        assert!(credentials.ensure_access().await.is_err());
        assert_eq!(server.requests.lock().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn revocation_uses_only_an_advertised_same_origin_endpoint() {
        let server = serve(|request, origin| {
            if request.path == "/revoke" {
                let input = form(&request.body);
                assert_eq!(input["token"], "refresh-to-revoke");
                assert_eq!(input["token_type_hint"], "refresh_token");
                return (200, json!({}));
            }
            let mut value = metadata_value(origin);
            value["revocation_endpoint"] = json!(format!("{origin}/revoke"));
            (200, value)
        })
        .await;
        let mut credentials = OAuthCredentials {
            origin: server.origin.clone(),
            client_id: "client".into(),
            access_token: "access".into(),
            refresh_token: "refresh-to-revoke".into(),
            expires_at: now() + 100,
            token_endpoint: format!("{}/token", server.origin),
            resource: format!("{}/mcp", server.origin),
            scope: "learner".into(),
        };
        credentials.revoke().await.unwrap();
        assert_eq!(server.requests.lock().unwrap().len(), 2);
        let no_revoke = serve(|_, origin| (200, metadata_value(origin))).await;
        credentials.origin = no_revoke.origin.clone();
        credentials.resource = format!("{}/mcp", no_revoke.origin);
        credentials.token_endpoint = format!("{}/token", no_revoke.origin);
        assert!(credentials.revoke().await.is_err());
        assert_eq!(no_revoke.requests.lock().unwrap().len(), 1);
    }
}
