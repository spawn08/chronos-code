// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  mainSidebar: [
    {
      type: 'category',
      label: 'Get Started',
      collapsed: false,
      items: [
        { type: 'doc', id: 'intro',              label: 'Introduction' },
        { type: 'doc', id: 'getting-started',    label: 'Getting Started' },
        { type: 'doc', id: 'using-chronos-code', label: 'Using Chronos Code' },
        { type: 'doc', id: 'headless',           label: 'Headless and Automation' },
      ],
    },
    {
      type: 'category',
      label: 'Configure',
      collapsed: false,
      items: [
        { type: 'doc', id: 'configuration',      label: 'Configuration' },
        { type: 'doc', id: 'agents-and-skills',  label: 'Agents, Skills and Instructions' },
        { type: 'doc', id: 'security',           label: 'Permissions and Safety' },
        { type: 'doc', id: 'mcp',                label: 'MCP Servers' },
        { type: 'doc', id: 'rollback',           label: 'Turning Features Off' },
      ],
    },
    {
      type: 'category',
      label: 'Guides',
      collapsed: false,
      items: [
        { type: 'doc', id: 'best-practices',     label: 'Best Practices' },
        { type: 'doc', id: 'use-cases',          label: 'Use Cases' },
        { type: 'doc', id: 'deployment',         label: 'Running as a Server' },
      ],
    },
    {
      type: 'category',
      label: 'About',
      collapsed: true,
      items: [
        { type: 'doc', id: 'why-chronos-code',   label: 'Why Chronos Code' },
        { type: 'doc', id: 'comparison',         label: 'How It Compares' },
      ],
    },
  ],
};

export default sidebars;
