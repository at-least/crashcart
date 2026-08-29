import { defineConfig } from 'vitepress'

// https://vitepress.dev/reference/site-config
export default defineConfig({
  title: 'CrashCart',
  description: 'Open-source, Sentry SDK compatible error tracking for mobile and web apps.',
  // GitHub Pages serves a project site under /<repo>/.
  base: '/crashcart/',
  lastUpdated: true,
  cleanUrls: true,

  themeConfig: {
    // https://vitepress.dev/reference/default-theme-config
    nav: [
      { text: 'Guide', link: '/guide/getting-started' },
      { text: 'Reference', link: '/export-format' },
    ],

    sidebar: [
      {
        text: 'Guide',
        items: [
          { text: 'Getting started', link: '/guide/getting-started' },
          { text: 'Projects & DSNs', link: '/guide/projects' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Export format (NDJSON)', link: '/export-format' },
        ],
      },
    ],

    socialLinks: [
      { icon: 'github', link: 'https://github.com/crashcartdev/crashcart' },
    ],

    editLink: {
      pattern: 'https://github.com/crashcartdev/crashcart/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },

    search: { provider: 'local' },
  },
})
