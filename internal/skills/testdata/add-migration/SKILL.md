---
name: add-migration
description: Create and apply a new database migration safely
---

1. Run `make migration name=<short_name>` to create the files.
2. Write both the up and the down migration.
3. Run `make migrate-test` and confirm the down migration restores the schema.
4. Never modify a migration that has already been merged.
