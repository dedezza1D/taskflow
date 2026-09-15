import { renderHook, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type {
  ApiDocument,
  DocumentArtifact,
  Report,
  TaskExecution,
} from './api'
import { useDocumentDetail, useDocuments } from './hooks'

const getDocument =
  vi.fn<
    (
      id: string,
    ) => Promise<{ document: ApiDocument; artifacts: DocumentArtifact[] }>
  >()
const getReport = vi.fn<(id: string) => Promise<Report>>()
const listDocuments = vi.fn<() => Promise<{ items: ApiDocument[] }>>()
const listExecutions =
  vi.fn<(taskId: string) => Promise<{ items: TaskExecution[] }>>()

vi.mock('./api', async (importOriginal) => {
  const real = await importOriginal<typeof import('./api')>()
  return {
    ...real,
    api: {
      ...real.api,
      getDocument: (id: string) => getDocument(id),
      getReport: (id: string) => getReport(id),
      listDocuments: () => listDocuments(),
      listExecutions: (taskId: string) => listExecutions(taskId),
    },
  }
})

afterEach(() => {
  getDocument.mockReset()
  getReport.mockReset()
  listExecutions.mockReset()
  listDocuments.mockReset()
})

describe('useDocuments', () => {
  // Polls skip hidden tabs to save requests, and the first load once skipped
  // with them: a tab opened in the background showed an empty skeleton until it
  // had been visible for up to fifteen seconds.
  it('loads once even when the tab starts hidden', async () => {
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    listDocuments.mockResolvedValue({ items: [doc({ id: 'a' })] })

    const { result, unmount } = renderHook(() => useDocuments())

    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.documents).toHaveLength(1)
    expect(listDocuments).toHaveBeenCalledTimes(1)

    unmount()
    hidden.mockRestore()
  })
})

const reportArtifact: DocumentArtifact = {
  id: 'r',
  document_id: 'd',
  stage: 'report',
  kind: 'report_json',
  storage_uri: 'fs://d/report.json',
  created_at: '2026-09-14T00:00:01Z',
}

function doc(over: Partial<ApiDocument>): ApiDocument {
  return {
    id: 'd',
    filename: 'd.txt',
    content_type: 'text/plain',
    storage_uri: 'fs://d/original',
    status: 'completed',
    task_id: 't',
    created_at: '2026-09-14T00:00:00Z',
    updated_at: '2026-09-14T00:00:01Z',
    version: 3,
    ...over,
  }
}

describe('useDocumentDetail', () => {
  // The worker marks a document completed and destroys its original a few
  // milliseconds later. The pane stopped polling on the first "completed" and
  // stayed on "the original is still stored" for as long as it was open.
  it('keeps polling briefly after completion until the shred is recorded', async () => {
    getDocument
      .mockResolvedValueOnce({ document: doc({}), artifacts: [reportArtifact] })
      .mockResolvedValue({
        document: doc({ raw_shredded_at: '2026-09-14T00:00:02Z' }),
        artifacts: [reportArtifact],
      })
    getReport.mockResolvedValue({
      document_id: 'd',
      generated_at: '2026-09-14T00:00:01Z',
      detector_version: 'regex-v2',
      total_findings: 0,
      regulations: [],
    })
    listExecutions.mockResolvedValue({ items: [] })

    const { result } = renderHook(() => useDocumentDetail('d'))

    await waitFor(
      () => expect(result.current.doc?.raw_shredded_at).toBeTruthy(),
      {
        timeout: 3000,
      },
    )
    // And the report, already known after the first poll, is not fetched again.
    expect(getReport).toHaveBeenCalledTimes(1)
  })

  it('stops waiting once the cap is reached, if the shred never lands', async () => {
    getDocument.mockResolvedValue({
      document: doc({}),
      artifacts: [reportArtifact],
    })
    getReport.mockResolvedValue({
      document_id: 'd',
      generated_at: '2026-09-14T00:00:01Z',
      detector_version: 'regex-v2',
      total_findings: 0,
      regulations: [],
    })
    listExecutions.mockResolvedValue({ items: [] })

    renderHook(() => useDocumentDetail('d'))

    // One poll plus three settle waits, one second apart.
    await waitFor(() => expect(getDocument).toHaveBeenCalledTimes(4), {
      timeout: 5000,
    })
    await new Promise((r) => setTimeout(r, 1500))
    expect(getDocument).toHaveBeenCalledTimes(4)
  }, 10000)
})
