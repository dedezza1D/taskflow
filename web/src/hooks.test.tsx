import { StrictMode } from 'react'
import { renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './api'
import type { ApiDocument, Report } from './api'
import { useReports } from './hooks'

const getReport = vi.fn<(id: string) => Promise<Report>>()

vi.mock('./api', async (importOriginal) => {
  const real = await importOriginal<typeof import('./api')>()
  return { ...real, api: { ...real.api, getReport: (id: string) => getReport(id) } }
})

afterEach(() => getReport.mockReset())

function completed(id: string): ApiDocument {
  return {
    id,
    filename: `${id}.txt`,
    content_type: 'text/plain',
    storage_uri: `fs://documents/${id}/original`,
    status: 'completed',
    created_at: '2026-09-11T12:00:00Z',
    updated_at: '2026-09-11T12:00:00Z',
    version: 1,
  }
}

function report(id: string): Report {
  return {
    document_id: id,
    generated_at: '2026-09-11T12:00:00Z',
    detector_version: 'regex-v2',
    total_findings: 0,
    regulations: [],
  }
}

describe('useReports', () => {
  it('loads a report for every completed document', async () => {
    getReport.mockImplementation(async (id) => report(id))
    const docs = [completed('a'), completed('b'), completed('c')]

    const { result } = renderHook(() => useReports(docs))

    await waitFor(() => expect(result.current.reports.size).toBe(3))
    expect(result.current.unreadable.size).toBe(0)
  })

  // The pool used to fetch exactly one report per worker and then stop: a flag
  // set by StrictMode's simulated unmount was never reset on the remount, so
  // every worker exited after its first await. The inventory sat at four
  // reports and never moved.
  it('survives the StrictMode mount/unmount/remount cycle', async () => {
    getReport.mockImplementation(async (id) => report(id))
    const docs = Array.from({ length: 9 }, (_, i) => completed(`d${i}`))

    const { result } = renderHook(() => useReports(docs), {
      wrapper: StrictMode,
    })

    await waitFor(() => expect(result.current.reports.size).toBe(9))
  })

  // A 500 because the artifact is gone will fail identically forever. Retrying
  // it once per document per poll is how a quiet page reached 209 requests for
  // 32 documents, climbing for as long as the tab stayed open.
  it('does not retry a permanent failure, and says the report is unreadable', async () => {
    getReport.mockRejectedValue(
      new ApiError(500, 'internal_error', 'failed to open report'),
    )
    const docs = [completed('a')]

    const { result, rerender } = renderHook(({ d }) => useReports(d), {
      initialProps: { d: docs },
    })

    await waitFor(() => expect(result.current.unreadable.has('a')).toBe(true))
    expect(getReport).toHaveBeenCalledTimes(1)

    // A new array is what the three-second poll produces; the effect re-runs.
    rerender({ d: [completed('a')] })
    rerender({ d: [completed('a')] })

    await waitFor(() => expect(getReport).toHaveBeenCalledTimes(1))
  })

  // "not ready" is a genuine race against the worker writing the artifact, so
  // it earns another try -- but only a few. A document reported as completed
  // whose report is never ready contradicts the pipeline's own invariant, and
  // retrying that forever is the same unbounded loop wearing a better name.
  it('retries a transient failure, but gives up after a bounded number of tries', async () => {
    getReport.mockRejectedValue(
      new ApiError(404, 'report_not_ready', 'not generated yet'),
    )

    const { result, rerender } = renderHook(({ d }) => useReports(d), {
      initialProps: { d: [completed('a')] },
    })

    for (let i = 0; i < 6; i++) {
      await waitFor(() => expect(getReport).toHaveBeenCalled())
      rerender({ d: [completed('a')] })
    }

    await waitFor(() => expect(result.current.unreadable.has('a')).toBe(true))
    expect(getReport.mock.calls.length).toBeLessThanOrEqual(3)
  })

  it('ignores documents that are not completed', async () => {
    getReport.mockImplementation(async (id) => report(id))
    const docs: ApiDocument[] = [
      { ...completed('queued'), status: 'uploaded' },
      { ...completed('broken'), status: 'failed' },
    ]

    renderHook(() => useReports(docs))

    await waitFor(() => expect(getReport).not.toHaveBeenCalled())
  })
})
