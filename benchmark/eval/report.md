# Chronos Code — Token Efficiency Eval Suite

> **Synthetic fixture replay:** These results measure deterministic local tool fixtures, not paired Chronos Code and external baseline-tool model runs. They are not a valid external performance benchmark.

27 tasks, 0 failed contract checks.

- **Aggregate savings**: 73.6% (130860 baseline tokens -> 34551 optimized tokens)
- **System prompt size**: 456 tokens (target <800)
- **Routed to a T1 (cheap-model) agent**: 0/27 tasks

| Task | Category | Difficulty | Baseline | Optimized | Savings | Route | Status |
|---|---|---|---|---|---|---|---|
| bugfix-easy-1 | bugfix | easy | 2148 | 996 | 53.6% | - | OK |
| bugfix-easy-2 | bugfix | easy | 2148 | 1012 | 52.9% | - | OK |
| bugfix-easy-3 | bugfix | easy | 2148 | 996 | 53.6% | - | OK |
| bugfix-medium-1 | bugfix | medium | 4218 | 1482 | 64.9% | - | OK |
| bugfix-medium-2 | bugfix | medium | 4218 | 1482 | 64.9% | - | OK |
| bugfix-medium-3 | bugfix | medium | 4218 | 1482 | 64.9% | - | OK |
| bugfix-hard-1 | bugfix | hard | 8174 | 1365 | 83.3% | - | OK |
| bugfix-hard-2 | bugfix | hard | 8174 | 1345 | 83.5% | - | OK |
| bugfix-hard-3 | bugfix | hard | 8174 | 1357 | 83.4% | - | OK |
| feature-easy-1 | feature | easy | 2148 | 996 | 53.6% | - | OK |
| feature-easy-2 | feature | easy | 2148 | 1012 | 52.9% | - | OK |
| feature-easy-3 | feature | easy | 2148 | 996 | 53.6% | - | OK |
| feature-medium-1 | feature | medium | 4218 | 1482 | 64.9% | - | OK |
| feature-medium-2 | feature | medium | 4218 | 1482 | 64.9% | - | OK |
| feature-medium-3 | feature | medium | 4218 | 1482 | 64.9% | - | OK |
| feature-hard-1 | feature | hard | 8174 | 1365 | 83.3% | - | OK |
| feature-hard-2 | feature | hard | 8174 | 1345 | 83.5% | - | OK |
| feature-hard-3 | feature | hard | 8174 | 1357 | 83.4% | - | OK |
| refactor-easy-1 | refactor | easy | 2148 | 996 | 53.6% | - | OK |
| refactor-easy-2 | refactor | easy | 2148 | 1012 | 52.9% | - | OK |
| refactor-easy-3 | refactor | easy | 2148 | 996 | 53.6% | - | OK |
| refactor-medium-1 | refactor | medium | 4218 | 1482 | 64.9% | - | OK |
| refactor-medium-2 | refactor | medium | 4218 | 1482 | 64.9% | - | OK |
| refactor-medium-3 | refactor | medium | 4218 | 1482 | 64.9% | - | OK |
| refactor-hard-1 | refactor | hard | 8174 | 1365 | 83.3% | - | OK |
| refactor-hard-2 | refactor | hard | 8174 | 1345 | 83.5% | - | OK |
| refactor-hard-3 | refactor | hard | 8174 | 1357 | 83.4% | - | OK |
