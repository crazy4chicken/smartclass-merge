import { defineConfig } from 'vitepress'

const base = '/smartclass-merge/'

export default defineConfig({
  title: 'SmartClass Merge',
  description: 'Turn one recording\'s segments into a single playable video',
  base,
  cleanUrls: true,
  lastUpdated: true,
  themeConfig: {
    nav: [
      { text: 'Deployment', link: '/guide/deploy' },
      { text: 'API Reference', link: '/api/overview' },
      {
        text: 'GitHub',
        link: 'https://github.com/crazy4chicken/smartclass-merge'
      }
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Guide',
          items: [
            { text: 'Deployment', link: '/guide/deploy' },
            { text: 'Architecture', link: '/guide/architecture' },
            { text: 'API Usage', link: '/guide/api' },
            { text: 'Development', link: '/guide/development' }
          ]
        }
      ],
      '/api/': [
        {
          text: 'API Reference',
          items: [
            { text: 'Overview', link: '/api/overview' },
            { text: 'Merges', link: '/api/reference/merges' },
            { text: 'Health', link: '/api/reference/health' },
            // Static file, not a route: VitePress leaves its URL untouched,
            // so the deployment base has to be part of the link.
            { text: 'OpenAPI document', link: `${base}openapi.yaml` }
          ]
        }
      ]
    },
    search: {
      provider: 'local'
    }
  }
})
