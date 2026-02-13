use std::path::{Path, PathBuf};

use anyhow::{Context, Result};
use rand::Rng;

const TOKEN_LENGTH: usize = 32;

/// Generate a random auth token and write it to `data_dir/token`.
pub fn generate_token(data_dir: &Path) -> Result<String> {
    let token: String = rand::thread_rng()
        .sample_iter(&rand::distributions::Alphanumeric)
        .take(TOKEN_LENGTH)
        .map(char::from)
        .collect();

    let path = token_path(data_dir);
    std::fs::write(&path, &token)
        .with_context(|| format!("writing token to {}", path.display()))?;

    // Restrict permissions (owner read/write only)
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
    }

    Ok(token)
}

/// Read existing token from disk, env var, or generate a new one.
///
/// Priority: CODEWIRE_TOKEN env var > existing file > generate new.
pub fn load_or_generate_token(data_dir: &Path) -> Result<String> {
    // Allow pre-setting token via env var (useful for containers)
    if let Ok(token) = std::env::var("CODEWIRE_TOKEN") {
        let token = token.trim().to_string();
        if !token.is_empty() {
            // Write to disk so validate_token() works
            let path = token_path(data_dir);
            std::fs::write(&path, &token)
                .with_context(|| format!("writing token to {}", path.display()))?;
            return Ok(token);
        }
    }

    let path = token_path(data_dir);
    if path.exists() {
        let token = std::fs::read_to_string(&path)
            .with_context(|| format!("reading {}", path.display()))?;
        let token = token.trim().to_string();
        if !token.is_empty() {
            return Ok(token);
        }
    }
    generate_token(data_dir)
}

/// Validate a candidate token against the stored token.
pub fn validate_token(data_dir: &Path, candidate: &str) -> bool {
    let path = token_path(data_dir);
    match std::fs::read_to_string(path) {
        Ok(stored) => stored.trim() == candidate.trim(),
        Err(_) => false,
    }
}

fn token_path(data_dir: &Path) -> PathBuf {
    data_dir.join("token")
}
