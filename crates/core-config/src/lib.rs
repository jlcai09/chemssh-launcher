//! Profile, LocalProxy, X11, and UI settings configuration (Phase 1).

pub const CRATE_NAME: &str = "core-config";

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn crate_marker() {
        assert_eq!(CRATE_NAME, "core-config");
    }
}
