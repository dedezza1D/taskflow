// What a pipeline failure means to the person looking at it. The worker records
// scrubbed, technical errors ("stage ocr: unreadable pdf: xref table: ...");
// those stay available, but the first thing on screen has to be a reason and
// a next step, not a stack of Go error wrapping.

export const STAGE_LABEL: Record<string, string> = {
  ocr: 'leitura (OCR)',
  pii: 'detecção',
  report: 'relatório',
}

export function stageLabel(stage: string | undefined): string {
  return stage ? (STAGE_LABEL[stage] ?? stage) : 'uma etapa anterior'
}

export interface FailureExplanation {
  reason: string
  action: string
}

const RETRY_UPLOAD =
  'Confira se o arquivo abre normalmente no seu computador e envie-o de novo.'

// Ordered: the first pattern that matches the recorded error wins.
const KNOWN: [RegExp, FailureExplanation][] = [
  [
    /over the \d+-page limit/i,
    {
      reason: 'O PDF tem mais páginas do que o limite aceito.',
      action: 'Divida o documento em partes menores e envie cada uma.',
    },
  ],
  [
    /pdf has no pages|neither a text layer nor embedded images/i,
    {
      reason: 'O PDF não tem texto nem imagens que possam ser lidos.',
      action: RETRY_UPLOAD,
    },
  ],
  [
    /unreadable pdf|corrupt pdf/i,
    {
      reason: 'O PDF está corrompido ou não segue o formato PDF.',
      action: `${RETRY_UPLOAD} Exportar ou salvar o arquivo de novo como PDF costuma resolver.`,
    },
  ],
  [
    /tesseract failed|likely corrupt document/i,
    {
      reason:
        'Não foi possível extrair texto da imagem — o arquivo parece corrompido.',
      action: RETRY_UPLOAD,
    },
  ],
  [
    /exceeds \d+ bytes/i,
    {
      reason: 'O arquivo é maior do que o pipeline consegue processar.',
      action: 'Envie uma versão menor do documento.',
    },
  ],
  [
    /unsupported content type/i,
    {
      reason: 'O conteúdo do arquivo não corresponde a um tipo suportado.',
      action: 'Envie PDF, texto ou imagem (PNG, JPEG, TIFF ou BMP).',
    },
  ],
  [
    /deadline|timeout|timed out|cancelled/i,
    {
      reason:
        'A análise passou do tempo limite — comum em digitalizações grandes ou pesadas.',
      action: 'Tente enviar em partes menores ou com resolução mais baixa.',
    },
  ],
  [
    /original object missing/i,
    {
      reason:
        'O arquivo original não estava mais disponível quando a análise começou.',
      action: RETRY_UPLOAD,
    },
  ],
  [
    /tesseract not installed/i,
    {
      reason: 'O servidor não tem o leitor de OCR instalado.',
      action: 'Avise o administrador da instalação.',
    },
  ],
]

export function explainFailure(
  stage: string | undefined,
  lastError: string | null | undefined,
): FailureExplanation {
  if (lastError) {
    for (const [pattern, explanation] of KNOWN) {
      if (pattern.test(lastError)) return explanation
    }
  }
  return {
    reason: `A análise parou na etapa de ${stageLabel(stage)}.`,
    action: `${RETRY_UPLOAD} Se voltar a falhar, avise o administrador.`,
  }
}
