import { ApiError } from '../api'

// The API speaks English on purpose — it is an API, and its error codes are the
// contract. People using this screen read Portuguese, so the interface
// translates by code and never shows the server's `details` verbatim: that is
// how "email or password is incorrect" ended up next to Portuguese labels.

const BY_CODE: Record<string, string> = {
  invalid_credentials: 'E-mail ou senha incorretos.',
  too_many_attempts:
    'Muitas tentativas de entrar. Aguarde alguns minutos e tente de novo.',
  unauthenticated: 'Sua sessão expirou. Entre novamente.',
  forbidden: 'Seu papel não permite esta ação.',
  email_taken: 'Já existe uma conta com este e-mail.',
  token_used: 'Este link já foi usado. Peça um novo.',
  token_invalid: 'Este link é inválido ou expirou. Peça um novo.',
  upload_too_large: 'O arquivo passa do limite de 25 MB.',
  unsupported_content_type:
    'Tipo de arquivo não suportado. Envie PDF, texto ou imagem (PNG, JPEG, TIFF ou BMP).',
  invalid_multipart: 'O envio chegou incompleto. Tente de novo.',
  read_error: 'Não foi possível ler o arquivo enviado. Tente de novo.',
  not_found: 'Não encontrado — pode ter sido apagado.',
  conflict: 'O documento está sendo atualizado. Tente de novo em instantes.',
  report_not_ready: 'O relatório ainda não está pronto.',
  recovery_disabled:
    'A recuperação de senha não está disponível neste servidor.',
  auth_disabled: 'Esta instalação não usa login.',
  documents_disabled:
    'O armazenamento de documentos não está configurado no servidor.',
  invalid_json: 'A requisição chegou malformada. Recarregue a página.',
  internal_error:
    'Erro interno do servidor. Tente de novo; se persistir, avise o administrador.',
}

// validation_error covers many rules; the details string is the only thing
// that tells them apart.
const VALIDATION: [RegExp, (m: RegExpMatchArray) => string][] = [
  [
    /at least (\d+) characters/,
    (m) => `A senha precisa ter pelo menos ${m[1]} caracteres.`,
  ],
  [/email is required/, () => 'Informe o e-mail.'],
  [/role must be/, () => 'Escolha um papel válido.'],
  [
    /cannot delete your own account/,
    () => 'Você não pode remover a própria conta.',
  ],
  [
    /use \/auth\/password/,
    () => 'Para trocar a sua própria senha, use Senha no topo da tela.',
  ],
  [/priority must be/, () => 'Prioridade inválida.'],
  [/'file' field is required/, () => 'Selecione um arquivo.'],
  [/is reserved/, () => 'Este tipo de tarefa é reservado.'],
  [/invalid (user|document|task) id/, () => 'Identificador inválido.'],
]

export function describeError(e: unknown, fallback: string): string {
  if (!(e instanceof ApiError)) {
    // fetch rejects with a TypeError when the request never got an answer.
    return e instanceof TypeError ? 'Sem conexão com o servidor.' : fallback
  }
  if (e.code === 'validation_error') {
    for (const [pattern, say] of VALIDATION) {
      const m = e.message.match(pattern)
      if (m) return say(m)
    }
    return 'Dados inválidos. Revise o formulário.'
  }
  if (BY_CODE[e.code]) return BY_CODE[e.code]
  // nginx's per-IP limit answers 429 with its own page, so there is no code.
  if (e.status === 429) return BY_CODE.too_many_attempts
  if (e.status >= 502 && e.status <= 504) {
    return 'O servidor não respondeu. Tente de novo em instantes.'
  }
  return fallback
}
