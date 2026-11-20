//! OS Keyring and Argon2id/AES-GCM fallback vault storage (Phase 1).

pub const SERVICE_NAME: &str = "chemssh-launcher";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn service_name_matches_go_contract() {
        assert_eq!(SERVICE_NAME, "chemssh-launcher");
    }
}
