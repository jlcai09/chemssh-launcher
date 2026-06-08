const BOTTOM_GAP = 24

export function createAutoScroll(getElement: () => HTMLElement | null) {
  let follow = true
  let frame = 0

  function isNearBottom(el: HTMLElement) {
    return el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_GAP
  }

  function onScroll() {
    const el = getElement()
    if (!el) return
    follow = isNearBottom(el)
  }

  function scrollToBottom(force = false) {
    const el = getElement()
    if (!el) return
    if (!force && !follow && !isNearBottom(el)) return
    if (frame) window.cancelAnimationFrame(frame)
    frame = window.requestAnimationFrame(() => {
      frame = 0
      const latest = getElement()
      if (!latest) return
      latest.scrollTop = latest.scrollHeight
      follow = true
    })
  }

  return { onScroll, scrollToBottom }
}
