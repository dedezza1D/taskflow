// The interface is Portuguese, so its dates are too — independent of whatever
// locale the browser happens to run in.
const DATE_TIME = new Intl.DateTimeFormat('pt-BR', {
  dateStyle: 'short',
  timeStyle: 'short',
})

export function formatDateTime(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : DATE_TIME.format(d)
}

export function formatDuration(fromIso: string, toIso: string): string {
  const ms = new Date(toIso).getTime() - new Date(fromIso).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  return ms < 1000
    ? `${ms} ms`
    : `${(ms / 1000).toFixed(1).replace('.', ',')} s`
}
