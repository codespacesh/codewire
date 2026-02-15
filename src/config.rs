use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct Config {
    #[serde(default)]
    pub node: NodeConfig,
    #[serde(default)]
    pub nats: Option<NatsConfig>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NodeConfig {
    /// Human-readable name for this node (used in fleet discovery).
    #[serde(default = "default_name")]
    pub name: String,
    /// WebSocket listen address (e.g. "0.0.0.0:9100"). None = no WebSocket listener.
    pub listen: Option<String>,
    /// Externally-accessible WSS URL for fleet discovery
    /// (e.g. "wss://9100--workspace.coder.codespace.sh/ws").
    pub external_url: Option<String>,
}

impl Default for NodeConfig {
    fn default() -> Self {
        Self {
            name: default_name(),
            listen: None,
            external_url: None,
        }
    }
}

/// Validate node name for use in NATS subjects (`.` is the NATS delimiter).
pub fn validate_node_name(name: &str) -> Result<()> {
    if name.is_empty()
        || !name
            .chars()
            .all(|c| c.is_alphanumeric() || c == '-' || c == '_')
    {
        bail!(
            "node name must be non-empty and alphanumeric (with - or _), got: {:?}",
            name
        );
    }
    Ok(())
}

fn default_name() -> String {
    let raw = std::env::var("HOSTNAME")
        .or_else(|_| std::env::var("HOST"))
        .unwrap_or_else(|_| "codewire".to_string());
    // Sanitize: replace dots and other invalid chars with hyphens
    raw.chars()
        .map(|c| {
            if c.is_alphanumeric() || c == '-' || c == '_' {
                c
            } else {
                '-'
            }
        })
        .collect()
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NatsConfig {
    pub url: String,
    pub token: Option<String>,
    pub creds_file: Option<PathBuf>,
}

/// Saved remote server entry (client-side).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ServerEntry {
    pub url: String,
    pub token: String,
}

/// Client-side servers config (~/.codewire/servers.toml).
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ServersConfig {
    #[serde(default)]
    pub servers: std::collections::HashMap<String, ServerEntry>,
}

impl Config {
    pub fn load(data_dir: &Path) -> Result<Self> {
        let path = data_dir.join("config.toml");
        let mut config: Self = if path.exists() {
            let content = std::fs::read_to_string(&path)
                .with_context(|| format!("reading {}", path.display()))?;
            toml::from_str(&content).with_context(|| format!("parsing {}", path.display()))?
        } else {
            Self::default()
        };

        // Override node config from env vars
        if let Ok(name) = std::env::var("CODEWIRE_NODE_NAME") {
            config.node.name = name;
        }
        if config.node.listen.is_none() {
            config.node.listen = std::env::var("CODEWIRE_LISTEN").ok();
        }
        if config.node.external_url.is_none() {
            config.node.external_url = std::env::var("CODEWIRE_EXTERNAL_URL").ok();
        }

        // Auto-discover NATS from env vars / well-known paths
        if config.nats.is_none() {
            config.nats = auto_discover_nats();
        }

        validate_node_name(&config.node.name)?;

        Ok(config)
    }
}

/// Auto-discover NATS config from environment variables or well-known paths.
fn auto_discover_nats() -> Option<NatsConfig> {
    // Check environment variables first
    if let Ok(url) = std::env::var("CODEWIRE_NATS_URL") {
        return Some(NatsConfig {
            url,
            token: std::env::var("CODEWIRE_NATS_TOKEN").ok(),
            creds_file: std::env::var("CODEWIRE_NATS_CREDS").ok().map(PathBuf::from),
        });
    }

    // Check well-known path (mounted by Coder template in codespace.sh workspaces)
    let creds_path = PathBuf::from("/etc/codewire/nats.creds");
    if creds_path.exists() {
        let url = std::env::var("CODEWIRE_NATS_URL")
            .unwrap_or_else(|_| "nats://nats.codespace-system:4222".to_string());
        return Some(NatsConfig {
            url,
            token: None,
            creds_file: Some(creds_path),
        });
    }

    None
}

impl ServersConfig {
    pub fn load(data_dir: &Path) -> Result<Self> {
        let path = data_dir.join("servers.toml");
        if path.exists() {
            let content = std::fs::read_to_string(&path)?;
            toml::from_str(&content).context("parsing servers.toml")
        } else {
            Ok(Self::default())
        }
    }

    pub fn save(&self, data_dir: &Path) -> Result<()> {
        let path = data_dir.join("servers.toml");
        let content = toml::to_string_pretty(self)?;
        std::fs::write(&path, content)?;
        Ok(())
    }
}
