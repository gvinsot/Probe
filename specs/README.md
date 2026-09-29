# Current product specifications

These specifications describe the source on `main`, checked against commit
`23a1d2e` on 2026-09-27. They describe implemented behavior and its limits;
they do not assert that every published release or running deployment has it.

| Specification | Scope |
| --- | --- |
| [CLI and evidence contract](probe-v0.4-spec.md) | `app/`: Git comparison, policy, deterministic analysis, controlled reviewer, sandbox, coverage, F1–F9 evidence stages, reports and exit codes. |
| [Hub, website and deployment](probe-hub-spec.md) | `hub/`, `web/`, `devops/`: account flows, monitored repositories, report viewer, security boundaries, static website and shipped deployment configuration. |

The CLI specification is self-contained. Its filename and F/R identifiers stay
stable for links from feature documentation. The initial V0 proposal and V0.2
specification have been removed; their history remains in Git. There is no
requirement to combine old specifications with the current contract.

The [CLI guide](../app/README.md), [Hub guide](../hub/README.md),
[report schema](../app/schema/confidence-report.schema.json) and
[release notes](../app/docs/releases/) serve different purposes: usage,
operations, machine format and release history. Keep those documents when
removing obsolete product proposals.

Each specification links its implementation and existing checks. When behavior
changes, update the relevant contract and its source/test references together.
Keep implemented scope, known limitations and future work distinct; a listed
acceptance criterion is not a claim that its check ran in the current environment.
