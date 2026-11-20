use std::net::TcpListener;

use crate::SessionError;

pub fn is_port_available(host: &str, port: u16) -> bool {
    let addr = format_host_port(host, port);
    TcpListener::bind(&addr).is_ok()
}

pub fn check_port_available(host: &str, port: u16) -> Result<(), SessionError> {
    if is_port_available(host, port) {
        Ok(())
    } else {
        Err(SessionError::PortOccupied {
            host: host.to_string(),
            port,
        })
    }
}

pub fn suggest_free_ports(host: &str, start: u16, count: usize) -> Vec<u16> {
    if count == 0 || start == u16::MAX {
        return Vec::new();
    }
    let mut ports = Vec::with_capacity(count);
    for port in (start.saturating_add(1))..=u16::MAX {
        if is_port_available(host, port) {
            ports.push(port);
            if ports.len() >= count {
                break;
            }
        }
        if port == u16::MAX {
            break;
        }
    }
    ports
}

fn format_host_port(host: &str, port: u16) -> String {
    let trimmed = host.trim();
    if trimmed.contains(':') && !trimmed.starts_with('[') {
        format!("[{trimmed}]:{port}")
    } else {
        format!("{trimmed}:{port}")
    }
}
