import { defineConfig } from 'vitepress'

// https://vitepress.dev/reference/site-config
export default defineConfig({
  title: 'CrashCart',
  description: 'Open-source, Sentry SDK compatible error tracking for mobile and web apps.',
  sitemap: { hostname: 'https://crashcart.app' },
  base: '/',
  lastUpdated: true,
  cleanUrls: true,

  head: [
    ['meta', { property: 'og:title', content: 'CrashCart' }],
    ['meta', { property: 'og:description', content: 'Open-source, Sentry SDK compatible error tracking. One Go binary, one Postgres.' }],
  ],

  themeConfig: {
    nav: [
      { text: 'Guide', link: '/guide/introduction', activeMatch: '/guide/' },
      { text: 'Install', link: '/deploy/docker', activeMatch: '/deploy/' },
      { text: 'Reference', link: '/reference/cli', activeMatch: '/reference/' },
    ],

    sidebar: {
      '/guide/': [
        {
          text: 'Start here',
          items: [
            { text: 'Introduction', link: '/guide/introduction' },
            { text: 'Compared to Sentry', link: '/guide/compared-to-sentry' },
            { text: 'Getting started', link: '/guide/getting-started' },
            { text: 'Projects & DSNs', link: '/guide/projects' },
            { text: 'Connect an SDK', link: '/guide/sdks' },
          ],
        },
        {
          text: 'Day to day',
          items: [
            { text: 'The viewer', link: '/guide/viewer' },
            { text: 'Issues & grouping', link: '/guide/issues' },
            { text: 'Releases & release health', link: '/guide/releases' },
            { text: 'Symbolication', link: '/guide/symbolication' },
            { text: 'Alerts', link: '/guide/alerts' },
          ],
        },
      ],
      '/deploy/': [
        {
          text: 'Install',
          items: [
            { text: 'Docker Compose on a VPS', link: '/deploy/docker' },
            { text: 'Go binary + systemd', link: '/deploy/binary' },
            { text: 'Kubernetes', link: '/deploy/kubernetes' },
          ],
        },
        {
          text: 'Operate',
          items: [
            { text: 'Before going live', link: '/deploy/checklist' },
            { text: 'Security & privacy', link: '/deploy/security' },
            { text: 'Configuration', link: '/deploy/configuration' },
            { text: 'Database & object store', link: '/deploy/postgres' },
            { text: 'Operations', link: '/deploy/operations' },
          ],
        },
      ],
      '/reference/': [
        {
          text: 'Reference',
          items: [
            { text: 'CLI', link: '/reference/cli' },
            { text: 'HTTP API', link: '/reference/api' },
            { text: 'Export format', link: '/reference/export-format' },
            { text: 'SDK compatibility', link: '/reference/sdks' },
            { text: 'Glossary', link: '/reference/glossary' },
          ],
        },
      ],
    },

    socialLinks: [
      { icon: 'github', link: 'https://github.com/crashcartapp/crashcart' },
    ],

    editLink: {
      pattern: 'https://github.com/crashcartapp/crashcart/edit/main/docs/:path',
      text: 'Edit this page on GitHub',
    },

    search: { provider: 'local' },

    footer: {
      message: 'Released under the MIT License.',
    },
  },
})
