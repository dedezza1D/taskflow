import { useEffect, useState } from 'react'

type Theme = 'system' | 'light' | 'dark'

const NEXT: Record<Theme, Theme> = {
  system: 'light',
  light: 'dark',
  dark: 'system',
}

const LABEL: Record<Theme, string> = {
  system: 'Tema do sistema',
  light: 'Tema claro',
  dark: 'Tema escuro',
}

const GLYPH: Record<Theme, string> = {
  system: '◐',
  light: '☀',
  dark: '☾',
}

export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(
    () => (localStorage.getItem('theme') as Theme | null) ?? 'system',
  )

  useEffect(() => {
    const root = document.documentElement
    if (theme === 'system') root.removeAttribute('data-theme')
    else root.setAttribute('data-theme', theme)
    localStorage.setItem('theme', theme)
  }, [theme])

  return (
    <button
      type="button"
      className="theme-toggle"
      title={LABEL[theme]}
      aria-label={LABEL[theme]}
      onClick={() => setTheme(NEXT[theme])}
    >
      <span aria-hidden="true">{GLYPH[theme]}</span>
    </button>
  )
}
