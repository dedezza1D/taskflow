import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import { describeError } from './errors'

describe('describeError', () => {
  // The server's details are English by design; showing them verbatim is how
  // "email or password is incorrect" landed on a Portuguese login screen.
  it('translates by code and never echoes the server details', () => {
    const e = new ApiError(
      401,
      'invalid_credentials',
      'email or password is incorrect',
    )
    const text = describeError(e, 'fallback')

    expect(text).toBe('E-mail ou senha incorretos.')
    expect(text).not.toContain('incorrect')
  })

  it('tells validation rules apart by their details', () => {
    const short = new ApiError(
      400,
      'validation_error',
      'password must be at least 12 characters',
    )
    expect(describeError(short, 'x')).toBe(
      'A senha precisa ter pelo menos 12 caracteres.',
    )

    const self = new ApiError(
      400,
      'validation_error',
      'you cannot delete your own account',
    )
    expect(describeError(self, 'x')).toBe(
      'Você não pode remover a própria conta.',
    )
  })

  it('keeps an unknown validation rule in Portuguese', () => {
    const e = new ApiError(400, 'validation_error', 'some brand new rule')
    expect(describeError(e, 'x')).not.toContain('rule')
  })

  it('asks the user to wait when sign-in is throttled, by API or nginx', () => {
    const api = new ApiError(
      429,
      'too_many_attempts',
      'too many sign-in attempts',
    )
    const edge = new ApiError(429, 'http_error', 'Too Many Requests')

    expect(describeError(api, 'x')).toContain('Muitas tentativas')
    expect(describeError(edge, 'x')).toContain('Muitas tentativas')
  })

  it('says the server did not answer for gateway errors', () => {
    expect(
      describeError(new ApiError(502, 'http_error', 'Bad Gateway'), 'x'),
    ).toContain('servidor não respondeu')
  })

  it('recognises a request that never reached the server', () => {
    expect(describeError(new TypeError('Failed to fetch'), 'x')).toBe(
      'Sem conexão com o servidor.',
    )
  })

  it('falls back to the caller context for anything else', () => {
    expect(
      describeError(
        new ApiError(418, 'teapot', ''),
        'Não foi possível enviar.',
      ),
    ).toBe('Não foi possível enviar.')
  })
})
