// Archive capabilities shared by the SFTP page and its context menus. The
// backend probes the host that owns the files, so the available formats differ
// between the local pane and each remote server.

export type ArchiveFormat = 'tar.zst' | 'tar.gz' | 'tar.xz' | 'tar' | 'zip' | '7z'

export interface ArchiveTools {
  tar: boolean
  zstd: boolean
  gzip: boolean
  xz: boolean
  zip: boolean
  unzip: boolean
  seven_zip: boolean
  formats: ArchiveFormat[]
}

type ArchiveToolName = 'tar' | 'zstd' | 'gzip' | 'xz' | 'unzip' | 'seven_zip'

// Longest suffixes first, mirroring internal/archive/archive.go.
const EXTRACT_SUFFIXES: [suffix: string, tools: ArchiveToolName[]][] = [
  ['.tar.zst', ['tar', 'zstd']],
  ['.tzst', ['tar', 'zstd']],
  ['.tar.gz', ['tar', 'gzip']],
  ['.tgz', ['tar', 'gzip']],
  ['.tar.xz', ['tar', 'xz']],
  ['.txz', ['tar', 'xz']],
  ['.tar', ['tar']],
  ['.zst', ['zstd']],
  ['.gz', ['gzip']],
  ['.xz', ['xz']],
  ['.zip', ['unzip']],
  ['.7z', ['seven_zip']]
]

/** True when the name looks like an archive and the host can extract it. */
export function canExtractName(name: string, tools: ArchiveTools | null): boolean {
  if (!tools) return false
  const lower = name.toLowerCase()
  for (const [suffix, needed] of EXTRACT_SUFFIXES) {
    if (!lower.endsWith(suffix) || lower.length === suffix.length) continue
    return needed.every(tool => tools[tool])
  }
  return false
}
