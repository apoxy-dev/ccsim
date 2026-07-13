import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// fs.allow reaches one level above the app so src can import
// stream/decoder.mjs — the repo's reference decoder — instead of a copy.
export default defineConfig({
  plugins: [react()],
  // host: true binds all interfaces (IPv4 + IPv6) so localhost resolves
  // regardless of which loopback the browser picks.
  server: { host: true, fs: { allow: ['..'] } },
  preview: { host: true },
})
