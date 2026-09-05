// @ts-check
// `@type` JSDoc annotations allow editor autocompletion and type checking
// (when paired with `@ts-check`).

import { themes as prismThemes } from 'prism-react-renderer';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Chronos Code',
  tagline: 'YAML-native AI coding agent harness built on the Chronos framework',
  favicon: 'img/favicon.ico',

  // GitHub Pages deployment config
  url: 'https://spawn08.github.io',
  baseUrl: '/chronos-code/',

  organizationName: 'spawn08',
  projectName: 'chronos-code',
  deploymentBranch: 'gh-pages',
  trailingSlash: false,

  onBrokenLinks: 'throw',
  onBrokenMarkdownLinks: 'warn',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  markdown: {
    mermaid: true,
  },

  themes: ['@docusaurus/theme-mermaid'],

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          sidebarPath: './sidebars.js',
          editUrl: 'https://github.com/spawn08/chronos-code/tree/main/docs/',
          routeBasePath: '/',
          // Show last git update time on each page
          showLastUpdateTime: true,
          showLastUpdateAuthor: false,
        },
        blog: {
          showReadingTime: true,
          editUrl: 'https://github.com/spawn08/chronos-code/tree/main/docs/',
          blogTitle: 'Chronos Code Announcements',
          blogDescription: 'Release notes and announcements for Chronos Code',
          postsPerPage: 10,
          feedOptions: {
            type: ['rss', 'atom'],
            xslt: true,
          },
        },
        theme: {
          customCss: './src/css/custom.css',
        },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      // Social preview card (shown when sharing the link on social media / GitHub)
      image: 'img/chronos-code-social-card.png',

      // Meta tags for social sharing
      metadata: [
        { name: 'twitter:card', content: 'summary_large_image' },
        { name: 'twitter:site', content: '@spawn08' },
        { name: 'twitter:creator', content: '@spawn08' },
        {
          name: 'description',
          content:
            'Chronos Code — YAML-native AI coding agent harness. Agents, skills, guardrails, MCP, and persistent memory, all driven by YAML config.',
        },
        {
          property: 'og:description',
          content:
            'Chronos Code — YAML-native AI coding agent harness. Agents, skills, guardrails, MCP, and persistent memory, all driven by YAML config.',
        },
        { property: 'og:type', content: 'website' },
      ],

      // Mermaid diagram theme
      mermaid: {
        theme: { light: 'neutral', dark: 'dark' },
        options: {
          fontFamily:
            '"ui-monospace", "SFMono-Regular", "SF Mono", Menlo, Consolas, monospace',
          fontSize: 14,
          flowchart: {
            curve: 'basis',
            padding: 20,
          },
        },
      },

      navbar: {
        title: 'Chronos Code',
        logo: {
          alt: 'Chronos Code Logo',
          src: 'img/logo.svg',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'mainSidebar',
            position: 'left',
            label: 'Docs',
          },
          { to: '/blog', label: 'Blog', position: 'left' },
          {
            href: 'https://github.com/spawn08/chronos-code',
            label: 'GitHub',
            position: 'right',
          },
        ],
      },

      footer: {
        style: 'dark',
        links: [
          {
            title: 'Docs',
            items: [
              { label: 'Introduction',    to: '/' },
              { label: 'Getting Started', to: '/getting-started' },
              { label: 'Configuration',   to: '/configuration' },
              { label: 'Security Policy', to: '/security' },
            ],
          },
          {
            title: 'Architecture',
            items: [
              { label: 'Overview',      to: '/architecture' },
              { label: 'Orchestrator',  to: '/architecture/orchestrator' },
              { label: 'MCP',           to: '/architecture/mcp' },
              { label: 'Memory',        to: '/architecture/memory' },
              { label: 'Planning',      to: '/architecture/planning' },
              { label: 'CLI',           to: '/architecture/cli' },
            ],
          },
          {
            title: 'More',
            items: [
              { label: 'GitHub',      href: 'https://github.com/spawn08/chronos-code' },
              { label: 'Releases',    href: 'https://github.com/spawn08/chronos-code/releases' },
              { label: 'Blog',        to: '/blog' },
            ],
          },
        ],
        copyright: `Copyright © ${new Date().getFullYear()} spawn08. Built with Docusaurus.`,
      },

      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
        additionalLanguages: ['bash', 'yaml', 'go', 'json', 'toml', 'diff'],
      },

      colorMode: {
        defaultMode: 'light',
        disableSwitch: false,
        respectPrefersColorScheme: true,
      },

      // Algolia DocSearch — configure when ready; remove the block to use the local search plugin
      // algolia: {
      //   appId: 'YOUR_APP_ID',
      //   apiKey: 'YOUR_SEARCH_API_KEY',
      //   indexName: 'chronos-code',
      //   contextualSearch: true,
      // },
    }),
};

export default config;
