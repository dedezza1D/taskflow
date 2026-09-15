import { useRef, useState } from 'react'
import type { DragEvent, ChangeEvent } from 'react'

// Mirrors the API's own ceiling and supported set, so an upload that cannot
// possibly succeed is rejected here instead of after a 25 MiB round trip.
const MAX_BYTES = 25 * 1024 * 1024
const ACCEPTED = new Set([
  'text/plain',
  'application/pdf',
  'image/png',
  'image/jpeg',
  'image/tiff',
  'image/bmp',
])
const ACCEPTED_EXT = /\.(txt|pdf|png|jpe?g|tiff?|bmp)$/i

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

function reject(file: File): string | null {
  if (file.size > MAX_BYTES) {
    return `${file.name} tem ${formatBytes(file.size)} — o limite é 25 MB.`
  }
  // An empty type is common for drag-and-drop; fall back to the extension.
  if (file.type ? !ACCEPTED.has(file.type) : !ACCEPTED_EXT.test(file.name)) {
    return `${file.name} não é um tipo suportado (PDF, texto ou imagem).`
  }
  return null
}

interface Props {
  onUpload: (file: File, priority: string) => Promise<void>
  onReject: (message: string) => void
}

export function UploadZone({ onUpload, onReject }: Props) {
  const [dragging, setDragging] = useState(false)
  const [busy, setBusy] = useState(false)
  const [priority, setPriority] = useState('normal')
  const inputRef = useRef<HTMLInputElement>(null)

  async function submit(file: File | undefined | null) {
    if (!file || busy) return
    const problem = reject(file)
    if (problem) {
      onReject(problem)
      if (inputRef.current) inputRef.current.value = ''
      return
    }
    setBusy(true)
    try {
      await onUpload(file, priority)
    } finally {
      setBusy(false)
      if (inputRef.current) inputRef.current.value = ''
    }
  }

  function onDrop(e: DragEvent) {
    e.preventDefault()
    setDragging(false)
    void submit(e.dataTransfer.files?.[0])
  }

  function onPick(e: ChangeEvent<HTMLInputElement>) {
    void submit(e.target.files?.[0])
  }

  return (
    <div
      className={`upload-zone ${dragging ? 'dragging' : ''} ${busy ? 'busy' : ''}`}
      onDragOver={(e) => {
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={onDrop}
    >
      <input
        ref={inputRef}
        type="file"
        accept=".txt,.pdf,.png,.jpg,.jpeg,.tif,.tiff,.bmp,text/plain,application/pdf,image/png,image/jpeg,image/tiff,image/bmp"
        onChange={onPick}
        hidden
      />
      <span className="upload-icon" aria-hidden="true">
        ⇪
      </span>
      <div className="upload-copy">
        <strong>
          {busy ? 'Enviando e analisando…' : 'Arraste um documento aqui'}
        </strong>
        <p>
          ou{' '}
          <button
            type="button"
            className="link"
            onClick={() => inputRef.current?.click()}
            disabled={busy}
          >
            escolha um arquivo
          </button>{' '}
          — PDF, texto ou imagem, até 25 MB
        </p>
      </div>
      <label className="priority-picker">
        prioridade
        <select
          value={priority}
          onChange={(e) => setPriority(e.target.value)}
          disabled={busy}
        >
          <option value="low">baixa</option>
          <option value="normal">normal</option>
          <option value="high">alta</option>
        </select>
      </label>
    </div>
  )
}
