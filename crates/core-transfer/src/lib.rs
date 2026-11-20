//! SFTP operations, transfer queue, resume, cross-server copy, and archive (Phase 3).

pub const CRATE_NAME: &str = "core-transfer";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn crate_marker() {
        assert_eq!(CRATE_NAME, "core-transfer");
    }
}
