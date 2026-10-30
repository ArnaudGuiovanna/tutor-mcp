// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

//! A single serial MCP connection. Configuration comes from the ACP host;
//! neither a model response nor a tool result can supply a command or URL.

use crate::{catalog::ToolEntry, mcp, mcp_stdio};
use serde_json::Value;
use std::{path::Path, sync::atomic::AtomicBool};

pub enum Client {
    Http(mcp::McpClient),
    Stdio(mcp_stdio::StdioClient),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    InvalidConfig,
    Connection,
    Catalog,
    Call,
    Close,
}
impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::InvalidConfig => "mcp_configuration_invalid",
            Self::Connection => "mcp_connection_failed",
            Self::Catalog => "mcp_catalog_failed",
            Self::Call => "mcp_call_outcome_unknown",
            Self::Close => "mcp_close_failed",
        })
    }
}
impl std::error::Error for Error {}

pub trait Transport {
    fn server_id(&self) -> &str;
    fn call_tool<'a>(
        &'a mut self,
        name: &'a str,
        args: Value,
        key: Option<&'a str>,
        cancel: &'a AtomicBool,
    ) -> impl std::future::Future<Output = Result<Value, Error>> + Send + 'a;
}

impl Client {
    pub async fn connect(config: &Value, cwd: &Path, cancel: &AtomicBool) -> Result<Self, Error> {
        match config.get("type").and_then(Value::as_str) {
            Some("http") => mcp::McpClient::connect(
                mcp::ServerConfig::from_acp(config).map_err(|_| Error::InvalidConfig)?,
                cancel,
            )
            .await
            .map(Self::Http)
            .map_err(|_| Error::Connection),
            None | Some("stdio") => mcp_stdio::StdioClient::connect(
                mcp_stdio::StdioConfig::from_acp(config, cwd).map_err(|_| Error::InvalidConfig)?,
                cancel,
            )
            .await
            .map(Self::Stdio)
            .map_err(|_| Error::Connection),
            _ => Err(Error::InvalidConfig),
        }
    }
    pub async fn list_tools(&mut self, cancel: &AtomicBool) -> Result<Vec<ToolEntry>, Error> {
        match self {
            Self::Http(client) => client.list_tools(cancel).await.map_err(|_| Error::Catalog),
            Self::Stdio(client) => client.list_tools(cancel).await.map_err(|_| Error::Catalog),
        }
    }
    pub fn take_tools_changed(&mut self) -> bool {
        match self {
            Self::Http(client) => client.take_tools_changed(),
            Self::Stdio(client) => client.take_tools_changed(),
        }
    }
    pub async fn close(&mut self) -> Result<(), Error> {
        match self {
            Self::Http(client) => {
                client.close();
                Ok(())
            }
            Self::Stdio(client) => client.close().await.map_err(|_| Error::Close),
        }
    }
}
impl Transport for Client {
    fn server_id(&self) -> &str {
        match self {
            Self::Http(client) => client.server_id(),
            Self::Stdio(client) => client.server_id(),
        }
    }
    async fn call_tool(
        &mut self,
        name: &str,
        args: Value,
        key: Option<&str>,
        cancel: &AtomicBool,
    ) -> Result<Value, Error> {
        match self {
            Self::Http(client) => client
                .call_tool(name, args, key, cancel)
                .await
                .map_err(|_| Error::Call),
            Self::Stdio(client) => client
                .call_tool(name, args, key, cancel)
                .await
                .map_err(|_| Error::Call),
        }
    }
}
