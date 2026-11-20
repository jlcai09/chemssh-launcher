//! SSH client, PTY shell, local forwarding, reverse proxy tunnel, and X11 (Phase 2).

pub const CRATE_NAME: &str = "core-ssh";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn crate_marker() {
        assert_eq!(CRATE_NAME, "core-ssh");
    }
}
