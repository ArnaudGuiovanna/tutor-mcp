// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT
//! Real Rust client ↔ Go institution qualification. All identities and backup
//! phrases in this program belong to its disposable test fixture.
use serde_json::{json, Value};
use std::path::Path;
use tokio::io::AsyncBufReadExt;
use tutor_client::{
    identity::{IdentityStore, KeyPair},
    native_auth::{self, Session},
    workspace,
};

const BACKUP_PHRASE: &str = "Disposable native qualification fixture only";

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("Native institution qualification: {error}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), String> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.as_slice() {
        [command, directory] if command == "prepare" => {
            let directory = Path::new(directory);
            let mut identities = serde_json::Map::new();
            for role in ["owner", "learner"] {
                let mut store = IdentityStore::load(&directory.join(role))?;
                store.create(None)?;
                store.backup(
                    &directory.join(format!("{role}-backup.json")),
                    BACKUP_PHRASE,
                )?;
                store.confirm_backup()?;
                identities.insert(
                    role.into(),
                    serde_json::to_value(store.public())
                        .map_err(|_| "Public identity serialization failed")?,
                );
            }
            let data = serde_json::to_vec_pretty(&identities)
                .map_err(|_| "Public identity serialization failed")?;
            workspace::private_write(&directory.join("public-keys.json"), &data)?;
            println!("Prepared two independent client identities and encrypted offline backups.");
            Ok(())
        }
        [command, directory, url] if command == "verify" => verify(Path::new(directory), url).await,
        _ => Err(
            "Usage: tutor-client-native-qualify prepare DIRECTORY | verify DIRECTORY HTTPS_URL"
                .into(),
        ),
    }
}

async fn overview(
    client: &reqwest::Client,
    session: &Session,
    wanted: u16,
) -> Result<Value, String> {
    let response = client
        .get(format!("{}/api/client/v1/overview", session.origin))
        .bearer_auth(&session.access_token)
        .send()
        .await
        .map_err(|_| "Overview transport failed")?;
    if response.status().as_u16() != wanted {
        return Err(format!(
            "Unexpected overview status {}, expected {wanted}",
            response.status()
        ));
    }
    if wanted != 200 {
        return Ok(Value::Null);
    }
    response
        .json()
        .await
        .map_err(|_| "Overview JSON invalid".into())
}

async fn rpc(
    client: &reqwest::Client,
    session: &Session,
    method: &str,
    params: Value,
) -> Result<Value, String> {
    let response = client
        .post(&session.institution.mcp_url)
        .bearer_auth(&session.access_token)
        .header("Accept", "application/json, text/event-stream")
        .header("MCP-Protocol-Version", "2025-03-26")
        .json(&json!({"jsonrpc":"2.0","id":1,"method":method,"params":params}))
        .send()
        .await
        .map_err(|_| "MCP transport failed")?;
    if !response.status().is_success() {
        return Err(format!(
            "MCP request rejected with status {}",
            response.status()
        ));
    }
    let text = response
        .text()
        .await
        .map_err(|_| "MCP response unavailable")?;
    let response: Value = serde_json::from_str(&text)
        .ok()
        .or_else(|| {
            text.lines()
                .filter_map(|line| line.strip_prefix("data: "))
                .find_map(|line| serde_json::from_str(line).ok())
        })
        .ok_or("MCP response JSON invalid")?;
    if response.get("error").is_some() || response["result"]["isError"] == true {
        return Err(format!("MCP operation {method} was rejected"));
    }
    Ok(response["result"].clone())
}

async fn tool(
    client: &reqwest::Client,
    session: &Session,
    name: &str,
    args: Value,
) -> Result<Value, String> {
    let result = rpc(
        client,
        session,
        "tools/call",
        json!({"name":name,"arguments":args}),
    )
    .await?;
    if let Some(value) = result.get("structuredContent") {
        return Ok(value.clone());
    }
    result["content"]
        .as_array()
        .and_then(|parts| {
            parts.iter().find_map(|part| {
                part["text"]
                    .as_str()
                    .and_then(|text| serde_json::from_str(text).ok())
            })
        })
        .ok_or_else(|| format!("MCP operation {name} returned no structured result"))
}

fn id(value: &Value, names: &[&str]) -> Result<String, String> {
    names
        .iter()
        .find_map(|name| value[*name].as_str())
        .filter(|id| !id.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| "Formation operation returned no identifier".into())
}

async fn admin(
    client: &reqwest::Client,
    session: &Session,
    path: &str,
    body: Value,
    retry: &str,
) -> Result<Value, String> {
    let response = client
        .post(format!("{}/admin/catalog{path}", session.origin))
        .bearer_auth(&session.access_token)
        .header("Idempotency-Key", retry)
        .json(&body)
        .send()
        .await
        .map_err(|_| "Catalogue transport failed")?;
    if !response.status().is_success() {
        return Err(format!(
            "Catalogue mutation rejected: {}",
            response.status()
        ));
    }
    response
        .json()
        .await
        .map_err(|_| "Catalogue JSON invalid".into())
}

async fn erasure(
    client: &reqwest::Client,
    owner: &Session,
    learner: &Session,
    owner_public_key: &str,
) -> Result<(), String> {
    let helper =
        std::env::var("SMOKE_DSAR_HELPER").map_err(|_| "Real worker erasure helper is required")?;
    let mut child = tokio::process::Command::new(helper)
        .args([
            &owner.principal.tenant_id,
            &learner.principal.learner_id,
            owner_public_key,
        ])
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::inherit())
        .kill_on_drop(true)
        .spawn()
        .map_err(|_| "Erasure helper failed to start")?;
    let stdout = child
        .stdout
        .take()
        .ok_or("Erasure helper output unavailable")?;
    let mut lines = tokio::io::BufReader::new(stdout).lines();
    for expected in ["queued", "revoked", "completed"] {
        let line = tokio::time::timeout(std::time::Duration::from_secs(160), lines.next_line())
            .await
            .map_err(|_| "Worker erasure deadline expired")?
            .map_err(|_| "Worker erasure output failed")?
            .ok_or("Worker erasure ended prematurely")?;
        let event: Value =
            serde_json::from_str(&line).map_err(|_| "Worker erasure event invalid")?;
        if event["event"] != expected {
            return Err("Unexpected worker erasure phase".into());
        }
        if expected == "revoked" {
            overview(client, learner, 401).await?;
        }
    }
    if !child
        .wait()
        .await
        .map_err(|_| "Erasure helper status unavailable")?
        .success()
    {
        return Err("Worker erasure verification failed".into());
    }
    overview(client, owner, 200).await?;
    Ok(())
}

async fn verify(directory: &Path, url: &str) -> Result<(), String> {
    let client = reqwest::Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(std::time::Duration::from_secs(45))
        .build()
        .map_err(|_| "HTTP client unavailable")?;
    for path in [
        "/authorize",
        "/signup",
        "/invite",
        "/console/login",
        "/learn/login",
        "/verify-email",
        "/recover",
        "/reset-password",
    ] {
        let response = client
            .get(format!("{url}{path}"))
            .send()
            .await
            .map_err(|_| "Retired route probe failed")?;
        if response.status().as_u16() != 404 {
            return Err(format!("Retired account route is reachable: {path}"));
        }
    }
    let owner = IdentityStore::load(&directory.join("owner"))?;
    let owner_keys = owner.active()?;
    let host_session = native_auth::open_session(url, &owner_keys, "formation:contribute").await?;
    let action_url = format!("{url}/api/client/v1/institution/actions");
    let body = serde_json::to_vec(&json!({"action":"overview","input":{}}))
        .map_err(|_| "Host action JSON serialization failed")?;
    let unsigned = client
        .post(&action_url)
        .bearer_auth(&host_session.access_token)
        .header("Content-Type", "application/json")
        .body(body.clone())
        .send()
        .await
        .map_err(|_| "Unsigned host action probe failed")?;
    if unsigned.status().as_u16() != 401 {
        return Err("Delegated bearer alone reached an institutional account action".into());
    }
    let proof = native_auth::signed_institution_action(
        &owner_keys,
        url,
        &action_url,
        &host_session.principal.tenant_id,
        &host_session.session_id,
        &body,
    )?;
    let another_host_session =
        native_auth::open_session(url, &owner_keys, "formation:contribute").await?;
    let cross_session = client
        .post(&action_url)
        .bearer_auth(&another_host_session.access_token)
        .header("Content-Type", "application/json")
        .header("X-Tutor-Action-Proof", &proof)
        .body(body.clone())
        .send()
        .await
        .map_err(|_| "Host session-binding probe failed")?;
    if cross_session.status().as_u16() != 401 {
        return Err("A host proof signed for another native session was accepted".into());
    }
    another_host_session.revoke(&owner_keys).await?;
    for replay in [false, true] {
        let response = client
            .post(&action_url)
            .bearer_auth(&host_session.access_token)
            .header("Content-Type", "application/json")
            .header("X-Tutor-Action-Proof", &proof)
            .body(body.clone())
            .send()
            .await
            .map_err(|_| "Signed host action transport failed")?;
        if response.status().as_u16() != if replay { 401 } else { 200 } {
            return Err("Rust host proof or persistent replay protection was rejected".into());
        }
        if !replay {
            let value: Value = response
                .json()
                .await
                .map_err(|_| "Host action JSON invalid")?;
            if value["institution"]["id"] != host_session.principal.tenant_id
                || value["membership"]["id"] != host_session.principal.membership_id
            {
                return Err(
                    "Signed host action crossed an institution or membership boundary".into(),
                );
            }
        }
    }
    host_session.revoke(&owner_keys).await?;
    println!("Rust/Go host account proof: exact body signature, tenant/session binding, native membership and single-use nonce verified.");
    let read = native_auth::open_session(url, &owner_keys, "").await?;
    if read.scope != "formation:read" || !read.principal.learner_id.is_empty() {
        return Err("Owner was given implicit write access or a synthetic learner".into());
    }
    let read_overview = overview(&client, &read, 200).await?;
    if read_overview["capabilities"]["formationAuthoring"] != false
        || read_overview["capabilities"]["learning"] != false
    {
        return Err("Owner read-only capabilities are incorrect".into());
    }
    read.revoke(&owner_keys).await?;
    overview(&client, &read, 401).await?;
    let owner_session =
        native_auth::open_session(url, &owner_keys, "formation:read formation:write").await?;
    let owner_view = overview(&client, &owner_session, 200).await?;
    if owner_view["capabilities"]["formationAuthoring"] != true {
        return Err("Owner explicit authoring grant unavailable".into());
    }
    rpc(&client, &owner_session, "initialize", json!({"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"tutor-native-qualification","version":"1"}})).await?;
    let draft = tool(&client, &owner_session, "draft_formation", json!({"name":"Native key qualification","description":"Real institution creation from the Rust client","idempotency_key":"native-qualification-draft"})).await?;
    let formation_id = id(&draft["formation"], &["id", "ID"])?;
    let version_id = id(&draft["version"], &["id", "ID"])?;
    let replay = tool(&client, &owner_session, "draft_formation", json!({"name":"Native key qualification","description":"Real institution creation from the Rust client","idempotency_key":"native-qualification-draft"})).await?;
    if id(&replay["formation"], &["id", "ID"])? != formation_id {
        return Err("Retry duplicated a formation".into());
    }
    tool(&client, &owner_session, "add_formation_concepts", json!({"version_id":version_id,"idempotency_key":"native-qualification-concepts","modules":[{"stable_key":"foundation","title":"Foundation","position":0}],"concepts":[{"module_stable_key":"foundation","stable_key":"equinox","label":"Explain an equinox","position":0,"description":"Explain why day and night have comparable durations near an equinox."}]})).await?;
    tool(
        &client,
        &owner_session,
        "get_formation_version",
        json!({"version_id":version_id}),
    )
    .await?;
    tool(
        &client,
        &owner_session,
        "publish_formation",
        json!({"version_id":version_id,"idempotency_key":"native-qualification-publish"}),
    )
    .await?;
    let learner = IdentityStore::load(&directory.join("learner"))?;
    let learner_keys = learner.active()?;
    if native_auth::open_session(url, &learner_keys, "formation:write")
        .await
        .is_ok()
    {
        return Err("Learner acquired an authoring grant".into());
    }
    let learner_session = native_auth::open_session(url, &learner_keys, "").await?;
    if learner_session.principal.learner_id.is_empty() {
        return Err("Learner membership has no actual learner".into());
    }
    overview(&client, &learner_session, 200).await?;
    let cohort = admin(
        &client,
        &owner_session,
        "/cohorts",
        json!({"formation_version_id":version_id,"name":"Native cohort","capacity":10}),
        "native-cohort",
    )
    .await?;
    let cohort_id = id(&cohort["cohort"], &["id"])?;
    admin(&client, &owner_session, &format!("/cohorts/{cohort_id}/admissions"), json!({"membership_id":learner_session.principal.membership_id,"decision":"invited","expected_version":0}), "native-admission").await?;
    let joined = tool(
        &client,
        &learner_session,
        "join_formation",
        json!({"cohort_id":cohort_id,"idempotency_key":"native-join"}),
    )
    .await?;
    let domain_id = id(&joined["enrollment"], &["domain_id"])?;
    let activity = tool(
        &client,
        &learner_session,
        "get_next_activity",
        json!({"domain_id":domain_id}),
    )
    .await?;
    let selected = &activity["activity"];
    let prepared = tool(&client, &learner_session, "prepare_assessment_attempt", json!({"domain_id":domain_id,"session_id":activity["session_id"],"decision_id":activity["decision_id"],"concept":selected["concept"],"activity_type":selected["type"],"observable":"Explain an equinox","task_text":"Explain why days and nights have similar lengths near an equinox.","rubric_json":json!({"criteria":[{"id":"correct","description":"Explains approximately equal day and night","max_score":1}],"passing_score":1}).to_string()})).await?;
    tool(&client, &learner_session, "submit_assessment_attempt", json!({"attempt_id":prepared["attempt_id"],"learner_response":"The Sun is near the celestial equator, giving approximately equal day and night."})).await?;
    tool(&client, &learner_session, "record_interaction", json!({"domain_id":domain_id,"session_id":activity["session_id"],"concept":selected["concept"],"activity_type":selected["type"],"success":true,"response_time_seconds":5,"confidence":0.9,"attempt_id":prepared["attempt_id"],"evaluator_id":"native-qualification","evaluation_method":"host_llm","rubric_score_json":json!({"criteria_scores":[{"id":"correct","score":1,"evidence":"Explains the position of the Sun and approximately equal day and night."}],"total":1,"max_total":1}).to_string()})).await?;
    let old_pair = KeyPair::restore(&directory.join("owner-backup.json"), BACKUP_PHRASE)?;
    let replacement = KeyPair::generate();
    replacement.backup(
        &directory.join("owner-replacement-backup.json"),
        BACKUP_PHRASE,
    )?;
    native_auth::replace_keys(
        &owner_session.institution,
        &old_pair.recovery,
        &replacement,
        true,
        None,
    )
    .await?;
    overview(&client, &owner_session, 401).await?;
    if native_auth::open_session(url, &owner_keys, "")
        .await
        .is_ok()
    {
        return Err("Recovered identity still accepts its old primary key".into());
    }
    let recovered =
        native_auth::open_session(url, &replacement.primary, "formation:read formation:write")
            .await?;
    if recovered.principal.user_id != owner_session.principal.user_id
        || recovered.principal.membership_id != owner_session.principal.membership_id
    {
        return Err("Recovery replaced the account instead of recovering it".into());
    }
    tool(
        &client,
        &recovered,
        "get_formation_version",
        json!({"version_id":version_id}),
    )
    .await?;
    erasure(
        &client,
        &recovered,
        &learner_session,
        &replacement.primary.public_key().to_hex(),
    )
    .await?;
    recovered.revoke(&replacement.primary).await?;
    println!(
        "{}",
        json!({"nativeClientRust":true,"tlsVerified":true,"noMailbox":true,"ownerWithoutLearner":true,"explicitAuthoring":true,"formationCreated":true,"formationPublished":true,"idempotentReplay":true,"learnerEscalationDenied":true,"tutoringRecorded":true,"backupRestored":true,"offlineRecovery":true,"oldSessionsRevoked":true,"sameAccountRecovered":true,"formationRetained":true,"workerErasure":true,"formationId":formation_id,"versionId":version_id})
    );
    Ok(())
}
