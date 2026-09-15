import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError, onSessionExpired } from './api'
import { isOutage } from './hooks'

function respond(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  )
}

afterEach(() => vi.unstubAllGlobals())

describe('session expiry', () => {
  // The list poll got a 401 when a session expired and showed "API fora do ar"
  // over a list that stayed on screen. Any call that finds the session gone must
  // tell the app, so it can go back to the login screen.
  it('notifies listeners when a call comes back unauthenticated', async () => {
    const expired = vi.fn()
    const off = onSessionExpired(expired)
    respond(401, { error: 'unauthenticated', details: 'sign in to continue' })

    await expect(api.listDocuments()).rejects.toBeInstanceOf(ApiError)
    expect(expired).toHaveBeenCalledTimes(1)
    off()
  })

  // A wrong password in the change-password form is also a 401, but the session
  // is fine — signing the user out for a typo would be its own bug.
  it('does not treat a wrong password as an expired session', async () => {
    const expired = vi.fn()
    const off = onSessionExpired(expired)
    respond(401, {
      error: 'invalid_credentials',
      details: 'current password is incorrect',
    })

    await expect(
      api.changePassword('wrong', 'a-new-long-password'),
    ).rejects.toMatchObject({
      code: 'invalid_credentials',
    })
    expect(expired).not.toHaveBeenCalled()
    off()
  })

  it('stops notifying after unsubscribe', async () => {
    const expired = vi.fn()
    onSessionExpired(expired)()
    respond(401, { error: 'unauthenticated' })

    await expect(api.listDocuments()).rejects.toBeInstanceOf(ApiError)
    expect(expired).not.toHaveBeenCalled()
  })

  it('keeps the error code for calls that answer 204 on success', async () => {
    respond(400, {
      error: 'validation_error',
      details: 'you cannot delete your own account',
    })
    await expect(api.deleteUser('me')).rejects.toMatchObject({
      code: 'validation_error',
      message: 'you cannot delete your own account',
    })
  })
})

describe('isOutage', () => {
  it('is an outage only when the server did not answer usefully', () => {
    expect(isOutage(new TypeError('Failed to fetch'))).toBe(true)
    expect(isOutage(new ApiError(502, 'http_error'))).toBe(true)
    expect(isOutage(new ApiError(401, 'unauthenticated'))).toBe(false)
    expect(isOutage(new ApiError(403, 'forbidden'))).toBe(false)
  })
})
