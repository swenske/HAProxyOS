import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Builds straight into dashboard/backend/static - go:embed (dashboard/
// backend/main.go) needs the built assets to already exist inside its
// own package directory tree at `go build` time, not a sibling
// directory - see the Makefile's dashboard-frontend-build target, which
// always runs before dashboard-build.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../backend/static',
    emptyOutDir: true,
  },
})
