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

    // ── Security & Issues ─────────────────────────────────────────────────────
    {
      type: 'category',
      label: 'Security & Issues',
      collapsed: false,
      items: [
        { type: 'doc', id: 'security',      label: 'Security Policy' },
        { type: 'doc', id: 'known-issues',  label: 'Known Issues' },
      ],
    },
  ],
};

export default sidebars;
