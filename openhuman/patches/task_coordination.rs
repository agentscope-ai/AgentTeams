//! Delegate receipt validation to the same protocol boundary as Python/Node Harnesses.
use serde_json::{json, Value};
use std::process::Stdio;
use tokio::io::AsyncWriteExt;
use once_cell::sync::Lazy;
use std::sync::Mutex;
use std::collections::HashSet;

static ROOMS: Lazy<Mutex<HashSet<String>>> = Lazy::new(|| Mutex::new(HashSet::new()));
pub fn is_control_room(room: &str) -> bool { ROOMS.lock().unwrap().contains(room) }

pub struct Notification { pub text: String }

pub async fn prepare(content: &Value, room: &str, sender: &str, own: &str) -> Option<Notification> {
    let mut child = tokio::process::Command::new("python3")
        .arg("/opt/agentteams/scripts/lib/task_coordination.py")
        .stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::null())
        .kill_on_drop(true).spawn().ok()?;
    let mut stdin = child.stdin.take()?;
    let input = json!({"content": content, "room": room, "sender": sender, "own": own});
    stdin.write_all(input.to_string().as_bytes()).await.ok()?;
    drop(stdin);
    let result = tokio::time::timeout(std::time::Duration::from_secs(15), child.wait_with_output()).await.ok()?.ok()?;
    if !result.status.success() { return None; }
    let packet: Value = serde_json::from_slice(&result.stdout).ok()?;
    let text = packet.get("text")?.as_str()?.to_owned();
    ROOMS.lock().unwrap().insert(room.to_owned());
    Some(Notification { text })
}
