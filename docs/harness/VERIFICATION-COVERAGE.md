# Verification coverage and background completion

The completion policy derives required checks from observed source changes. `internal/checks/registry.yaml` defines supported languages, manifests, runners, flags, and inspection bounds.

## Check selection and evidence

- Checks group affected paths by their nearest relevant manifest. Supported selections cover Go, Python, Rust, and JavaScript package scripts using npm, yarn, pnpm, or bun.
- A receipt matches the check family and manifest root. An ancestor module's recursive test command cannot satisfy a nested module's obligation.
- Every affected input needs coverage. Separate targeted checks can jointly satisfy one obligation.
- Scope inference uses the actual command and working directory. Literal `cd directory && check` chains retain their directory. Selector-affecting environment assignments remain visible to inference.
- Unsupported or ambiguous commands record uncertainty. They provide no passing coverage. Explicit trusted-adapter scopes remain supported.
- Later overlapping writes invalidate passing results. Nonrecursive Go package checks also track direct package-input directories and module manifests. Direct sibling additions, edits, and removals invalidate earlier evidence; a subsequent package check can cover a removed Go input.

## Background processes

Completion observes tenant/session-owned shell lifecycle snapshots without consuming output. Exited processes receive a final source scan; tracked verification commands record their terminal receipt.

An active process that can write source introduces a pending background obligation. This includes active tests, builds, unknown scripts, and development servers. Repair instructions request `await_shell` for the existing process and then fresh affected checks. Read-only processes can remain active.

Enforce mode blocks successful completion while obligations remain unmet. Report mode allows completion and appends an explicit note listing unresolved checks or background processes, including disabled-repair and repeated-failure exits.

## Conservative limits

- Command inference recognizes bounded literal commands, registered options, and inspected simple package scripts. Custom wrappers such as `make test`, filtered Go tests, virtual-environment runner paths, dynamic scripts, and unsupported selectors may require a supported command or explicit trusted-adapter coverage.
- Source freshness follows the runtime's source/configuration filename allowlist and scan limits. Arbitrary generated data and embedded binary assets need additional input mapping.
- Direct Go package membership is tracked. Complete transitive dependency coverage and symbol-level test mapping remain future work.
- Background ownership covers processes supervised by this session's manager. Adoption after a process restart remains future work.
- Development servers can write through plugins or configuration; they remain potential writers until they finish or the user authorizes stopping them.

These changes improve verification correctness. Cursor parity still requires paired live evaluation on the same tasks and comparable usage data.
