// Server profiles are cached per page for the terminal sidebar and the SFTP
// new-tab menu. A BroadcastChannel keeps those caches in sync across browser
// tabs and the shell's iframes without polling the backend.
const PROFILE_CHANNEL = 'chemssh-launcher:profiles'

interface ProfileChangeMessage {
  type: 'profiles-changed'
}

export function notifyProfilesChanged() {
  if (typeof BroadcastChannel === 'undefined') return
  try {
    const channel = new BroadcastChannel(PROFILE_CHANNEL)
    channel.postMessage({ type: 'profiles-changed' } satisfies ProfileChangeMessage)
    channel.close()
  } catch {
    // A blocked channel only costs the automatic refresh; save stays intact.
  }
}

export function onProfilesChanged(handler: () => void) {
  if (typeof BroadcastChannel === 'undefined') return () => {}
  let channel: BroadcastChannel
  try {
    channel = new BroadcastChannel(PROFILE_CHANNEL)
  } catch {
    return () => {}
  }
  channel.addEventListener('message', event => {
    const data = event.data as ProfileChangeMessage | null
    if (data?.type === 'profiles-changed') handler()
  })
  return () => channel.close()
}
