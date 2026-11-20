//! Session lifecycle, local port conflict checks, health checks, and log buffers (Phase 1-2).

pub const CRATE_NAME: &str = "core-session";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn crate_marker() {
        assert_eq!(CRATE_NAME, "core-session");
    }
}
