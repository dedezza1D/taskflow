import { useCallback, useEffect, useRef, useState } from 'react'
import type { RefObject } from 'react'
import { api, ApiError } from './api'
import type {
  ApiDocument,
  DocumentArtifact,
  Report,
  TaskExecution,
} from './api'

/** While a document is being analysed, the list follows it closely. */
export const BUSY_POLL_MS = 3000
/** Otherwise it only needs to notice uploads made by someone else. */
export const IDLE_POLL_MS = 15000

export interface DocumentsState {
  documents: ApiDocument[]
  apiDown: boolean
  loading: boolean
  refresh: () => Promise<void>
  addLocally: (doc: ApiDocument) => void
  removeLocally: (id: string) => void
}

function inFlight(docs: ApiDocument[]): boolean {
  return docs.some((d) => d.status === 'uploaded' || d.status === 'processing')
}

/**
 * "The API is down" is only true when the server did not answer usefully: no
 * response at all, or a gateway/server error. A 401 means the session ended —
 * api.ts reports that to the app, which returns to the login screen — and it
 * used to surface here as "API fora do ar" over a list that stayed on screen.
 */
export function isOutage(e: unknown): boolean {
  if (!(e instanceof ApiError)) return true
  return e.status >= 500
}

/** Keeps the list fresh — closely while anything is in flight; idles when the tab hides. */
export function useDocuments(): DocumentsState {
  const [documents, setDocuments] = useState<ApiDocument[]>([])
  const [apiDown, setApiDown] = useState(false)
  const [loading, setLoading] = useState(true)
  const busy = useRef(false)

  const refresh = useCallback(async () => {
    try {
      const { items } = await api.listDocuments()
      busy.current = inFlight(items)
      setDocuments(items)
      setApiDown(false)
    } catch (e) {
      if (isOutage(e)) setApiDown(true)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    let timer: number | undefined
    let stopped = false

    function schedule() {
      if (timer !== undefined) window.clearTimeout(timer)
      timer = window.setTimeout(
        tick,
        busy.current ? BUSY_POLL_MS : IDLE_POLL_MS,
      )
    }

    // A chain of timeouts rather than one interval, so every wait can pick its
    // own length from what the last answer said.
    async function tick(initial = false) {
      // The first load always happens: a tab opened in the background must not
      // sit on a loading skeleton until someone looks at it.
      if (initial || !document.hidden) await refresh()
      if (!stopped) schedule()
    }

    // Coming back to the tab is exactly when stale data would be read, and the
    // idle wait could still have fifteen seconds to run.
    async function onVisible() {
      if (document.hidden || stopped) return
      await refresh()
      if (!stopped) schedule()
    }

    void tick(true)
    document.addEventListener('visibilitychange', onVisible)

    return () => {
      stopped = true
      if (timer !== undefined) window.clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [refresh])

  const addLocally = useCallback((doc: ApiDocument) => {
    // Just uploaded: the next wait should be the short one.
    busy.current = true
    setDocuments((prev) => [doc, ...prev])
  }, [])

  const removeLocally = useCallback((id: string) => {
    setDocuments((prev) => prev.filter((d) => d.id !== id))
  }, [])

  return { documents, apiDown, loading, refresh, addLocally, removeLocally }
}

/**
 * Reports for the whole corpus, so the inventory can aggregate them. A report
 * is immutable once written, so each is fetched once and cached for the
 * session; fetches run a few at a time rather than all at once.
 */
export interface ReportsState {
  reports: Map<string, Report>
  /** Completed documents whose report the API refuses to hand over. */
  unreadable: Set<string>
  /** Store a report fetched elsewhere (the detail pane), so it is not fetched twice. */
  remember: (id: string, report: Report) => void
}

export function useReports(documents: ApiDocument[]): ReportsState {
  const [reports, setReports] = useState<Map<string, Report>>(new Map())
  const [unreadable, setUnreadable] = useState<Set<string>>(new Set())
  // Ids already fetched or being fetched. Held in a ref, not state, so that
  // storing a report does not re-trigger this effect and tear down the pool
  // that is still fetching the rest.
  const handled = useRef(new Set<string>())
  // Transient failures still need a ceiling. A document reported as "completed"
  // whose report is "not ready" contradicts the pipeline's own invariant --
  // the artifact is written before the row is marked done -- so it is a broken
  // state wearing a retryable name, and retrying it forever is how three stale
  // rows quietly generate traffic for as long as the tab stays open.
  const attempts = useRef(new Map<string, number>())
  const MAX_ATTEMPTS = 3
  const unmounted = useRef(false)

  // Reset on every run, not just set on teardown: StrictMode mounts, tears
  // down and remounts the same instance in development, so a flag that is only
  // ever set to true stays true for the life of the component -- and every
  // worker below then exits after a single fetch, leaving the inventory stuck
  // at zero with four reports loaded.
  useEffect(() => {
    unmounted.current = false
    return () => {
      unmounted.current = true
    }
  }, [])

  useEffect(() => {
    const queue = documents
      .filter((d) => d.status === 'completed' && !handled.current.has(d.id))
      .map((d) => d.id)
    if (queue.length === 0) return

    for (const id of queue) handled.current.add(id)

    async function worker() {
      for (;;) {
        const id = queue.pop()
        if (id === undefined) return
        try {
          const report = await api.getReport(id)
          if (unmounted.current) return
          setReports((prev) => new Map(prev).set(id, report))
        } catch (e) {
          // Retry only what can plausibly change. "report_not_ready" means the
          // worker has not written it yet, and a network failure may pass; both
          // earn another tick. Anything else -- a 500 because the artifact is
          // gone, a 403 -- will fail identically forever, and retrying it once
          // per poll per document is how a quiet page ends up issuing hundreds
          // of requests a minute against a corpus it can never load.
          const transient =
            !(e instanceof ApiError) || e.code === 'report_not_ready'
          const tries = (attempts.current.get(id) ?? 0) + 1
          attempts.current.set(id, tries)
          if (transient && tries < MAX_ATTEMPTS) {
            handled.current.delete(id)
          } else if (!unmounted.current) {
            setUnreadable((prev) =>
              prev.has(id) ? prev : new Set(prev).add(id),
            )
          }
        }
      }
    }

    void Promise.all(
      Array.from({ length: Math.min(4, queue.length) }, () => worker()),
    )
  }, [documents])

  const remember = useCallback((id: string, report: Report) => {
    handled.current.add(id)
    setReports((prev) => (prev.has(id) ? prev : new Map(prev).set(id, report)))
  }, [])

  return { reports, unreadable, remember }
}

export interface DetailState {
  doc: ApiDocument | null
  artifacts: DocumentArtifact[]
  report: Report | null
  gone: boolean
  /** The report exists as an artifact but cannot be fetched. Terminal: the
   * poll below stops, and the panel says so instead of claiming to be busy. */
  reportUnreadable: boolean
  /** Attempt history, loaded once the document reaches a terminal state. */
  executions: TaskExecution[] | null
}

const EMPTY_DETAIL: DetailState = {
  doc: null,
  artifacts: [],
  report: null,
  gone: false,
  reportUnreadable: false,
  executions: null,
}

/**
 * Detail + report for one document, polled until the document reaches a
 * terminal state. Backs off as the wait grows so a stuck document does not
 * hammer the API forever.
 *
 * `knownReport` is the corpus pool's copy, if it already has one; a report is
 * immutable, so fetching it again for the pane is pure waste. `onReportLoaded`
 * hands a report fetched here back to that pool for the same reason.
 */
export function useDocumentDetail(
  documentId: string | null,
  knownReport: Report | null = null,
  onReportLoaded?: (id: string, report: Report) => void,
): DetailState {
  const [state, setState] = useState<DetailState>(EMPTY_DETAIL)

  // Read through refs: the poll below lives as long as the document id, and
  // must see the latest values without restarting.
  const known = useRef(knownReport)
  known.current = knownReport
  const loaded = useRef(onReportLoaded)
  loaded.current = onReportLoaded

  useEffect(() => {
    setState(EMPTY_DETAIL)
    if (!documentId) return

    let stop = false
    let timer: number | undefined
    let delay = 1000
    // Bounded, for the same reason the corpus-level pool is: a document whose
    // report cannot be read is not a document still working, and polling it
    // every eight seconds for as long as the pane stays open is the cost of
    // pretending otherwise.
    let reportAttempts = 0
    let reportUnreadable = false
    // "completed" is not quite the end. The worker marks the document done,
    // then destroys the original, then closes the attempt — milliseconds apart.
    // Stopping on the first "completed" froze the pane on "the original is still
    // stored" and an erasure dialog promising to delete files already gone. A few
    // more polls let those land; the cap covers a shred that failed and was left
    // to the retention sweep.
    let settleWaits = 0
    const MAX_SETTLE_WAITS = 3
    // Immutable once written, so a report fetched by one poll serves the rest.
    let fetchedReport: Report | null = null

    async function load(): Promise<boolean> {
      try {
        const { document, artifacts } = await api.getDocument(documentId!)
        if (stop) return true

        let report: Report | null = known.current ?? fetchedReport
        if (!report && artifacts.some((a) => a.stage === 'report')) {
          try {
            report = await api.getReport(documentId!)
            fetchedReport = report
            if (!stop) loaded.current?.(documentId!, report)
          } catch (e) {
            // "report_not_ready" is a genuine race against the worker writing
            // the artifact. Anything else will fail the same way forever.
            const transient =
              !(e instanceof ApiError) || e.code === 'report_not_ready'
            reportAttempts++
            if (!transient || reportAttempts >= 3) reportUnreadable = true
          }
        }
        if (stop) return true

        let terminal =
          document.status === 'failed' ||
          // Retrying will not produce a report, and the panel has a way to say so.
          reportUnreadable ||
          (document.status === 'completed' && report !== null)

        // The attempt history only changes while the document is working, so
        // it is read once, at the end. For a failure it is where the reason is.
        let executions: TaskExecution[] | null = null
        if (terminal && document.task_id) {
          try {
            executions = (await api.listExecutions(document.task_id)).items
          } catch {
            executions = null // the panel still works without the history
          }
          if (stop) return true
        }

        const unsettled =
          document.status === 'completed' &&
          (!document.raw_shredded_at ||
            (executions ?? []).some((e) => e.status === 'started'))
        if (terminal && unsettled && settleWaits < MAX_SETTLE_WAITS) {
          settleWaits++
          terminal = false
        }

        setState({
          doc: document,
          artifacts,
          report,
          gone: false,
          reportUnreadable,
          executions,
        })
        return terminal
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) {
          if (!stop) setState((s) => ({ ...s, gone: true }))
          return true
        }
        return false
      }
    }

    async function tick() {
      const terminal = await load()
      if (stop || terminal) return
      // Settling takes milliseconds, so those extra polls stay short instead of
      // inheriting the backoff a long OCR run has built up.
      delay = settleWaits > 0 ? 1000 : Math.min(delay * 1.5, 8000)
      timer = window.setTimeout(tick, delay)
    }
    void tick()

    return () => {
      stop = true
      if (timer !== undefined) window.clearTimeout(timer)
    }
  }, [documentId])

  return state
}

const FOCUSABLE =
  'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

/**
 * Modal keyboard behaviour: focus moves into the dialog, Tab cycles inside it
 * instead of wandering into the page behind, Escape closes it (unless busy), and
 * focus returns to whatever opened it. aria-modal alone tells a screen reader
 * the rest of the page is inert; it does nothing for the Tab key.
 */
export function useModalFocus(
  ref: RefObject<HTMLElement | null>,
  onEscape: () => void,
  busy = false,
) {
  const escape = useRef(onEscape)
  escape.current = onEscape
  const isBusy = useRef(busy)
  isBusy.current = busy
  // Captured during the first render, not in the effect: an autoFocus field
  // inside the dialog takes focus at commit, before any effect runs, and the
  // effect would record that field as the thing to return focus to.
  const [opener] = useState(() =>
    document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null,
  )

  useEffect(() => {
    const root = ref.current

    // Respect an autoFocus field already inside; otherwise take the first control.
    if (root && !root.contains(document.activeElement)) {
      root.querySelector<HTMLElement>(FOCUSABLE)?.focus()
    }

    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        if (!isBusy.current) escape.current()
        return
      }
      if (e.key !== 'Tab' || !root) return
      const items = [...root.querySelectorAll<HTMLElement>(FOCUSABLE)]
      if (items.length === 0) return
      const first = items[0]
      const last = items[items.length - 1]
      const active = document.activeElement
      if (e.shiftKey && (active === first || !root.contains(active))) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && (active === last || !root.contains(active))) {
        e.preventDefault()
        first.focus()
      }
    }

    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('keydown', onKey)
      if (opener?.isConnected) opener.focus()
    }
  }, [ref, opener])
}

export interface Toast {
  id: number
  kind: 'ok' | 'error'
  message: string
}

let toastSeq = 0

export function useToasts() {
  const [toasts, setToasts] = useState<Toast[]>([])

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id))
  }, [])

  const push = useCallback(
    (kind: Toast['kind'], message: string) => {
      const id = ++toastSeq
      setToasts((prev) => [...prev, { id, kind, message }])
      window.setTimeout(() => dismiss(id), 6000)
    },
    [dismiss],
  )

  return { toasts, push, dismiss }
}
