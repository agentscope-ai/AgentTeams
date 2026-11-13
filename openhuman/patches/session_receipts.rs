//! Save the accepted native Matrix input before model execution.
use serde::Serialize;
use sha2::{Digest, Sha256};
use std::path::Path;

pub(crate) fn checkpoint(
    workspace: &Path,
    room_id: &str,
    event_id: &str,
    session: &str,
    input: &impl Serialize,
) -> std::io::Result<()> {
    let directory = workspace.join(".agentteams/matrix-sessions");
    std::fs::create_dir_all(&directory)?;
    let key = format!("{:x}", Sha256::digest(format!("{room_id}\0{event_id}")));
    let path = directory.join(format!("{key}.json"));
    let temporary = directory.join(format!("{key}.tmp"));
    let snapshot = serde_json::json!({
        "session": session,
        "input": input,
        "receipts": [{"room_id": room_id, "event_id": event_id}],
    });
    std::fs::write(&temporary, serde_json::to_vec(&snapshot)?)?;
    std::fs::rename(temporary, path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn saves_native_input_and_exact_source_without_waiting_for_a_reply() {
        let root = std::env::temp_dir().join(format!("receipt-test-{}", std::process::id()));
        let input = serde_json::json!({"role": "user", "content": "hello"});
        checkpoint(&root, "!room:hs", "$input", "native-session", &input).unwrap();
        let path = std::fs::read_dir(root.join(".agentteams/matrix-sessions"))
            .unwrap()
            .next()
            .unwrap()
            .unwrap()
            .path();
        let saved: serde_json::Value =
            serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        assert_eq!(saved["input"], input);
        assert_eq!(saved["session"], "native-session");
        assert_eq!(saved["receipts"][0]["event_id"], "$input");
        std::fs::remove_dir_all(root).unwrap();
    }
}
