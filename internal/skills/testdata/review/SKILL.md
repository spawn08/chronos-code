---
name: focused-review
description: Review a focused change for correctness.
version: 1.0.0
triggers: [review, correctness]
model_hint: sonnet
tools_required: [file_read, file_grep]
---
# Focused Review

Inspect the changed code and report concrete correctness defects.
