---
description: Detailed reference for Stillwater's settings, providers, rules, and environment variables.
---

# Reference

The map for "where do I find the exact list of X?" Each page is a deep enumeration; for the *concepts*, follow the cross-links into [core concepts](../core-concepts/index.md).

<div class="grid cards" markdown>

- __Settings, by tab__

    ---

    A guided tour of every tab in Settings: General, Providers, Connections, Libraries, Automation, Rules, Users, Auth providers, Maintenance, Logs, and Updates.

    [Read more](settings-by-tab.md)

- __Rules catalogue__

    ---

    All 22 built-in rules with their defaults, configurable knobs, and what each fix does.

    [Read more](rules-catalogue.md)

- __Providers__

    ---

    The capability matrix (generated from the provider definitions in the codebase), the fallback chain semantics, per-field aggregation, and the auth tiers.

    [Read more](providers.md)

- __Environment variables__

    ---

    Every `SW_` environment variable Stillwater honors at startup, with defaults and YAML field equivalents.

    [Read more](environment-variables.md)

</div>

## API Reference

The complete REST API specification is published separately and rendered from `api/openapi.yaml`. It covers every endpoint Stillwater exposes under `/api/v1/`, including request bodies, response shapes, and authentication. The Web UI consumes the same API via HTMX, so anything the UI can do can also be scripted.

[Open the API reference &rarr;](../api/index.html){ .md-button }
