import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'llm-proxy-manifestd',
  description:
    'Headless, manifest-driven priority and concurrency proxy for OpenAI-compatible LLM endpoints.',
  srcDir: 'docs',
  base: '/',
  cleanUrls: true,
  lastUpdated: true,
  sitemap: { hostname: 'https://llm-proxy-manifestd.lerenn.dev' },
  // CNAME and other static files live in website/public.
  vite: { publicDir: '../public' },
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/guide/getting-started' },
      { text: 'GitHub', link: 'https://github.com/lerenn/llm-proxy-manifestd' },
    ],
    sidebar: [
      {
        text: 'Guide',
        items: [
          { text: 'Getting Started', link: '/guide/getting-started' },
          { text: 'Helm Values', link: '/guide/helm-values' },
          { text: 'Dashboard', link: '/guide/dashboard' },
          { text: 'Architecture', link: '/guide/architecture' },
          { text: 'Priority Queues', link: '/guide/priority-queues' },
          { text: 'Releasing', link: '/guide/releasing' },
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: 'https://github.com/lerenn/llm-proxy-manifestd' }],
    search: { provider: 'local' },
    footer: { message: 'Released under the MIT License.', copyright: 'Copyright © Louis Fradin' },
  },
})
