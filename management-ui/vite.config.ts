import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import { viteSingleFile } from 'vite-plugin-singlefile';
import path from 'path';
import fs from 'fs';

// The server serves this exact file name from MANAGEMENT_STATIC_PATH (or <config dir>/static).
const OUTPUT_FILE_NAME = 'management.html';

// Version shown on the System page: VERSION env first, then package.json.
// Git tags are not used because this folder lives inside the CLIProxyAPI repository,
// whose tags describe the Go server rather than this panel.
function getVersion(): string {
  if (process.env.VERSION) {
    return process.env.VERSION;
  }
  try {
    const pkg = JSON.parse(fs.readFileSync(path.resolve(__dirname, 'package.json'), 'utf8'));
    if (pkg.version && pkg.version !== '0.0.0') {
      return pkg.version;
    }
  } catch {
    // package.json not readable
  }
  return 'dev';
}

// Rename after vite-plugin-singlefile has inlined everything and the file is on disk.
// Mutating the in-memory bundle is not supported by Rolldown, so this works on the output.
function renameHtmlOutput(fileName: string): Plugin {
  let outDir = 'dist';
  return {
    name: 'rename-html-output',
    apply: 'build',
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      const source = path.join(outDir, 'index.html');
      if (fs.existsSync(source)) fs.renameSync(source, path.join(outDir, fileName));
    },
  };
}

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [
    react(),
    viteSingleFile({
      removeViteModuleLoader: true,
    }),
    renameHtmlOutput(OUTPUT_FILE_NAME),
  ],
  define: {
    __APP_VERSION__: JSON.stringify(getVersion()),
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    // `pnpm dev` talks to a running CLIProxyAPI. The login page accepts the API base URL,
    // so proxying is only needed when you prefer same-origin requests in development.
    proxy: process.env.CLIPROXY_URL
      ? {
          '/v0': { target: process.env.CLIPROXY_URL, changeOrigin: true },
          '/v8': { target: process.env.CLIPROXY_URL, changeOrigin: true },
        }
      : undefined,
  },
  css: {
    modules: {
      localsConvention: 'camelCase',
      generateScopedName: '[name]__[local]___[hash:base64:5]',
    },
    preprocessorOptions: {
      scss: {
        additionalData: `@use "@/styles/variables.scss" as *;`,
      },
    },
  },
  build: {
    target: 'es2020',
    outDir: 'dist',
    assetsInlineLimit: 100000000,
    chunkSizeWarningLimit: 100000000,
    cssCodeSplit: false,
    rolldownOptions: {
      output: {
        codeSplitting: false,
      },
    },
  },
});
