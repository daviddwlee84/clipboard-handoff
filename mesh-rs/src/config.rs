//! Paths, persisted Ed25519 identity, and the config KV store (SPEC §5).

use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
};

use anyhow::{Context, Result, anyhow};
use iroh::SecretKey;

/// Resolved runtime paths derived from global flags + OS defaults.
#[derive(Clone, Debug)]
pub struct Paths {
    pub config_dir: PathBuf,
    pub socket: PathBuf,
    pub room: String,
}

impl Paths {
    pub fn resolve(
        config_dir: Option<PathBuf>,
        socket: Option<PathBuf>,
        room: String,
    ) -> Result<Self> {
        let config_dir = match config_dir {
            Some(d) => d,
            None => default_config_dir()?,
        };
        // Default socket lives next to the config so a client and the daemon it
        // auto-spawns (same --config-dir) always agree on the path.
        let socket = socket.unwrap_or_else(|| config_dir.join("daemon.sock"));
        Ok(Self {
            config_dir,
            socket,
            room,
        })
    }

    pub fn secret_path(&self) -> PathBuf {
        self.config_dir.join("secret.key")
    }
    pub fn config_path(&self) -> PathBuf {
        self.config_dir.join("config.json")
    }

    pub fn ensure_dir(&self) -> Result<()> {
        std::fs::create_dir_all(&self.config_dir)
            .with_context(|| format!("create config dir {}", self.config_dir.display()))
    }
}

fn default_config_dir() -> Result<PathBuf> {
    let pd = directories::ProjectDirs::from("", "", "mesh-rs")
        .ok_or_else(|| anyhow!("cannot determine config dir"))?;
    Ok(pd.config_dir().to_path_buf())
}

/// Load the persisted Ed25519 secret, or generate + persist a new one.
pub fn load_or_create_secret(path: &Path) -> Result<SecretKey> {
    if path.exists() {
        let bytes = std::fs::read(path).with_context(|| format!("read {}", path.display()))?;
        let arr: [u8; 32] = bytes
            .as_slice()
            .try_into()
            .map_err(|_| anyhow!("secret key file must be exactly 32 bytes"))?;
        Ok(SecretKey::from_bytes(&arr))
    } else {
        let sk = SecretKey::generate();
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        std::fs::write(path, sk.to_bytes()).with_context(|| format!("write {}", path.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600));
        }
        Ok(sk)
    }
}

/// Simple persisted key/value config (SPEC §5), stored as JSON.
#[derive(Debug, Default)]
pub struct ConfigStore {
    path: PathBuf,
    map: BTreeMap<String, String>,
}

impl ConfigStore {
    pub fn load(path: PathBuf) -> Result<Self> {
        let map = if path.exists() {
            let s = std::fs::read_to_string(&path)?;
            serde_json::from_str(&s).unwrap_or_default()
        } else {
            BTreeMap::new()
        };
        Ok(Self { path, map })
    }

    /// Value for `key`, applying SPEC §5 defaults when unset.
    pub fn get(&self, key: &str) -> Option<String> {
        if let Some(v) = self.map.get(key) {
            return Some(v.clone());
        }
        match key {
            "auto_copy" => Some("notify".into()),
            "internet" => Some("off".into()),
            "broadcast_on_copy" => Some("off".into()),
            "device_name" => Some(default_device_name()),
            "room" => Some("default".into()),
            _ => None,
        }
    }

    pub fn set(&mut self, key: &str, value: &str) -> Result<()> {
        self.map.insert(key.to_string(), value.to_string());
        let s = serde_json::to_string_pretty(&self.map)?;
        std::fs::write(&self.path, s).with_context(|| format!("write {}", self.path.display()))?;
        Ok(())
    }

    pub fn auto_copy(&self) -> String {
        self.get("auto_copy").unwrap_or_else(|| "notify".into())
    }
}

pub fn default_device_name() -> String {
    if let Ok(out) = std::process::Command::new("hostname").output() {
        if out.status.success() {
            let name = String::from_utf8_lossy(&out.stdout).trim().to_string();
            if !name.is_empty() {
                return name;
            }
        }
    }
    std::env::var("HOSTNAME")
        .or_else(|_| std::env::var("COMPUTERNAME"))
        .unwrap_or_else(|_| "clip-device".into())
}
