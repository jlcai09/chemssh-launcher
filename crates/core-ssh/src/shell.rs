use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use russh::ChannelMsg;
use serde::Serialize;
use tokio::sync::mpsc;

use crate::SshError;

const BATCH_WINDOW_MS: u64 = 16;
const BATCH_MAX_BYTES: usize = 16 * 1024;
const INPUT_QUEUE_CAPACITY: usize = 256;

#[derive(Debug, Clone, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum TerminalEvent {
    Output { seq: u64, data_b64: String },
    FlowControl { paused: bool },
    Exit { exit_code: Option<u32> },
}

#[derive(Debug, Clone, Serialize)]
pub struct SendInputResult {
    pub accepted: bool,
    pub flow_paused: bool,
}

enum ShellCommand {
    Input(Vec<u8>),
    Resize { cols: u32, rows: u32 },
    Close,
}

pub struct PtyShellSession {
    cmd_tx: mpsc::Sender<ShellCommand>,
    flow_paused: Arc<AtomicBool>,
    closed: Arc<AtomicBool>,
}

impl PtyShellSession {
    pub async fn open(
        mut channel: russh::Channel<russh::client::Msg>,
        cols: u32,
        rows: u32,
        term: &str,
        event_tx: mpsc::Sender<TerminalEvent>,
    ) -> Result<Self, SshError> {
        channel
            .request_pty(false, term, cols, rows, 0, 0, &[])
            .await
            .map_err(|e| SshError::Shell(format!("request pty: {e}")))?;
        channel
            .request_shell(false)
            .await
            .map_err(|e| SshError::Shell(format!("request shell: {e}")))?;

        let (cmd_tx, mut cmd_rx) = mpsc::channel::<ShellCommand>(INPUT_QUEUE_CAPACITY);
        let flow_paused = Arc::new(AtomicBool::new(false));
        let closed = Arc::new(AtomicBool::new(false));
        let closed_task = Arc::clone(&closed);
        let seq = Arc::new(AtomicU64::new(0));

        tokio::spawn(async move {
            let mut pending_out = Vec::<u8>::with_capacity(BATCH_MAX_BYTES);
            let mut flush_interval = tokio::time::interval(Duration::from_millis(BATCH_WINDOW_MS));
            flush_interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            let mut exit_code: Option<u32> = None;

            loop {
                tokio::select! {
                    biased;
                    cmd = cmd_rx.recv() => {
                        match cmd {
                            Some(ShellCommand::Input(bytes)) => {
                                if channel.data(bytes.as_slice()).await.is_err() {
                                    break;
                                }
                            }
                            Some(ShellCommand::Resize { cols, rows }) => {
                                let _ = channel.window_change(cols, rows, 0, 0).await;
                            }
                            Some(ShellCommand::Close) | None => {
                                let _ = channel.eof().await;
                                let _ = channel.close().await;
                                break;
                            }
                        }
                    }
                    msg = channel.wait() => {
                        match msg {
                            Some(ChannelMsg::Data { data }) => {
                                pending_out.extend_from_slice(&data);
                                if pending_out.len() >= BATCH_MAX_BYTES {
                                    flush_output(&mut pending_out, &seq, &event_tx).await;
                                }
                            }
                            Some(ChannelMsg::ExtendedData { data, .. }) => {
                                pending_out.extend_from_slice(&data);
                                if pending_out.len() >= BATCH_MAX_BYTES {
                                    flush_output(&mut pending_out, &seq, &event_tx).await;
                                }
                            }
                            Some(ChannelMsg::ExitStatus { exit_status }) => {
                                exit_code = Some(exit_status);
                            }
                            Some(ChannelMsg::Eof) | Some(ChannelMsg::Close) | None => {
                                break;
                            }
                            _ => {}
                        }
                    }
                    _ = flush_interval.tick(), if !pending_out.is_empty() => {
                        flush_output(&mut pending_out, &seq, &event_tx).await;
                    }
                }
            }

            if !pending_out.is_empty() {
                flush_output(&mut pending_out, &seq, &event_tx).await;
            }
            closed_task.store(true, Ordering::SeqCst);
            let _ = event_tx.send(TerminalEvent::Exit { exit_code }).await;
        });

        Ok(Self {
            cmd_tx,
            flow_paused,
            closed,
        })
    }

    /// Sends input bytes and updates local XOFF/XON flow-control tracking.
    pub fn send_input(&self, data: Vec<u8>) -> Result<SendInputResult, SshError> {
        if self.closed.load(Ordering::SeqCst) {
            return Err(SshError::Shell("terminal session is closed".into()));
        }
        // Track Ctrl+S (0x13 XOFF) / Ctrl+Q (0x11 XON) in order.
        for &b in &data {
            if b == 0x13 {
                self.flow_paused.store(true, Ordering::SeqCst);
            } else if b == 0x11 {
                self.flow_paused.store(false, Ordering::SeqCst);
            }
        }
        let paused = self.flow_paused.load(Ordering::SeqCst);
        match self.cmd_tx.try_send(ShellCommand::Input(data)) {
            Ok(()) => Ok(SendInputResult {
                accepted: true,
                flow_paused: paused,
            }),
            Err(mpsc::error::TrySendError::Full(_)) => Ok(SendInputResult {
                accepted: false,
                flow_paused: paused,
            }),
            Err(mpsc::error::TrySendError::Closed(_)) => {
                Err(SshError::Shell("terminal session is closed".into()))
            }
        }
    }

    pub async fn resize(&self, cols: u32, rows: u32) -> Result<(), SshError> {
        self.cmd_tx
            .send(ShellCommand::Resize { cols, rows })
            .await
            .map_err(|_| SshError::Shell("terminal session is closed".into()))
    }

    pub async fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
        let _ = self.cmd_tx.send(ShellCommand::Close).await;
    }
}

async fn flush_output(
    pending: &mut Vec<u8>,
    seq: &AtomicU64,
    event_tx: &mpsc::Sender<TerminalEvent>,
) {
    let next_seq = seq.fetch_add(1, Ordering::SeqCst) + 1;
    let data_b64 = BASE64.encode(pending.as_slice());
    pending.clear();
    let _ = event_tx
        .send(TerminalEvent::Output {
            seq: next_seq,
            data_b64,
        })
        .await;
}
