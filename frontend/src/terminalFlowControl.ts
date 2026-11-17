// These are locally sent XON/XOFF requests, not an acknowledgement from the
// remote PTY: IXON may be disabled, remapped, or handled by an application.
export type TerminalFlowControl = 'none' | 'pause-requested' | 'resume-requested'

export function nextTerminalFlowControl(current: TerminalFlowControl, data: string): TerminalFlowControl {
  const pause = data.lastIndexOf('\x13')
  const resume = data.lastIndexOf('\x11')
  if (pause < 0 && resume < 0) return current
  return pause > resume ? 'pause-requested' : 'resume-requested'
}
