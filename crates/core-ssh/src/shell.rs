use std::sync::Arc;
use std::time::Duration;

use russh::{Channel, ChannelMsg, ChannelReadHalf, ChannelWriteHalf, Pty, Sig, client};
use serde::Serialize;
use tokio::sync::mpsc;

use crate::SshError;
use crate::shell_integration::ShellIntegration;
use crate::x11::PreparedX11;

const SHELL_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);
const X11_REQUEST_TIMEOUT: Duration = Duration::from_secs(5);
const INPUT_COALESCE_CAP: usize = 16 * 1024;

#[derive(Debug, Clone)]
pub enum TerminalEvent {
    Output(Vec<u8>),
    Exit {
        status: Option<u32>,
        reason: String,
    },
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum SendInputResult {
    Sent,
    Closed,
}

#[derive(Debug, Clone)]
pub struct ShellOpenOptions {
    pub cols: u32,
    pub rows: u32,
    pub term: String,
    pub startup_command: Option<String>,
    pub x11: Option<PreparedX11>,
}

impl Default for ShellOpenOptions {
    fn default() -> Self {
        Self {
            cols: 120,
            rows: 36,
            term: "xterm-256color".to_string(),
            startup_command: None,
            x11: None,
        }
    }
}

enum ShellControl {
    Input(Vec<u8>),
    Resize { cols: u32, rows: u32 },
    Signal(Sig),
    Close,
}

#[derive(Clone)]
pub struct PtyShellSession {
    control_tx: mpsc::UnboundedSender<ShellControl>,
    integration: Option<Arc<ShellIntegration>>,
    x11_active: bool,
}

pub struct PreparedPtyChannel {
    read_half: ChannelReadHalf,
    write_half: ChannelWriteHalf<client::Msg>,
    early_output: Vec<u8>,
    x11_active: bool,
}

pub async fn prepare_pty_channel(
    channel: Channel<client::Msg>,
    cols: u32,
    rows: u32,
    term: &str,
    x11: Option<&PreparedX11>,
) -> Result<PreparedPtyChannel, SshError> {
    let (mut read_half, write_half) = channel.split();
    let cols = cols.max(20);
    let rows = rows.max(5);
    let term = if term.trim().is_empty() {
        "xterm-256color"
    } else {
        term.trim()
    };

    let mut early_output = Vec::new();
    let pty_modes = [
        (Pty::ECHO, 1),
        (Pty::TTY_OP_ISPEED, 14400),
        (Pty::TTY_OP_OSPEED, 14400),
    ];

    write_half
        .request_pty(true, term, cols, rows, 0, 0, &pty_modes)
        .await
        .map_err(|err| SshError::Shell(format!("failed to request PTY: {err}")))?;
    wait_channel_reply(&mut read_half, &mut early_output, SHELL_REQUEST_TIMEOUT, false).await?;

    let mut x11_active = false;
    if let Some(x11) = x11 {
        write_half
            .request_x11(
                true,
                false,
                "MIT-MAGIC-COOKIE-1",
                &x11.fake_cookie_hex,
                x11.display.screen_num,
            )
            .await
            .map_err(|err| SshError::Shell(format!("failed to request X11 forwarding: {err}")))?;
        wait_channel_reply(&mut read_half, &mut early_output, X11_REQUEST_TIMEOUT, true).await?;
        x11_active = true;
    }

    Ok(PreparedPtyChannel {
        read_half,
        write_half,
        early_output,
        x11_active,
    })
}

impl PreparedPtyChannel {
    pub async fn start(
        mut self,
        startup_command: Option<&str>,
        integration: Option<ShellIntegration>,
    ) -> Result<(PtyShellSession, mpsc::UnboundedReceiver<TerminalEvent>), SshError> {
        if let Some(cmd) = startup_command {
            self.write_half
                .exec(true, cmd.as_bytes().to_vec())
                .await
                .map_err(|err| SshError::Shell(format!("failed to start shell command: {err}")))?;
        } else {
            self.write_half
                .request_shell(true)
                .await
                .map_err(|err| SshError::Shell(format!("failed to start interactive shell: {err}")))?;
        }
        wait_channel_reply(
            &mut self.read_half,
            &mut self.early_output,
            SHELL_REQUEST_TIMEOUT,
            false,
        )
        .await?;

        let (event_tx, event_rx) = mpsc::unbounded_channel();
        if !self.early_output.is_empty() {
            let _ = event_tx.send(TerminalEvent::Output(self.early_output));
        }

        let (control_tx, mut control_rx) = mpsc::unbounded_channel::<ShellControl>();
        let write_half = self.write_half;
        let mut read_half = self.read_half;

        tokio::spawn(async move {
            let mut next_ctrl: Option<ShellControl> = None;
            loop {
                let ctrl = match next_ctrl.take() {
                    Some(c) => c,
                    None => match control_rx.recv().await {
                        Some(c) => c,
                        None => break,
                    },
                };
                match ctrl {
                    ShellControl::Input(mut bytes) => {
                        while bytes.len() < INPUT_COALESCE_CAP {
                            match control_rx.try_recv() {
                                Ok(ShellControl::Input(more)) => bytes.extend_from_slice(&more),
                                Ok(other) => {
                                    next_ctrl = Some(other);
                                    break;
                                }
                                Err(_) => break,
                            }
                        }
                        if !bytes.is_empty() && write_half.data_bytes(bytes).await.is_err() {
                            break;
                        }
                    }
                    ShellControl::Resize { cols, rows } => {
                        let _ = write_half
                            .window_change(cols.max(1), rows.max(1), 0, 0)
                            .await;
                    }
                    ShellControl::Signal(sig) => {
                        let _ = write_half.signal(sig).await;
                    }
                    ShellControl::Close => {
                        let _ = write_half.eof().await;
                        let _ = write_half.close().await;
                        break;
                    }
                }
            }
            let _ = write_half.close().await;
        });

        tokio::spawn(async move {
            let mut exit_status: Option<u32> = None;
            let mut exit_signal: Option<String> = None;

            while let Some(msg) = read_half.wait().await {
                match msg {
                    ChannelMsg::Data { data } | ChannelMsg::ExtendedData { data, .. } => {
                        if !data.is_empty()
                            && event_tx.send(TerminalEvent::Output(data.to_vec())).is_err()
                        {
                            break;
                        }
                    }
                    ChannelMsg::ExitStatus { exit_status: code } => {
                        exit_status = Some(code);
                    }
                    ChannelMsg::ExitSignal {
                        signal_name,
                        error_message,
                        ..
                    } => {
                        exit_signal = Some(if error_message.trim().is_empty() {
                            format!("{signal_name:?}")
                        } else {
                            format!("{signal_name:?}: {error_message}")
                        });
                    }
                    ChannelMsg::Close => break,
                    ChannelMsg::Eof | _ => {}
                }
            }

            let reason = if let Some(sig) = exit_signal {
                format!("terminated by signal {sig}")
            } else if let Some(code) = exit_status {
                format!("shell exited with status {code}")
            } else {
                "shell connection closed".to_string()
            };

            let _ = event_tx.send(TerminalEvent::Exit {
                status: exit_status,
                reason,
            });
        });

        Ok((
            PtyShellSession {
                control_tx,
                integration: integration.map(Arc::new),
                x11_active: self.x11_active,
            },
            event_rx,
        ))
    }
}

impl PtyShellSession {
    pub async fn open_on_channel(
        channel: Channel<client::Msg>,
        options: ShellOpenOptions,
        integration: Option<ShellIntegration>,
    ) -> Result<(Self, mpsc::UnboundedReceiver<TerminalEvent>), SshError> {
        let prepared = prepare_pty_channel(
            channel,
            options.cols,
            options.rows,
            &options.term,
            options.x11.as_ref(),
        )
        .await?;
        prepared
            .start(options.startup_command.as_deref(), integration)
            .await
    }

    pub fn integration(&self) -> Option<&ShellIntegration> {
        self.integration.as_deref()
    }

    pub fn x11_active(&self) -> bool {
        self.x11_active
    }

    pub fn send_input(&self, data: Vec<u8>) -> SendInputResult {
        if data.is_empty() {
            return SendInputResult::Sent;
        }
        match self.control_tx.send(ShellControl::Input(data)) {
            Ok(()) => SendInputResult::Sent,
            Err(_) => SendInputResult::Closed,
        }
    }

    pub fn resize(&self, cols: u32, rows: u32) -> Result<(), SshError> {
        self.control_tx
            .send(ShellControl::Resize {
                cols: cols.max(1),
                rows: rows.max(1),
            })
            .map_err(|_| SshError::Shell("terminal session is closed".into()))
    }

    pub fn signal(&self, sig: Sig) -> Result<(), SshError> {
        self.control_tx
            .send(ShellControl::Signal(sig))
            .map_err(|_| SshError::Shell("terminal session is closed".into()))
    }

    pub fn close(&self) {
        let _ = self.control_tx.send(ShellControl::Close);
    }
}

async fn wait_channel_reply(
    read_half: &mut ChannelReadHalf,
    early_output: &mut Vec<u8>,
    reply_timeout: Duration,
    is_x11: bool,
) -> Result<(), SshError> {
    let deadline = tokio::time::Instant::now() + reply_timeout;
    loop {
        let now = tokio::time::Instant::now();
        let remaining = deadline.saturating_duration_since(now);
        if remaining.is_zero() {
            return Err(if is_x11 {
                SshError::X11Timeout
            } else {
                SshError::Timeout(reply_timeout)
            });
        }

        let msg = tokio::time::timeout(remaining, read_half.wait())
            .await
            .map_err(|_| {
                if is_x11 {
                    SshError::X11Timeout
                } else {
                    SshError::Timeout(reply_timeout)
                }
            })?;

        match msg {
            Some(ChannelMsg::Success) => return Ok(()),
            Some(ChannelMsg::Failure) => {
                return Err(SshError::Shell(if is_x11 {
                    "SSH server rejected X11 forwarding request".into()
                } else {
                    "SSH channel request rejected by server".into()
                }));
            }
            Some(ChannelMsg::Data { data }) | Some(ChannelMsg::ExtendedData { data, .. }) => {
                early_output.extend_from_slice(&data);
            }
            Some(ChannelMsg::Eof | ChannelMsg::Close) | None => {
                return Err(SshError::Shell(
                    "SSH channel closed while waiting for request confirmation".into(),
                ));
            }
            Some(_) => continue,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};

    use russh::keys::{Algorithm, PrivateKey, PublicKeyOrCertificate};
    use russh::server::{self, Server};
    use russh::ChannelId;
    use tokio::net::TcpListener;

    struct FakeClientHandler;
    impl russh::client::Handler for FakeClientHandler {
        type Error = russh::Error;

        fn check_server_key(
            &mut self,
            _server_public_key: &PublicKeyOrCertificate,
        ) -> impl std::future::Future<Output = Result<bool, Self::Error>> + Send {
            async { Ok(true) }
        }
    }

    #[derive(Clone)]
    struct TestServerState {
        last_cols: Arc<AtomicU32>,
        saw_pty_modes: Arc<AtomicBool>,
        hang_x11: bool,
    }

    struct TestServer {
        state: TestServerState,
    }

    impl server::Server for TestServer {
        type Handler = TestServerHandler;

        fn new_client(&mut self, _peer_addr: Option<std::net::SocketAddr>) -> Self::Handler {
            TestServerHandler {
                state: self.state.clone(),
                channels: HashMap::new(),
            }
        }
    }

    struct TestServerHandler {
        state: TestServerState,
        channels: HashMap<ChannelId, ()>,
    }

    impl server::Handler for TestServerHandler {
        type Error = russh::Error;

        async fn auth_none(&mut self, _user: &str) -> Result<server::Auth, Self::Error> {
            Ok(server::Auth::Accept)
        }

        async fn channel_open_session(
            &mut self,
            channel: Channel<server::Msg>,
            reply: server::ChannelOpenHandle,
            _session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            self.channels.insert(channel.id(), ());
            reply.accept().await;
            Ok(())
        }

        async fn pty_request(
            &mut self,
            channel: ChannelId,
            _term: &str,
            col_width: u32,
            _row_height: u32,
            _pix_width: u32,
            _pix_height: u32,
            modes: &[(Pty, u32)],
            session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            self.state.last_cols.store(col_width, Ordering::SeqCst);
            let has_echo = modes.iter().any(|(m, v)| *m == Pty::ECHO && *v == 1);
            if has_echo {
                self.state.saw_pty_modes.store(true, Ordering::SeqCst);
            }
            session.channel_success(channel)?;
            Ok(())
        }

        async fn x11_request(
            &mut self,
            channel: ChannelId,
            _single_connection: bool,
            _x11_auth_protocol: &str,
            _x11_auth_cookie: &str,
            _x11_screen_number: u32,
            session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            if !self.state.hang_x11 {
                session.channel_success(channel)?;
            }
            Ok(())
        }

        async fn shell_request(
            &mut self,
            channel: ChannelId,
            session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            // Send early output before/alongside ChannelSuccess to test buffering.
            session.data(channel, b"BANNER> ".as_slice())?;
            session.channel_success(channel)?;
            Ok(())
        }

        async fn window_change_request(
            &mut self,
            _channel: ChannelId,
            col_width: u32,
            _row_height: u32,
            _pix_width: u32,
            _pix_height: u32,
            _session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            self.state.last_cols.store(col_width, Ordering::SeqCst);
            Ok(())
        }

        async fn data(
            &mut self,
            channel: ChannelId,
            data: &[u8],
            session: &mut server::Session,
        ) -> Result<(), Self::Error> {
            if data == b"exit\n" {
                session.data(channel, b"BYE\n".as_slice())?;
                session.exit_status_request(channel, 0)?;
                session.eof(channel)?;
                session.close(channel)?;
            } else {
                let mut echoed = b"ECHO:".to_vec();
                echoed.extend_from_slice(data);
                session.data(channel, echoed)?;
            }
            Ok(())
        }
    }

    async fn start_test_server(
        hang_x11: bool,
    ) -> (std::net::SocketAddr, TestServerState) {
        let key = PrivateKey::random(&mut rand::rng(), Algorithm::Ed25519).unwrap();
        let config = Arc::new(server::Config {
            keys: vec![key],
            ..Default::default()
        });
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let state = TestServerState {
            last_cols: Arc::new(AtomicU32::new(0)),
            saw_pty_modes: Arc::new(AtomicBool::new(false)),
            hang_x11,
        };
        let mut srv = TestServer {
            state: state.clone(),
        };
        tokio::spawn(async move {
            let _ = srv.run_on_socket(config, &listener).await;
        });
        (addr, state)
    }

    #[tokio::test]
    async fn pty_shell_delivers_banner_input_echo_resize_and_exit() {
        let (addr, state) = start_test_server(false).await;
        let client_cfg = Arc::new(russh::client::Config::default());
        let mut handle = russh::client::connect(client_cfg, addr, FakeClientHandler)
            .await
            .unwrap();
        assert!(handle.authenticate_none("user").await.unwrap().success());

        let channel = handle.channel_open_session().await.unwrap();
        let (shell, mut events) = PtyShellSession::open_on_channel(
            channel,
            ShellOpenOptions {
                cols: 100,
                rows: 30,
                ..Default::default()
            },
            None,
        )
        .await
        .unwrap();

        assert!(state.saw_pty_modes.load(Ordering::SeqCst));
        assert_eq!(state.last_cols.load(Ordering::SeqCst), 100);

        let first = tokio::time::timeout(Duration::from_secs(2), events.recv())
            .await
            .unwrap()
            .unwrap();
        match first {
            TerminalEvent::Output(bytes) => assert_eq!(bytes, b"BANNER> "),
            other => panic!("unexpected first event: {other:?}"),
        }

        assert_eq!(shell.send_input(b"ls -la\n".to_vec()), SendInputResult::Sent);
        let second = tokio::time::timeout(Duration::from_secs(2), events.recv())
            .await
            .unwrap()
            .unwrap();
        match second {
            TerminalEvent::Output(bytes) => assert_eq!(bytes, b"ECHO:ls -la\n"),
            other => panic!("unexpected second event: {other:?}"),
        }

        shell.resize(132, 42).unwrap();
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(state.last_cols.load(Ordering::SeqCst), 132);

        assert_eq!(shell.send_input(b"exit\n".to_vec()), SendInputResult::Sent);
        let mut saw_exit = false;
        let mut collected = Vec::new();
        while let Ok(Some(ev)) = tokio::time::timeout(Duration::from_secs(2), events.recv()).await {
            match ev {
                TerminalEvent::Output(b) => collected.extend_from_slice(&b),
                TerminalEvent::Exit { status, .. } => {
                    assert_eq!(status, Some(0));
                    saw_exit = true;
                    break;
                }
            }
        }
        assert_eq!(collected, b"BYE\n");
        assert!(saw_exit);
    }

    #[tokio::test]
    async fn x11_request_timeout_returns_x11_timeout_error() {
        let (addr, _state) = start_test_server(true).await;
        let client_cfg = Arc::new(russh::client::Config::default());
        let mut handle = russh::client::connect(client_cfg, addr, FakeClientHandler)
            .await
            .unwrap();
        assert!(handle.authenticate_none("user").await.unwrap().success());

        let channel = handle.channel_open_session().await.unwrap();
        let fake_x11 = PreparedX11 {
            display: crate::x11::parse_x11_display("127.0.0.1:0").unwrap(),
            fake_cookie: [0xaa; 16],
            fake_cookie_hex: "aa".repeat(16),
            real_proto: String::new(),
            real_cookie: Vec::new(),
        };

        let res = PtyShellSession::open_on_channel(
            channel,
            ShellOpenOptions {
                x11: Some(fake_x11),
                ..Default::default()
            },
            None,
        )
        .await;

        assert!(matches!(res, Err(SshError::X11Timeout)));
    }
}
