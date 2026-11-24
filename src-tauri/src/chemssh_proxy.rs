use std::convert::Infallible;
use std::io::Read;
use std::net::TcpListener;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use core_config::Profile;
use flate2::read::{DeflateDecoder, GzDecoder, ZlibDecoder};
use http::header::{
    ACCEPT, ACCEPT_ENCODING, CONNECTION, CONTENT_ENCODING, CONTENT_LENGTH, CONTENT_TYPE, COOKIE,
    HOST, LOCATION, REFERER, SET_COOKIE, TRANSFER_ENCODING, UPGRADE,
};
use http::{HeaderMap, HeaderName, HeaderValue, Method, Request, Response, StatusCode, Uri};
use http_body_util::combinators::BoxBody;
use http_body_util::{BodyExt, Empty, Full};
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper_util::rt::TokioIo;
use percent_encoding::percent_decode_str;
use rand::RngCore;
use serde::Serialize;
use subtle::ConstantTimeEq;
use tokio::io::copy_bidirectional;
use tokio::net::TcpStream;
use url::Url;

use crate::chemssh_bridge::{BridgeHttpError, ChemSshBridgeOpenRequest};
use crate::client_identity::ChemSshBridgeClientIdentity;
use crate::commands::sessions::{
    profile_proxy_prefix, profile_proxy_slug, ActiveSession,
};
use crate::state::AppState;

const LAUNCHER_API_TOKEN_COOKIE: &str = "chemssh_launcher_token";
const HEADER_LAUNCHER_TOKEN: HeaderName = HeaderName::from_static("x-chemssh-launcher-token");
const CHEMSSH_PROXY_MAX_RETRIES: usize = 3;
const CHEMSSH_PROXY_RETRY_BASE_MS: u64 = 200;
const MAX_BRIDGE_REQUEST_BODY_BYTES: usize = 1024 * 1024;
const MAX_PROXIED_HTML_BYTES: usize = 16 * 1024 * 1024;

type BoxError = Box<dyn std::error::Error + Send + Sync>;
type ProxyBody = BoxBody<Bytes, BoxError>;

// Self-healing guard copied from the Go proxy: a transient dynamic-import failure
// while ChemSSH finishes booting reloads the iframe once automatically.
const CHEMSSH_IMPORT_RELOADER_SCRIPT: &str = r#"<script>(function(){if(sessionStorage.getItem('__chemsshReloaded')){sessionStorage.removeItem('__chemsshReloaded');return;}var reloaded=false;function isImportError(ev){var msg=(ev&&ev.message)||'';if(/Failed to fetch dynamically imported module/.test(msg))return true;if(ev&&ev.error&&/Failed to fetch dynamically imported module/.test(ev.error.message||''))return true;return false;}window.addEventListener('error',function(ev){if(reloaded)return;if(!isImportError(ev))return;reloaded=true;try{sessionStorage.setItem('__chemsshReloaded','1');}catch(e){}try{window.location.reload();}catch(e){}},{capture:true});window.addEventListener('unhandledrejection',function(ev){if(reloaded)return;var msg=(ev&&ev.reason&&(ev.reason.message||ev.reason))||'';if(/Failed to fetch dynamically imported module/.test(String(msg))){reloaded=true;try{sessionStorage.setItem('__chemsshReloaded','1');}catch(e){}try{window.location.reload();}catch(e){}}},{capture:true});})();</script>"#;

const CHEMSSH_LOADING_HTML: &str = r#"<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<title>ChemSSH</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
html,body{height:100%;background:#f6f6f7;color:#1d1d1f}
body{display:flex;align-items:center;justify-content:center;
  font-family:-apple-system,BlinkMacSystemFont,"SF Pro Text","Segoe UI","PingFang SC","Microsoft YaHei",system-ui,sans-serif}
.wrap{display:inline-flex;align-items:center;gap:10px;user-select:none}
.spinner{width:16px;height:16px;border-radius:50%;border:1.5px solid rgba(29,29,31,.12);
  border-top-color:#6e6e73;animation:spin .75s linear infinite}
.label{font-size:12.5px;font-weight:500;color:#86868b}
@keyframes spin{to{transform:rotate(360deg)}}
</style>
</head>
<body>
<div class="wrap" role="status" aria-live="polite">
  <span class="spinner" aria-hidden="true"></span>
  <span class="label">ChemSSH</span>
</div>
</body>
</html>"#;

pub fn generate_api_token() -> String {
    let mut bytes = [0u8; 32];
    rand::rng().fill_bytes(&mut bytes);
    hex::encode(bytes)
}

pub fn bind_proxy_listener() -> Result<(TcpListener, String), String> {
    let listener =
        TcpListener::bind(("127.0.0.1", 0)).map_err(|e| format!("bind ChemSSH proxy: {e}"))?;
    listener
        .set_nonblocking(true)
        .map_err(|e| format!("configure ChemSSH proxy listener: {e}"))?;
    let port = listener
        .local_addr()
        .map_err(|e| format!("read ChemSSH proxy port: {e}"))?
        .port();
    Ok((listener, format!("http://127.0.0.1:{port}")))
}

pub fn spawn_proxy_server(std_listener: TcpListener, state: Arc<AppState>) {
    tauri::async_runtime::spawn(async move {
        if let Err(err) = serve_proxy_listener(std_listener, state).await {
            eprintln!("chemssh proxy server stopped: {err}");
        }
    });
}

pub async fn serve_proxy_listener(
    std_listener: TcpListener,
    state: Arc<AppState>,
) -> Result<(), String> {
    let listener = tokio::net::TcpListener::from_std(std_listener)
        .map_err(|e| format!("convert proxy listener: {e}"))?;
    loop {
        let (stream, _) = listener
            .accept()
            .await
            .map_err(|e| format!("accept proxy connection: {e}"))?;
        let conn_state = Arc::clone(&state);
        tokio::spawn(async move {
            let io = TokioIo::new(stream);
            let service = service_fn(move |req: Request<Incoming>| {
                let s = Arc::clone(&conn_state);
                async move { Ok::<_, Infallible>(handle_request(req, s).await) }
            });
            let _ = hyper::server::conn::http1::Builder::new()
                .preserve_header_case(true)
                .serve_connection(io, service)
                .with_upgrades()
                .await;
        });
    }
}

async fn handle_request(req: Request<Incoming>, state: Arc<AppState>) -> Response<ProxyBody> {
    let path = req.uri().path().to_string();
    match path.as_str() {
        "/api/file-icon" => handle_file_icon(&req, &state),
        "/api/chemssh-bridge/capabilities" => handle_bridge_capabilities(&req, &state).await,
        "/api/chemssh-bridge/client-identity" => handle_bridge_client_identity(&req, &state),
        "/api/chemssh-bridge/open" => handle_bridge_open(req, &state, false).await,
        "/api/chemssh-bridge/open-text" => handle_bridge_open(req, &state, true).await,
        "/api/chemssh-bridge/open-sync-events" => {
            handle_bridge_open_sync_events(&req, &state).await
        }
        _ => proxy_to_chemssh(req, &state).await,
    }
}

fn empty_body() -> ProxyBody {
    Empty::<Bytes>::new()
        .map_err(|never| match never {})
        .boxed()
}

fn full_body(bytes: impl Into<Bytes>) -> ProxyBody {
    Full::new(bytes.into())
        .map_err(|never| match never {})
        .boxed()
}

fn json_response<T: Serialize>(
    status: StatusCode,
    payload: &T,
    cookie_token: Option<&str>,
) -> Response<ProxyBody> {
    let body = serde_json::to_vec(payload).unwrap_or_else(|_| b"{}".to_vec());
    let mut builder = Response::builder()
        .status(status)
        .header(CONTENT_TYPE, "application/json; charset=utf-8");
    if let Some(tok) = cookie_token {
        builder = builder.header(SET_COOKIE, token_cookie_header_value(tok));
    }
    builder
        .body(full_body(body))
        .unwrap_or_else(|_| Response::new(empty_body()))
}

fn json_error(status: u16, message: impl Into<String>, cookie_token: Option<&str>) -> Response<ProxyBody> {
    let code = StatusCode::from_u16(status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let payload = serde_json::json!({ "error": message.into() });
    let mut resp = json_response(code, &payload, cookie_token);
    set_no_store(resp.headers_mut());
    resp
}

fn token_cookie_header_value(api_token: &str) -> String {
    format!("{LAUNCHER_API_TOKEN_COOKIE}={api_token}; Path=/; SameSite=Lax")
}

fn set_no_store(headers: &mut HeaderMap) {
    headers.insert(
        HeaderName::from_static("cache-control"),
        HeaderValue::from_static("no-store, no-cache, must-revalidate, max-age=0"),
    );
    headers.insert(HeaderName::from_static("pragma"), HeaderValue::from_static("no-cache"));
    headers.insert(HeaderName::from_static("expires"), HeaderValue::from_static("0"));
}

fn query_param(uri: &Uri, key: &str) -> Option<String> {
    let query = uri.query()?;
    for (k, v) in url::form_urlencoded::parse(query.as_bytes()) {
        if k == key {
            return Some(v.into_owned());
        }
    }
    None
}

fn header_str<'a>(headers: &'a HeaderMap, name: &HeaderName) -> Option<&'a str> {
    headers.get(name).and_then(|v| v.to_str().ok())
}

async fn resolve_session(req: &Request<Incoming>, state: &AppState) -> Option<Arc<ActiveSession>> {
    let query_pid = query_param(req.uri(), "profile_id");
    let referer = header_str(req.headers(), &REFERER);
    state
        .sessions
        .find_for_proxy_request(query_pid.as_deref(), req.uri().path(), referer)
        .await
}

fn handle_file_icon(req: &Request<Incoming>, state: &AppState) -> Response<ProxyBody> {
    if req.method() != Method::GET {
        return json_error(405, "method not allowed", Some(&state.api_token));
    }
    let name = query_param(req.uri(), "name").unwrap_or_default();
    let is_dir = query_param(req.uri(), "is_dir")
        .map(|v| {
            let trimmed = v.trim();
            trimmed == "1" || trimmed.eq_ignore_ascii_case("true")
        })
        .unwrap_or(false);
    let size = query_param(req.uri(), "size")
        .and_then(|v| v.trim().parse::<u32>().ok())
        .unwrap_or(16);

    let icon = state.file_icons.get_icon(&name, is_dir, size);
    Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_TYPE, icon.content_type)
        .header("cache-control", "public, max-age=86400")
        .body(full_body(icon.bytes.to_vec()))
        .unwrap_or_else(|_| Response::new(empty_body()))
}

async fn handle_bridge_capabilities(
    req: &Request<Incoming>,
    state: &AppState,
) -> Response<ProxyBody> {
    if req.method() != Method::GET {
        return json_error(405, "method not allowed", Some(&state.api_token));
    }
    let session = resolve_session(req, state).await;
    let caps = state
        .chemssh_bridge
        .capabilities(state, session.as_ref())
        .await;
    json_response(StatusCode::OK, &caps, Some(&state.api_token))
}

fn handle_bridge_client_identity(
    req: &Request<Incoming>,
    state: &AppState,
) -> Response<ProxyBody> {
    if req.method() != Method::GET {
        return json_error(405, "method not allowed", Some(&state.api_token));
    }
    match state.client_identity.ensure() {
        Ok(ident) => {
            let payload = ChemSshBridgeClientIdentity::from(&ident);
            json_response(StatusCode::OK, &payload, Some(&state.api_token))
        }
        Err(err) => json_error(500, err, Some(&state.api_token)),
    }
}

async fn handle_bridge_open(
    req: Request<Incoming>,
    state: &AppState,
    text_mode: bool,
) -> Response<ProxyBody> {
    if req.method() != Method::POST {
        return json_error(405, "method not allowed", Some(&state.api_token));
    }
    if let Err(err) = validate_bridge_write_request(req.headers(), &state.api_token) {
        return json_error(403, err, Some(&state.api_token));
    }
    let session = resolve_session(&req, state).await;
    let body_bytes = match read_limited_body(req.into_body(), MAX_BRIDGE_REQUEST_BODY_BYTES).await {
        Ok(b) => b,
        Err(err) => return json_error(400, err, Some(&state.api_token)),
    };
    let parsed: ChemSshBridgeOpenRequest = match serde_json::from_slice(&body_bytes) {
        Ok(v) => v,
        Err(err) => return json_error(400, format!("invalid JSON: {err}"), Some(&state.api_token)),
    };
    match state
        .chemssh_bridge
        .open_file(state, session.as_ref(), &parsed.path, text_mode)
        .await
    {
        Ok(resp) => json_response(StatusCode::OK, &resp, Some(&state.api_token)),
        Err(BridgeHttpError { status, message }) => {
            json_error(status, message, Some(&state.api_token))
        }
    }
}

async fn handle_bridge_open_sync_events(
    req: &Request<Incoming>,
    state: &AppState,
) -> Response<ProxyBody> {
    if req.method() != Method::GET {
        return json_error(405, "method not allowed", Some(&state.api_token));
    }
    let after = query_param(req.uri(), "after")
        .and_then(|v| v.trim().parse::<u64>().ok())
        .unwrap_or(0);
    let session = resolve_session(req, state).await;
    let events = state
        .chemssh_bridge
        .open_sync_events(state, session.as_ref(), after)
        .await;
    let payload = serde_json::json!({ "events": events });
    json_response(StatusCode::OK, &payload, Some(&state.api_token))
}

async fn read_limited_body(body: Incoming, limit: usize) -> Result<Vec<u8>, String> {
    let mut out = Vec::new();
    let mut stream = body;
    while let Some(frame_res) = stream.frame().await {
        let frame = frame_res.map_err(|e| format!("read request body: {e}"))?;
        if let Ok(data) = frame.into_data() {
            if out.len() + data.len() > limit {
                return Err("request body too large".into());
            }
            out.extend_from_slice(&data);
        }
    }
    Ok(out)
}

pub fn validate_bridge_write_request(headers: &HeaderMap, api_token: &str) -> Result<(), String> {
    if let Some(origin) = header_str(headers, &ORIGIN_HEADER) {
        let trimmed = origin.trim();
        if !trimmed.is_empty() {
            let parsed_origin =
                Url::parse(trimmed).map_err(|_| "invalid launcher API origin".to_string())?;
            let origin_host = parsed_origin
                .host_str()
                .ok_or_else(|| "invalid launcher API origin".to_string())?;
            let host_header = header_str(headers, &HOST)
                .map(str::trim)
                .filter(|s| !s.is_empty())
                .ok_or_else(|| "invalid launcher API origin".to_string())?;
            let parsed_host = Url::parse(&format!("http://{host_header}"))
                .map_err(|_| "invalid launcher API origin".to_string())?;
            let req_host = parsed_host.host_str().unwrap_or("");
            if !origin_host.eq_ignore_ascii_case(req_host)
                || parsed_origin.port() != parsed_host.port()
            {
                return Err("invalid launcher API origin".into());
            }
        }
    }

    if let Some(fetch_site) = headers
        .get("sec-fetch-site")
        .and_then(|v| v.to_str().ok())
    {
        match fetch_site.trim() {
            "" | "same-origin" | "none" => {}
            _ => return Err("cross-site launcher API request rejected".into()),
        }
    }

    let header_tok = header_str(headers, &HEADER_LAUNCHER_TOKEN).unwrap_or("");
    if api_token.is_empty() || header_tok.is_empty() {
        return Err("missing launcher API token".into());
    }
    if !constant_time_eq(header_tok.as_bytes(), api_token.as_bytes()) {
        return Err("invalid launcher API token".into());
    }

    // If the launcher token cookie is present, require it to match too; do not
    // fail if WebView2 suppresses cookies inside an embedded iframe.
    if let Some(cookie_val) = extract_cookie(headers, LAUNCHER_API_TOKEN_COOKIE) {
        if !constant_time_eq(cookie_val.as_bytes(), api_token.as_bytes()) {
            return Err("invalid launcher API token".into());
        }
    }
    Ok(())
}

const ORIGIN_HEADER: HeaderName = HeaderName::from_static("origin");

fn extract_cookie<'a>(headers: &'a HeaderMap, name: &str) -> Option<&'a str> {
    for raw in headers.get_all(COOKIE) {
        let Ok(s) = raw.to_str() else { continue };
        for part in s.split(';') {
            let trimmed = part.trim();
            if let Some((k, v)) = trimmed.split_once('=') {
                if k.trim() == name {
                    return Some(v.trim());
                }
            }
        }
    }
    None
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && bool::from(a.ct_eq(b))
}

async fn proxy_to_chemssh(req: Request<Incoming>, state: &AppState) -> Response<ProxyBody> {
    let Some(session) = resolve_session(&req, state).await else {
        return json_error(503, "chemssh service is not available; start forwarding first", None);
    };
    if !session.is_forwarding().await {
        return json_error(503, "chemssh service is not available; start forwarding first", None);
    }

    let html_wanted = wants_html(req.headers());
    if html_wanted && !session.is_ready().await {
        let refresh = req
            .uri()
            .path_and_query()
            .map(|pq| pq.as_str())
            .unwrap_or("/");
        return loading_page_response(refresh);
    }

    let profile = session.profile().clone();
    let target_host = profile.local_host.clone();
    let target_port = profile.local_port;
    let connect_addr = format_host_port(&target_host, target_port);
    let client_id = state.client_identity.client_id();
    let sec_token = session.security_token().map(str::to_string);
    let asset_prefix = profile_proxy_prefix(&profile);

    let is_upgrade = req.headers().contains_key(UPGRADE);
    let target_path = chemssh_proxy_target_path(req.uri().path(), &profile);
    let upstream_path_and_query = rewrite_upstream_path_and_query(
        &target_path,
        req.uri().query(),
        &client_id,
        sec_token.as_deref(),
    );

    if is_upgrade {
        return handle_websocket_upgrade(
            req,
            &connect_addr,
            &upstream_path_and_query,
            &client_id,
            sec_token.as_deref(),
        )
        .await;
    }

    let (parts, body) = req.into_parts();
    let can_retry_after_send = matches!(parts.method, Method::GET | Method::HEAD);
    let max_attempts = if !can_retry_after_send {
        1
    } else if html_wanted {
        CHEMSSH_PROXY_MAX_RETRIES
    } else {
        2
    };

    let mut pending_body = if can_retry_after_send {
        None
    } else {
        Some(body)
    };
    let mut last_err = String::new();

    for attempt in 0..max_attempts {
        if attempt > 0 {
            let delay = CHEMSSH_PROXY_RETRY_BASE_MS * (1u64 << (attempt - 1));
            tokio::time::sleep(Duration::from_millis(delay)).await;
        }

        // TCP connect retries before sending any body even for POST/PUT.
        let stream = match connect_upstream_with_retry(&connect_addr, can_retry_after_send).await {
            Ok(s) => s,
            Err(err) => {
                last_err = err;
                if can_retry_after_send && is_retriable_msg(&last_err) {
                    continue;
                }
                if html_wanted && is_retriable_msg(&last_err) {
                    let refresh = parts
                        .uri
                        .path_and_query()
                        .map(|pq| pq.as_str())
                        .unwrap_or("/");
                    return loading_page_response(refresh);
                }
                return json_error(502, last_err, None);
            }
        };

        let upstream_req = build_upstream_request(
            &parts.method,
            &parts.headers,
            &upstream_path_and_query,
            &connect_addr,
            &client_id,
            sec_token.as_deref(),
            html_wanted,
            false,
            match pending_body.take() {
                Some(b) => b.map_err(|e| -> BoxError { Box::new(e) }).boxed(),
                None => empty_body(),
            },
        );

        let io = TokioIo::new(stream);
        let handshake = hyper::client::conn::http1::Builder::new()
            .preserve_header_case(true)
            .handshake(io)
            .await;

        let (mut sender, conn) = match handshake {
            Ok(pair) => pair,
            Err(err) => {
                last_err = err.to_string();
                if can_retry_after_send && is_retriable_msg(&last_err) {
                    continue;
                }
                return json_error(502, format!("proxy ChemSSH handshake failed: {last_err}"), None);
            }
        };

        tokio::spawn(async move {
            let _ = conn.await;
        });

        match sender.send_request(upstream_req).await {
            Ok(upstream_resp) => {
                return transform_upstream_response(
                    upstream_resp,
                    &state.api_token,
                    &asset_prefix,
                    &target_host,
                    target_port,
                    &profile,
                )
                .await;
            }
            Err(err) => {
                last_err = err.to_string();
                if can_retry_after_send && is_retriable_msg(&last_err) {
                    continue;
                }
                if html_wanted && is_retriable_msg(&last_err) {
                    let refresh = parts
                        .uri
                        .path_and_query()
                        .map(|pq| pq.as_str())
                        .unwrap_or("/");
                    return loading_page_response(refresh);
                }
                return json_error(502, format!("proxy ChemSSH request failed: {last_err}"), None);
            }
        }
    }

    if html_wanted && is_retriable_msg(&last_err) {
        let refresh = parts
            .uri
            .path_and_query()
            .map(|pq| pq.as_str())
            .unwrap_or("/");
        return loading_page_response(refresh);
    }
    json_error(
        502,
        format!("proxy ChemSSH request failed after {max_attempts} retries: {last_err}"),
        None,
    )
}

async fn connect_upstream_with_retry(
    addr: &str,
    single_attempt: bool,
) -> Result<TcpStream, String> {
    let attempts = if single_attempt { 1 } else { CHEMSSH_PROXY_MAX_RETRIES };
    let mut last_err = String::new();
    for attempt in 0..attempts {
        if attempt > 0 {
            let delay = CHEMSSH_PROXY_RETRY_BASE_MS * (1u64 << (attempt - 1));
            tokio::time::sleep(Duration::from_millis(delay)).await;
        }
        match TcpStream::connect(addr).await {
            Ok(stream) => return Ok(stream),
            Err(err) => {
                last_err = err.to_string();
                if !is_retriable_msg(&last_err) {
                    break;
                }
            }
        }
    }
    Err(last_err)
}

async fn handle_websocket_upgrade(
    mut req: Request<Incoming>,
    connect_addr: &str,
    upstream_path_and_query: &str,
    client_id: &str,
    sec_token: Option<&str>,
) -> Response<ProxyBody> {
    let stream = match connect_upstream_with_retry(connect_addr, false).await {
        Ok(s) => s,
        Err(err) => return json_error(502, format!("connect ChemSSH websocket: {err}"), None),
    };
    let client_upgrade = hyper::upgrade::on(&mut req);
    let upstream_req = build_upstream_request(
        req.method(),
        req.headers(),
        upstream_path_and_query,
        connect_addr,
        client_id,
        sec_token,
        false,
        true,
        empty_body(),
    );

    let io = TokioIo::new(stream);
    let (mut sender, conn) = match hyper::client::conn::http1::Builder::new()
        .preserve_header_case(true)
        .handshake(io)
        .await
    {
        Ok(pair) => pair,
        Err(err) => return json_error(502, format!("ChemSSH websocket handshake: {err}"), None),
    };
    tokio::spawn(async move {
        let _ = conn.with_upgrades().await;
    });

    let mut upstream_resp = match sender.send_request(upstream_req).await {
        Ok(resp) => resp,
        Err(err) => return json_error(502, format!("ChemSSH websocket upstream: {err}"), None),
    };

    if upstream_resp.status() == StatusCode::SWITCHING_PROTOCOLS {
        let upstream_upgrade = hyper::upgrade::on(&mut upstream_resp);
        tokio::spawn(async move {
            if let (Ok(client_io), Ok(server_io)) =
                tokio::join!(client_upgrade, upstream_upgrade)
            {
                let mut c = TokioIo::new(client_io);
                let mut s = TokioIo::new(server_io);
                let _ = copy_bidirectional(&mut c, &mut s).await;
            }
        });
    }

    let (mut resp_parts, resp_body) = upstream_resp.into_parts();
    if resp_parts.status != StatusCode::SWITCHING_PROTOCOLS {
        strip_hop_by_hop_headers(&mut resp_parts.headers, false);
    }
    Response::from_parts(
        resp_parts,
        resp_body.map_err(|e| -> BoxError { Box::new(e) }).boxed(),
    )
}

#[allow(clippy::too_many_arguments)]
fn build_upstream_request(
    method: &Method,
    orig_headers: &HeaderMap,
    path_and_query: &str,
    connect_addr: &str,
    client_id: &str,
    sec_token: Option<&str>,
    html_wanted: bool,
    keep_upgrade: bool,
    body: ProxyBody,
) -> Request<ProxyBody> {
    let mut headers = orig_headers.clone();
    strip_hop_by_hop_headers(&mut headers, keep_upgrade);

    if let Ok(hv) = HeaderValue::from_str(connect_addr) {
        headers.insert(HOST, hv.clone());
        headers.insert(HeaderName::from_static("x-forwarded-host"), hv);
    }
    headers.insert(
        HeaderName::from_static("x-forwarded-proto"),
        HeaderValue::from_static("http"),
    );

    if !keep_upgrade && html_wanted {
        headers.insert(ACCEPT_ENCODING, HeaderValue::from_static("gzip"));
    }

    if !client_id.is_empty() {
        if let Ok(hv) = HeaderValue::from_str(client_id) {
            headers.insert(HeaderName::from_static("x-chemssh-client-id"), hv);
        }
    }
    if let Some(tok) = sec_token {
        let clean = tok.replace(['\r', '\n', '\0'], "");
        if !clean.is_empty() {
            if let Ok(hv) = HeaderValue::from_str(&clean) {
                headers.insert(HeaderName::from_static("x-chemssh-token"), hv);
            }
        }
    }

    let uri: Uri = path_and_query
        .parse()
        .unwrap_or_else(|_| Uri::from_static("/"));
    let mut req = Request::builder().method(method).uri(uri);
    if let Some(h) = req.headers_mut() {
        *h = headers;
    }
    req.body(body).unwrap_or_else(|_| Request::new(empty_body()))
}

async fn transform_upstream_response(
    resp: Response<Incoming>,
    api_token: &str,
    asset_prefix: &str,
    target_host: &str,
    target_port: u16,
    profile: &Profile,
) -> Response<ProxyBody> {
    let (mut parts, body) = resp.into_parts();
    strip_hop_by_hop_headers(&mut parts.headers, false);

    if let Some(loc) = parts.headers.get(LOCATION).and_then(|v| v.to_str().ok()) {
        if let Some(rewritten) =
            rewrite_chemssh_location_header(loc, target_host, target_port, profile)
        {
            if let Ok(hv) = HeaderValue::from_str(&rewritten) {
                parts.headers.insert(LOCATION, hv);
            }
        }
    }

    let is_html = parts
        .headers
        .get(CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .map(|v| v.to_ascii_lowercase().contains("text/html"))
        .unwrap_or(false);

    if !is_html {
        return Response::from_parts(
            parts,
            body.map_err(|e| -> BoxError { Box::new(e) }).boxed(),
        );
    }

    let raw = match read_limited_body(body, MAX_PROXIED_HTML_BYTES).await {
        Ok(b) => b,
        Err(err) => return json_error(502, err, None),
    };

    if let Ok(hv) = HeaderValue::from_str(&token_cookie_header_value(api_token)) {
        parts.headers.append(SET_COOKIE, hv);
    }

    let encoding = parts
        .headers
        .get(CONTENT_ENCODING)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    let Ok(decoded) = decode_html_body(&raw, encoding) else {
        // Unsupported encoding (e.g. brotli): keep original compressed bytes intact.
        return Response::from_parts(parts, full_body(raw));
    };

    let rewritten = rewrite_chemssh_asset_urls(&decoded, asset_prefix);
    let snippet = format!(
        "{CHEMSSH_IMPORT_RELOADER_SCRIPT}{}",
        chemssh_launcher_token_script(api_token)
    );
    let injected = inject_after_head_open(&rewritten, snippet.as_bytes());

    parts.headers.remove(CONTENT_ENCODING);
    parts.headers.remove(TRANSFER_ENCODING);
    if let Ok(hv) = HeaderValue::from_str(&injected.len().to_string()) {
        parts.headers.insert(CONTENT_LENGTH, hv);
    }

    Response::from_parts(parts, full_body(injected))
}

fn strip_hop_by_hop_headers(headers: &mut HeaderMap, keep_upgrade: bool) {
    let extra: Vec<HeaderName> = headers
        .get_all(CONNECTION)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|s| s.split(','))
        .filter_map(|tok| HeaderName::from_bytes(tok.trim().as_bytes()).ok())
        .collect();

    if !keep_upgrade {
        headers.remove(CONNECTION);
        headers.remove(UPGRADE);
        for name in extra {
            headers.remove(&name);
        }
    }
    headers.remove(HeaderName::from_static("proxy-connection"));
    headers.remove(HeaderName::from_static("keep-alive"));
    headers.remove(HeaderName::from_static("proxy-authenticate"));
    headers.remove(HeaderName::from_static("proxy-authorization"));
    headers.remove(HeaderName::from_static("te"));
    headers.remove(HeaderName::from_static("trailer"));
    headers.remove(TRANSFER_ENCODING);
}

fn wants_html(headers: &HeaderMap) -> bool {
    header_str(headers, &ACCEPT)
        .map(|v| v.contains("text/html"))
        .unwrap_or(false)
}

fn loading_page_response(refresh_uri: &str) -> Response<ProxyBody> {
    let safe_refresh = refresh_uri.replace(['\r', '\n'], "");
    let mut builder = Response::builder()
        .status(StatusCode::SERVICE_UNAVAILABLE)
        .header(CONTENT_TYPE, "text/html; charset=utf-8")
        .header("refresh", format!("2;url={safe_refresh}"));
    if let Some(h) = builder.headers_mut() {
        set_no_store(h);
    }
    builder
        .body(full_body(CHEMSSH_LOADING_HTML))
        .unwrap_or_else(|_| Response::new(empty_body()))
}

fn is_retriable_msg(msg: &str) -> bool {
    let lower = msg.to_ascii_lowercase();
    lower.contains("connection refused")
        || lower.contains("forcibly closed")
        || lower.contains("connection reset")
        || lower.contains("wsarecv")
        || lower.contains("wsasend")
        || lower.contains("broken pipe")
        || lower.contains("unexpected eof")
        || lower.contains("eof")
        || lower.contains("timed out")
        || lower.contains("timeout")
}

fn format_host_port(host: &str, port: u16) -> String {
    let trimmed = host.trim();
    if trimmed.contains(':') && !trimmed.starts_with('[') {
        format!("[{trimmed}]:{port}")
    } else {
        format!("{trimmed}:{port}")
    }
}

pub fn chemssh_proxy_target_path(path: &str, profile: &Profile) -> String {
    let slug = profile_proxy_slug(profile);
    let trimmed = path.trim_start_matches('/');
    if let Some((seg0, rest)) = trimmed.split_once('/') {
        let decoded_seg0 = percent_decode_str(seg0)
            .decode_utf8()
            .map(|c| c.into_owned())
            .unwrap_or_else(|_| seg0.to_string());
        if decoded_seg0.eq_ignore_ascii_case(&slug) || decoded_seg0 == profile.id {
            if rest == "chemssh" || rest == "chemssh/" {
                return "/".into();
            }
            if let Some(tail) = rest.strip_prefix("chemssh/") {
                return if tail.is_empty() {
                    "/".into()
                } else {
                    format!("/{tail}")
                };
            }
            return if rest.is_empty() {
                "/".into()
            } else {
                format!("/{rest}")
            };
        }
    }

    if path == "/chemssh" || path == "/chemssh/" {
        "/".into()
    } else if let Some(tail) = path.strip_prefix("/chemssh/") {
        if tail.is_empty() {
            "/".into()
        } else {
            format!("/{tail}")
        }
    } else if path.is_empty() {
        "/".into()
    } else {
        path.to_string()
    }
}

pub fn is_terminal_websocket_path(path: &str) -> bool {
    path == "/api/terminal/ws" || path.starts_with("/api/terminal/ws/")
}

pub fn rewrite_upstream_path_and_query(
    target_path: &str,
    orig_query: Option<&str>,
    client_id: &str,
    security_token: Option<&str>,
) -> String {
    let is_ws = is_terminal_websocket_path(target_path);
    let mut ser = url::form_urlencoded::Serializer::new(String::new());
    if let Some(q) = orig_query {
        for (k, v) in url::form_urlencoded::parse(q.as_bytes()) {
            if k == "profile_id" || k == "launcher_open_seq" {
                continue;
            }
            if is_ws && !client_id.is_empty() && k == "client_id" {
                continue;
            }
            if is_ws && security_token.is_some() && k == "token" {
                continue;
            }
            ser.append_pair(&k, &v);
        }
    }
    if is_ws {
        if !client_id.is_empty() {
            ser.append_pair("client_id", client_id);
        }
        if let Some(tok) = security_token {
            let clean = tok.replace(['\r', '\n', '\0'], "");
            if !clean.is_empty() {
                ser.append_pair("token", &clean);
            }
        }
    }
    let query_str = ser.finish();
    if query_str.is_empty() {
        target_path.to_string()
    } else {
        format!("{target_path}?{query_str}")
    }
}

pub fn chemssh_launcher_token_script(api_token: &str) -> String {
    let fallback_json = serde_json::to_string(api_token).unwrap_or_else(|_| "\"\"".to_string());
    format!(
        r#"<script>(function(){{
var C='chemssh_launcher_token',H='X-ChemSSH-Launcher-Token',P='/api/chemssh-bridge/',F={fallback_json};
function token(){{var parts=document.cookie?document.cookie.split(';'):[];for(var i=0;i<parts.length;i++){{var item=parts[i].trim();if(item.indexOf(C+'=')===0){{return decodeURIComponent(item.slice(C.length+1));}}}}return F||'';}}
function origin(){{try{{return window.location.origin||(window.location.protocol+'//'+window.location.host);}}catch(e){{return '';}}}}
function inScope(url){{try{{var target=new URL(url,window.location.href);return target.origin===origin()&&target.pathname.indexOf(P)===0;}}catch(e){{return false;}}}}
var originalFetch=window.fetch;
if(typeof originalFetch!=='function'){{return;}}
window.fetch=function(input,init){{
try{{
var url=typeof input==='string'?input:(input&&input.url)||'';
var value=token();
if(value&&inScope(url)){{
var headers=new Headers((init&&init.headers)||(input&&input.headers)||undefined);
if(!headers.has(H)){{headers.set(H,value);var next=Object.assign({{}},init||{{}});next.headers=headers;return originalFetch.call(this,input,next);}}
}}
}}catch(e){{}}
return originalFetch.call(this,input,init);
}};
}})();</script>"#
    )
}

pub fn rewrite_chemssh_asset_urls(body: &[u8], asset_prefix: &str) -> Vec<u8> {
    if asset_prefix.is_empty() {
        return body.to_vec();
    }
    let double_from = b"\"/assets/";
    let double_to = format!("\"{asset_prefix}/assets/");
    let single_from = b"'/assets/";
    let single_to = format!("'{asset_prefix}/assets/");
    let step1 = replace_bytes(body, double_from, double_to.as_bytes());
    replace_bytes(&step1, single_from, single_to.as_bytes())
}

fn replace_bytes(haystack: &[u8], needle: &[u8], replacement: &[u8]) -> Vec<u8> {
    if needle.is_empty() {
        return haystack.to_vec();
    }
    let mut out = Vec::with_capacity(haystack.len());
    let mut i = 0;
    while i < haystack.len() {
        if haystack[i..].starts_with(needle) {
            out.extend_from_slice(replacement);
            i += needle.len();
        } else {
            out.push(haystack[i]);
            i += 1;
        }
    }
    out
}

pub fn inject_after_head_open(body: &[u8], snippet: &[u8]) -> Vec<u8> {
    let lower: Vec<u8> = body.iter().map(|b| b.to_ascii_lowercase()).collect();
    if let Some(head_idx) = lower.windows(5).position(|w| w == b"<head") {
        if let Some(close_rel) = body[head_idx..].iter().position(|&b| b == b'>') {
            let insert_at = head_idx + close_rel + 1;
            let mut out = Vec::with_capacity(body.len() + snippet.len());
            out.extend_from_slice(&body[..insert_at]);
            out.extend_from_slice(snippet);
            out.extend_from_slice(&body[insert_at..]);
            return out;
        }
    }
    let mut out = Vec::with_capacity(body.len() + snippet.len());
    out.extend_from_slice(snippet);
    out.extend_from_slice(body);
    out
}

pub fn decode_html_body(raw: &[u8], encoding: &str) -> Result<Vec<u8>, String> {
    let norm = encoding.trim().to_ascii_lowercase();
    match norm.as_str() {
        "" | "identity" => Ok(raw.to_vec()),
        "gzip" => {
            let mut dec = GzDecoder::new(raw);
            let mut out = Vec::new();
            dec.read_to_end(&mut out)
                .map_err(|e| format!("decode gzip HTML: {e}"))?;
            Ok(out)
        }
        "deflate" => {
            let mut zdec = ZlibDecoder::new(raw);
            let mut out = Vec::new();
            if zdec.read_to_end(&mut out).is_ok() {
                return Ok(out);
            }
            out.clear();
            let mut rdec = DeflateDecoder::new(raw);
            rdec.read_to_end(&mut out)
                .map_err(|e| format!("decode deflate HTML: {e}"))?;
            Ok(out)
        }
        other => Err(format!("unsupported content encoding: {other}")),
    }
}

pub fn rewrite_chemssh_location_header(
    location: &str,
    target_host: &str,
    target_port: u16,
    profile: &Profile,
) -> Option<String> {
    let prefix = profile_proxy_prefix(profile);
    if let Ok(parsed) = Url::parse(location) {
        let parsed_host = parsed.host_str().unwrap_or("");
        let parsed_port = parsed.port_or_known_default();
        if !parsed_host.eq_ignore_ascii_case(target_host) || parsed_port != Some(target_port) {
            return None;
        }
        let mut out = if parsed.path() == "/" {
            prefix
        } else if parsed.path().starts_with('/') && !parsed.path().starts_with(&prefix) {
            format!("{prefix}{}", parsed.path())
        } else {
            parsed.path().to_string()
        };
        if let Some(q) = parsed.query() {
            out.push('?');
            out.push_str(q);
        }
        if let Some(f) = parsed.fragment() {
            out.push('#');
            out.push_str(f);
        }
        return Some(out);
    }

    // Relative URL reference starting with '/'.
    if location.starts_with("//") {
        return None;
    }
    let (path_part, suffix) = match location.find(['?', '#']) {
        Some(idx) => (&location[..idx], &location[idx..]),
        None => (location, ""),
    };
    let new_path = if path_part == "/" {
        prefix
    } else if path_part.starts_with('/') && !path_part.starts_with(&prefix) {
        format!("{prefix}{path_part}")
    } else {
        path_part.to_string()
    };
    Some(format!("{new_path}{suffix}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::commands::sessions::{session_proxy_url, ActiveSession, SessionBackend, SessionStatus, LocalProxyStatus};
    use core_secret::MemoryStore;
    use flate2::write::GzEncoder;
    use flate2::Compression;
    use std::io::Write;
    use std::sync::atomic::AtomicBool;
    use tempfile::TempDir;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::sync::Mutex;

    fn test_profile() -> Profile {
        let mut p = Profile::remote_defaults();
        p.id = "remote-1".into();
        p.name = "team box".into();
        p.local_host = "127.0.0.1".into();
        p.local_port = 8888;
        p
    }

    #[test]
    fn rewrites_target_paths_for_escaped_and_bare_prefixes() {
        let p = test_profile();
        assert_eq!(chemssh_proxy_target_path("/team%20box/chemssh", &p), "/");
        assert_eq!(
            chemssh_proxy_target_path("/team%20box/chemssh/assets/app.js", &p),
            "/assets/app.js"
        );
        assert_eq!(
            chemssh_proxy_target_path("/chemssh/api/system/identity", &p),
            "/api/system/identity"
        );
        assert_eq!(
            rewrite_chemssh_location_header("/assets/app.js", "127.0.0.1", 8888, &p).as_deref(),
            Some("/team%20box/chemssh/assets/app.js")
        );
    }

    #[test]
    fn rewrites_html_assets_and_injects_scripts_plain_and_gzip() {
        let body = br#"<!doctype html><html><head data-x="1"><script type="module" src="/assets/index.js"></script></head><body></body></html>"#;
        let scoped = rewrite_chemssh_asset_urls(body, "/team%20box/chemssh");
        let snippet = format!(
            "{CHEMSSH_IMPORT_RELOADER_SCRIPT}{}",
            chemssh_launcher_token_script("tok-123")
        );
        let injected = inject_after_head_open(&scoped, snippet.as_bytes());
        let text = String::from_utf8(injected).unwrap();
        assert!(text.contains(r#"src="/team%20box/chemssh/assets/index.js""#));
        assert!(text.contains("__chemsshReloaded"));
        assert!(text.contains("X-ChemSSH-Launcher-Token"));
        assert!(text.contains("tok-123"));

        let mut enc = GzEncoder::new(Vec::new(), Compression::default());
        enc.write_all(body).unwrap();
        let gz = enc.finish().unwrap();
        let decoded = decode_html_body(&gz, "gzip").unwrap();
        assert_eq!(decoded, body);
        assert!(decode_html_body(&[1, 2, 3], "br").is_err());
    }

    #[test]
    fn write_validation_enforces_token_origin_and_fetch_site() {
        let mut headers = HeaderMap::new();
        headers.insert(HOST, HeaderValue::from_static("127.0.0.1:4567"));
        assert!(validate_bridge_write_request(&headers, "secret-1").is_err());

        headers.insert(
            HeaderName::from_static("x-chemssh-launcher-token"),
            HeaderValue::from_static("wrong"),
        );
        assert!(validate_bridge_write_request(&headers, "secret-1").is_err());

        headers.insert(
            HeaderName::from_static("x-chemssh-launcher-token"),
            HeaderValue::from_static("secret-1"),
        );
        headers.insert(
            ORIGIN_HEADER,
            HeaderValue::from_static("http://evil.example:4567"),
        );
        assert!(validate_bridge_write_request(&headers, "secret-1").is_err());

        headers.insert(
            ORIGIN_HEADER,
            HeaderValue::from_static("http://127.0.0.1:4567"),
        );
        headers.insert(
            HeaderName::from_static("sec-fetch-site"),
            HeaderValue::from_static("cross-site"),
        );
        assert!(validate_bridge_write_request(&headers, "secret-1").is_err());

        headers.insert(
            HeaderName::from_static("sec-fetch-site"),
            HeaderValue::from_static("same-origin"),
        );
        assert!(validate_bridge_write_request(&headers, "secret-1").is_ok());

        // Cookie matching is enforced if present, and tolerated when absent.
        headers.insert(
            COOKIE,
            HeaderValue::from_static("chemssh_launcher_token=other"),
        );
        assert!(validate_bridge_write_request(&headers, "secret-1").is_err());
        headers.insert(
            COOKIE,
            HeaderValue::from_static("chemssh_launcher_token=secret-1"),
        );
        assert!(validate_bridge_write_request(&headers, "secret-1").is_ok());
    }

    #[tokio::test]
    async fn end_to_end_proxy_forwards_html_bridge_icons_and_websocket() {
        let upstream = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let upstream_addr = upstream.local_addr().unwrap();

        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = upstream.accept().await else {
                    break;
                };
                tokio::spawn(async move {
                    let mut buf = [0u8; 4096];
                    let Ok(n) = sock.read(&mut buf).await else {
                        return;
                    };
                    let req_text = String::from_utf8_lossy(&buf[..n]).into_owned();
                    let first_line = req_text.lines().next().unwrap_or("");

                    if first_line.starts_with("GET /api/terminal/ws") {
                        assert!(first_line.contains("client_id=client_launcher_"));
                        assert!(first_line.contains("token=sec-tok-9"));
                        assert!(req_text.contains("X-ChemSSH-Client-Id: client_launcher_") || req_text.contains("x-chemssh-client-id: client_launcher_"));
                        let resp = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n";
                        let _ = sock.write_all(resp.as_bytes()).await;
                        let mut ws_buf = [0u8; 64];
                        if let Ok(m) = sock.read(&mut ws_buf).await {
                            if m > 0 {
                                let _ = sock.write_all(&ws_buf[..m]).await;
                            }
                        }
                        return;
                    }

                    // HTML page response validating stripped launcher query & injected headers
                    assert!(!first_line.contains("profile_id="));
                    assert!(!first_line.contains("launcher_open_seq="));
                    let has_cid = req_text.to_ascii_lowercase().contains("x-chemssh-client-id: client_launcher_");
                    let has_tok = req_text.to_ascii_lowercase().contains("x-chemssh-token: sec-tok-9");
                    assert!(has_cid && has_tok, "missing injected headers:\n{req_text}");

                    let html = r#"<html><head><script src="/assets/app.js"></script></head><body>ok</body></html>"#;
                    let resp = format!(
                        "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\n\r\n{}",
                        html.len(),
                        html
                    );
                    let _ = sock.write_all(resp.as_bytes()).await;
                });
            }
        });

        let dir = TempDir::new().unwrap();
        let ws_root = dir.path().join("workspace");
        std::fs::create_dir_all(&ws_root).unwrap();
        let mut state = AppState::new_custom(
            dir.path().to_path_buf(),
            dir.path().join("profiles.json"),
            dir.path().join("settings.json"),
            dir.path().join("cache"),
            Arc::new(MemoryStore::new()),
        );
        let (listener, proxy_base) = bind_proxy_listener().unwrap();
        state.proxy_base_url = proxy_base.clone();
        let state = Arc::new(state);
        let serve_state = Arc::clone(&state);
        tokio::spawn(async move {
            let _ = serve_proxy_listener(listener, serve_state).await;
        });

        let mut profile = Profile::local_defaults();
        profile.id = "local-ws".into();
        profile.name = "Local Lab".into();
        profile.local_host = "127.0.0.1".into();
        profile.local_port = upstream_addr.port();

        let status = SessionStatus {
            profile_id: profile.id.clone(),
            name: profile.name.clone(),
            running: true,
            forwarding: true,
            forwarding_active: true,
            open_seq: 3,
            open_browser: true,
            phase: "ready".into(),
            url: session_proxy_url(&proxy_base, &profile, 3),
            local_url: profile.browser_url(),
            warnings: Vec::new(),
            local_proxy: LocalProxyStatus::default(),
            last_error: None,
        };
        let active = Arc::new(ActiveSession {
            profile_id: profile.id.clone(),
            profile_name: profile.name.clone(),
            status: Mutex::new(status),
            backend: SessionBackend::Local {
                profile: profile.clone(),
                child: Mutex::new(None),
                security_token: Some("sec-tok-9".into()),
            },
            stop_requested: AtomicBool::new(false),
            remote_pid: Mutex::new(123),
            workspace_root: Mutex::new(ws_root.to_string_lossy().into_owned()),
        });
        state
            .sessions
            .insert_active_for_test(profile.id.clone(), active)
            .await;

        let proxy_port: u16 = Url::parse(&proxy_base).unwrap().port().unwrap();
        let proxy_host = format!("127.0.0.1:{proxy_port}");

        // 1. HTML proxy request
        let html_resp = raw_http_request(
            &proxy_host,
            &format!(
                "GET /Local%20Lab/chemssh?profile_id=local-ws&launcher_open_seq=3 HTTP/1.1\r\nHost: {proxy_host}\r\nAccept: text/html\r\nConnection: close\r\n\r\n"
            ),
        )
        .await;
        assert!(html_resp.contains("200 OK"), "{html_resp}");
        assert!(html_resp.contains("/Local%20Lab/chemssh/assets/app.js"));
        assert!(html_resp.contains("X-ChemSSH-Launcher-Token"));
        assert!(
            html_resp
                .to_ascii_lowercase()
                .contains("set-cookie: chemssh_launcher_token="),
            "{html_resp}"
        );

        // 2. Capabilities + client identity + file icon
        let caps_resp = raw_http_request(
            &proxy_host,
            &format!(
                "GET /api/chemssh-bridge/capabilities HTTP/1.1\r\nHost: {proxy_host}\r\nConnection: close\r\n\r\n"
            ),
        )
        .await;
        assert!(caps_resp.contains(r#""enabled":true"#), "{caps_resp}");
        assert!(caps_resp.contains(r#""open_default":true"#), "{caps_resp}");

        let id_resp = raw_http_request(
            &proxy_host,
            &format!(
                "GET /api/chemssh-bridge/client-identity HTTP/1.1\r\nHost: {proxy_host}\r\nConnection: close\r\n\r\n"
            ),
        )
        .await;
        assert!(id_resp.contains("client_launcher_"), "{id_resp}");

        let icon_resp = raw_http_request(
            &proxy_host,
            &format!(
                "GET /api/file-icon?name=input.inp&is_dir=0&size=16 HTTP/1.1\r\nHost: {proxy_host}\r\nConnection: close\r\n\r\n"
            ),
        )
        .await;
        assert!(icon_resp.contains("200 OK"), "{icon_resp}");

        // 3. Bridge open write auth enforcement (403 without token; valid token reaches workspace check)
        let unauth = raw_http_request(
            &proxy_host,
            &format!(
                "POST /api/chemssh-bridge/open HTTP/1.1\r\nHost: {proxy_host}\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{{}}"
            ),
        )
        .await;
        assert!(unauth.contains("403 Forbidden"), "{unauth}");

        let outside_body = r#"{"path":"/outside-workspace-path"}"#;
        let authed_outside = raw_http_request(
            &proxy_host,
            &format!(
                "POST /api/chemssh-bridge/open HTTP/1.1\r\nHost: {proxy_host}\r\nOrigin: http://{proxy_host}\r\nX-ChemSSH-Launcher-Token: {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                state.api_token,
                outside_body.len(),
                outside_body
            ),
        )
        .await;
        assert!(
            authed_outside.contains("outside the active ChemSSH workspace"),
            "{authed_outside}"
        );

        // 4. WebSocket upgrade forwarding
        let mut ws_stream = TcpStream::connect(&proxy_host).await.unwrap();
        let ws_req = format!(
            "GET /Local%20Lab/chemssh/api/terminal/ws/s1?client_id=rand_1 HTTP/1.1\r\nHost: {proxy_host}\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
        );
        ws_stream.write_all(ws_req.as_bytes()).await.unwrap();
        let mut hs_buf = [0u8; 512];
        let hs_n = ws_stream.read(&mut hs_buf).await.unwrap();
        assert!(String::from_utf8_lossy(&hs_buf[..hs_n]).contains("101 Switching Protocols"));
        ws_stream.write_all(b"PING").await.unwrap();
        let mut echo = [0u8; 8];
        let en = ws_stream.read(&mut echo).await.unwrap();
        assert_eq!(&echo[..en], b"PING");
    }

    async fn raw_http_request(addr: &str, raw_req: &str) -> String {
        let mut sock = TcpStream::connect(addr).await.unwrap();
        sock.write_all(raw_req.as_bytes()).await.unwrap();
        let mut out = Vec::new();
        let _ = sock.read_to_end(&mut out).await;
        String::from_utf8_lossy(&out).into_owned()
    }
}
