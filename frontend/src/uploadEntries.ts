export type UploadConflictAction = 'overwrite' | 'skip' | 'suffix' | 'cancel'

export type UploadConflictResolution = {
  action: UploadConflictAction
  applyAll: boolean
}

export type UploadEntry = {
  file: File
  relativePath: string
  displayPath: string
  rootName: string
}

export type FileSystemEntryLike = {
  isFile: boolean
  isDirectory: boolean
  name: string
  file?: (callback: (file: File) => void, errorCallback?: (error: unknown) => void) => void
  createReader?: () => {
    readEntries: (callback: (entries: FileSystemEntryLike[]) => void, errorCallback?: (error: unknown) => void) => void
  }
}

export const SAFE_UPLOAD_SEGMENT_RE = /^[A-Za-z0-9._()\-]+$/

export function filesToUploadEntries(files: File[]) {
  return files
    .filter(file => file.name)
    .map(file => {
      const relativePath = sanitizeRelativePath(file.webkitRelativePath || file.name)
      return {
        file,
        relativePath,
        displayPath: relativePath,
        rootName: relativePath.split('/')[0] ?? file.name
      } satisfies UploadEntry
    })
}

export function normalizeUploadEntries(entries: UploadEntry[]) {
  let invalidCount = 0
  let renamedCount = 0
  const normalized: UploadEntry[] = []

  for (const entry of entries) {
    const originalPath = sanitizeRelativePath(entry.relativePath)
    const originalDisplayPath = sanitizeRelativePath(entry.displayPath || entry.relativePath)
    const relativePath = normalizeUploadRelativePath(originalPath)
    const displayPath = normalizeUploadRelativePath(originalDisplayPath)
    if (!relativePath || !isSafeUploadRelativePath(relativePath)) {
      invalidCount += 1
      continue
    }
    if (relativePath !== originalPath || displayPath !== originalDisplayPath) renamedCount += 1
    normalized.push({
      ...entry,
      relativePath,
      displayPath,
      rootName: relativePath.split('/')[0] ?? entry.file.name
    })
  }

  return { entries: normalized, invalidCount, renamedCount }
}

export function sanitizeRelativePath(path: string) {
  return path.replace(/\\/g, '/').split('/').filter(part => part && part !== '.' && part !== '..').join('/')
}

export function normalizeUploadRelativePath(path: string) {
  return sanitizeRelativePath(path)
    .split('/')
    .map(normalizeUploadSegment)
    .filter(Boolean)
    .join('/')
}

export function normalizeUploadSegment(value: string) {
  return value.replace(/\s+/g, '_')
}

export function isSafeUploadRelativePath(path: string) {
  const parts = path.split('/').filter(Boolean)
  return parts.length > 0 && parts.every(part => SAFE_UPLOAD_SEGMENT_RE.test(part))
}

export function hasFileDrag(event: DragEvent) {
  return Array.from(event.dataTransfer?.types ?? []).includes('Files')
}

export function setUploadDropEffect(event: DragEvent) {
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy'
}

export async function collectDropUploadEntries(event: DragEvent) {
  const items = Array.from(event.dataTransfer?.items ?? [])
  if (items.length > 0) {
    const entries = await Promise.all(items.map(item => collectDataTransferItem(item)))
    const flattened = entries.flat().filter(entry => entry.file.name)
    if (flattened.length > 0) return flattened
  }
  return filesToUploadEntries(Array.from(event.dataTransfer?.files ?? []).filter(file => file.name))
}

export async function collectDataTransferItem(item: DataTransferItem): Promise<UploadEntry[]> {
  if (item.kind !== 'file') return []
  const getEntry = (item as DataTransferItem & { webkitGetAsEntry?: () => FileSystemEntryLike | null }).webkitGetAsEntry
  const entry = getEntry?.call(item)
  if (!entry) {
    const file = item.getAsFile()
    return file ? filesToUploadEntries([file]) : []
  }
  return collectFileSystemEntry(entry, '')
}

export async function collectFileSystemEntry(entry: FileSystemEntryLike, parentPath: string): Promise<UploadEntry[]> {
  const relativePath = sanitizeRelativePath(parentPath ? `${parentPath}/${entry.name}` : entry.name)
  if (entry.isFile) {
    const file = await readFileSystemEntryFile(entry)
    return file
      ? [{
          file,
          relativePath,
          displayPath: relativePath,
          rootName: relativePath.split('/')[0] ?? file.name
        }]
      : []
  }
  if (!entry.isDirectory || !entry.createReader) return []
  const children = await readAllDirectoryEntries(entry)
  const nested = await Promise.all(children.map(child => collectFileSystemEntry(child, relativePath)))
  return nested.flat()
}

export function readFileSystemEntryFile(entry: FileSystemEntryLike) {
  return new Promise<File | null>(resolve => {
    if (!entry.file) {
      resolve(null)
      return
    }
    entry.file(file => resolve(file), () => resolve(null))
  })
}

export async function readAllDirectoryEntries(entry: FileSystemEntryLike) {
  const reader = entry.createReader?.()
  if (!reader) return []
  const entries: FileSystemEntryLike[] = []
  while (true) {
    const batch = await new Promise<FileSystemEntryLike[]>(resolve => {
      reader.readEntries(resolve, () => resolve([]))
    })
    if (batch.length === 0) break
    entries.push(...batch)
  }
  return entries
}
