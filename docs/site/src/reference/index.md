---
description: Detailed reference for Stillwater's settings, providers, rules, and environment variables.
---

# Reference

The map for "where do I find the exact list of X?" Each page is a deep enumeration; for the *concepts*, follow the cross-links into [core concepts](../core-concepts/index.md).

<div class="grid cards" markdown>

- __Settings, by tab__

    ---

    A guided tour of every tab in Settings: General, Providers, Connections, Libraries, Automation, Rules, Users, Auth Providers, Maintenance, Logs, and Updates.

    [Read more](settings-by-tab.md)

- __Rules catalog__

    ---

    All 26 built-in rules with their defaults, configurable knobs, and what each fix does.

    [Read more](rules-catalogue.md)

- __Providers__

    ---

    The capability matrix (generated from the provider definitions in the codebase), the fallback chain semantics, per-field aggregation, and the auth tiers.

    [Read more](providers.md)

- __Environment variables__

    ---

    Every `SW_` environment variable Stillwater honors at startup, with defaults and YAML field equivalents.

    [Read more](environment-variables.md)

- __CLI reference__

    ---

    Every command-line flag and subcommand the stillwater binary accepts, with types, defaults, and descriptions. Generated from the Go flags registry.

    [Read more](cli.md)

- __Preferences reference__

    ---

    Every user preference key, its default value, and its allowed values or numeric range. Generated from the Go preference registry.

    [Read more](preferences.md)

- __UI label glossary__

    ---

    Exact wording for sidebar navigation, Settings tabs, artist page sections, image type names, and key action buttons. Use when writing docs to match what users see.

    [Read more](ui-labels.md)

</div>

## API Reference

The complete REST API specification is published separately and rendered from `api/openapi.yaml`. It covers every endpoint Stillwater exposes under `/api/v1/`, including request bodies, response shapes, and authentication. The Web UI consumes the same API via HTMX, so anything the UI can do can also be scripted.

[Open the API reference &rarr;](../api/index.md){ .md-button }
