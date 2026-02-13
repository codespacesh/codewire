use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct Config {
    #[serde(default)]
    pub daemon: DaemonConfig,
    #[serde(default)]
    pub nats: Option<NatsConfig>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DaemonConfig {
    /// Human-readable name for this daemon (used in fleet discovery).
    #[serde(default = "default_name")]
    pub name: String,
    /// WebSocket listen address (e.g. "0.0.0.0:9100"). None = no WebSocket listener.
    pub listen: Option<String>,
    /// Externally-accessible WSS URL for fleet discovery
    /// (e.g. "wss://9100--workspace.coder.codespace.sh/ws").
    pub external_url: Option<String>,
}

impl Default for DaemonConfig {
    fn default() -> Self {
        Self {
            name: default_name(),
            listen: None,
            external_url: None,
        }
    }
}

/// Validate daemon name for use in NATS subjects (`.` is the NATS delimiter).
pub fn validate_daemon_name(name: &str) -> Result<()> {
    if name.is_empty()
        || !name
            .chars()
            .all(|c| c.is_alphanumeric() || c == '-' || c == '_')
    {
        bail!(
            "daemon name must be non-empty and alphanumeric (with - or _), got: {:?}",
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

        // Override external_url from env if not set in config
        if config.daemon.external_url.is_none() {
            config.daemon.external_url = std::env::var("CODEWIRE_EXTERNAL_URL").ok();
        }

        validate_daemon_name(&config.daemon.name)?;

        Ok(config)
    }
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
