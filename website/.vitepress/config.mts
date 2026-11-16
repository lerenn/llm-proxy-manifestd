import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'LLM Proxy ManifestD',
  description:
    'Kubernetes-native priority and concurrency proxy for OpenAI-compatible LLM endpoints: deployed with Helm, configured by a hot-reloading ConfigMap, with optional CLI sidecars.',
  srcDir: 'docs',
  base: '/',
  cleanUrls: true,
  lastUpdated: true,
  sitemap: { hostname: 'https://llm-proxy-manifestd.lerenn.dev' },
  // CNAME and other static files live in website/public.
  vite: { publicDir: '../public' },
  head: [['link', { rel: 'icon', type: 'image/png', href: '/favicon.png' }]],
  themeConfig: {
    logo: '/logo.svg',
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
          { text: 'Multi-pool routing', link: '/guide/multi-pool-routing' },
          { text: 'Protocol endpoints & checker', link: '/guide/protocol-endpoints' },
          { text: 'Subscription providers', link: '/guide/subscription-providers' },
          { text: 'Releasing', link: '/guide/releasing' },
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: 'https://github.com/lerenn/llm-proxy-manifestd' }],
    search: { provider: 'local' },
    footer: { message: 'Released under the MIT License.', copyright: 'Copyright © Louis Fradin' },
  },
})
