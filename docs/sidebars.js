// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  mainSidebar: [
    // ── Getting Started ──────────────────────────────────────────────────────
    {
      type: 'category',
      label: 'Getting Started',
      collapsed: false,
      items: [
        { type: 'doc', id: 'intro',            label: 'Introduction' },
        { type: 'doc', id: 'getting-started',  label: 'Getting Started' },
        { type: 'doc', id: 'configuration',    label: 'Configuration' },
        { type: 'doc', id: 'rollback',         label: 'Rollback Controls' },
      ],
    },

    // ── Why Chronos Code ─────────────────────────────────────────────────────
    {
      type: 'category',
      label: 'Why Chronos Code',
      collapsed: false,
      items: [
        { type: 'doc', id: 'why-chronos-code', label: 'Why Chronos Code' },
        { type: 'doc', id: 'comparison',       label: 'How It Compares' },
        { type: 'doc', id: 'use-cases',        label: 'Use Cases' },
        { type: 'doc', id: 'best-practices',   label: 'Best Practices' },
      ],
    },

    // ── Architecture ─────────────────────────────────────────────────────────
    {
      type: 'category',
      label: 'Architecture',
      collapsed: false,
      items: [
        { type: 'doc', id: 'architecture/overview',      label: 'Overview' },
        { type: 'doc', id: 'architecture/orchestrator',  label: 'Orchestrator' },
        { type: 'doc', id: 'architecture/mcp',           label: 'MCP' },
        { type: 'doc', id: 'architecture/memory',        label: 'Memory' },
        { type: 'doc', id: 'architecture/planning',      label: 'Planning' },
        { type: 'doc', id: 'architecture/cli',           label: 'CLI' },
        { type: 'doc', id: 'architecture/security',      label: 'Security' },
      ],
    },

    // ── Diagrams ─────────────────────────────────────────────────────────────
    {
      type: 'category',
      label: 'Diagrams',
      collapsed: true,
      items: [
        { type: 'doc', id: 'diagrams/architecture-overview', label: 'Architecture Overview' },
        { type: 'doc', id: 'diagrams/request-lifecycle',     label: 'Request Lifecycle' },
        { type: 'doc', id: 'diagrams/mcp-discovery',         label: 'MCP Discovery' },
        { type: 'doc', id: 'diagrams/context-budget',        label: 'Context Budget' },
        { type: 'doc', id: 'diagrams/orchestrator-phases',   label: 'Orchestrator Phases' },
        { type: 'doc', id: 'diagrams/data-flow',             label: 'Data Flow' },
      ],
    },

    // ── Security ─────────────────────────────────────────────────────────────
    {
      type: 'category',
      label: 'Security',
      collapsed: false,
      items: [
        { type: 'doc', id: 'security', label: 'Security Policy' },
      ],
    },
  ],
};

export default sidebars;
