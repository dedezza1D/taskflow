// defineConfig from vitest/config, not vite: it is the same function widened to
// accept the test block below. With vite's, `tsc -b` rejects the config — which
// npm run build runs and a bare `tsc --noEmit` does not, so the break only
// surfaces at build time.
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  // The units worth testing here are the ones that decide what the operator
  // sees: the inventory arithmetic and the report-fetching pool. Both produced
  // real defects that no test could have caught, because there was no runner.
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.{ts,tsx}'],
  },
  server: {
    // The API has no CORS middleware (it never needed one); in dev the Vite
    // server proxies /api to it so the browser sees a single origin.
    //
    // Points at the API directly rather than through nginx, so this flow works
    // without certificates. Note that Vite itself serves plain HTTP: a session
    // cookie marked Secure will not survive it, so run the API with
    // SECURE_COOKIES=false when developing this way.
    proxy: {
      '/api': {
        target: process.env.VITE_API_TARGET ?? 'http://localhost:8080',
        changeOrigin: true,
        // Accept the self-signed dev certificate when pointed at nginx.
        secure: false,
      },
    },
  },
})
