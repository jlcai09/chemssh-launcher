// An SFTP session starts in the remote home directory; its realpath('.') says
// nothing about the interactive terminal's cwd. Never use it as a fallback.
export async function terminalUploadDirectory(
  knownDirectory: string,
  readCurrentDirectory: () => Promise<{ path?: string }>,
  unavailableMessage: string
): Promise<string> {
  try {
    const current = await readCurrentDirectory()
    if (isTerminalDirectory(current.path)) return current.path
  } catch {
    // Hosts without procfs can still report cwd through OSC 7 / OSC 633.
  }
  if (isTerminalDirectory(knownDirectory)) return knownDirectory
  throw new Error(unavailableMessage)
}

export function isTerminalDirectory(path: unknown): path is string {
  return typeof path === 'string' && path.startsWith('/') && !/[\r\n\0]/.test(path)
}
