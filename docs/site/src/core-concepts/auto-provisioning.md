---
description: How Stillwater creates a local account for a federated user on first sign-in, the guard rails that restrict who qualifies, and how the default role is assigned.
---

<!-- code: internal/auth/provider.go (Authenticator.CanAutoProvision, Authenticator.MapRole), internal/auth/provider_emby.go (EmbyProvider.CanAutoProvision, guardRail "admin"/"any_user"), internal/auth/provider_jellyfin.go (JellyfinProvider.CanAutoProvision), internal/auth/provider_oidc.go (OIDCProvider.CanAutoProvision, userGroups, adminGroups), internal/auth/user.go (CreateFederatedUser), internal/api/handlers.go (completeLogin: lookup -> guard rail -> provision -> session), web/templates/settings_auth_providers.templ -->

# Auto-provisioning

**Auto-provisioning** is the mechanism that creates a Stillwater account the first time a federated user signs in. Without it, every new user would need an administrator to pre-create their account manually. With it, a user whose identity passes the configured guard rail gets a Stillwater account automatically on first login.

## The sign-in flow

When a user signs in via Emby, Jellyfin, or OIDC, Stillwater runs through three steps in order:

1. **Credential check** -- Stillwater forwards the credentials to the upstream provider. If the provider rejects them, login fails immediately.
2. **Account lookup** -- Stillwater searches for an existing local account linked to that provider identity. If it finds one, the user logs in.
3. **Provision or reject** -- if no existing account is found, Stillwater consults the guard rail. Pass the guard rail and a new account is created on the spot and a session is issued. Fail it and the login is rejected with "This account is not authorized for this Stillwater instance."

The provisioned account is a permanent Stillwater account. It persists across sessions and is visible in Settings > Users. Stillwater does not re-create or re-provision on subsequent sign-ins; the existing account is found at step 2 and reused.

## Guard rails

A guard rail is the condition an incoming identity must satisfy before auto-provisioning fires. Guard rails exist to prevent anyone with a valid upstream account from walking into your Stillwater instance.

### Emby and Jellyfin guard rails

Two options:

- **Admins only** -- only users who are administrators on the upstream server qualify. This is the default. If the upstream server marks the user as an admin, Stillwater provisions them as an administrator (ignoring the configured default role for that connection). If the upstream server marks them as a regular user, login is rejected.
- **Any user** -- any user who can authenticate against the upstream server qualifies. Combined with a permissive Emby or Jellyfin installation, this means anyone on the upstream server can sign in.

The guard rail setting lives under Settings > Auth providers > Emby (or Jellyfin) > Guard rail (see [`settings-auth-providers-auth-emby-guard-rail`](../reference/settings-by-tab.md#settings-auth-providers-auth-emby-guard-rail) and [`settings-auth-providers-auth-jellyfin-guard-rail`](../reference/settings-by-tab.md#settings-auth-providers-auth-jellyfin-guard-rail)).

### OIDC guard rails

OIDC works differently because the IdP does not expose a simple admin flag. Two guard rail knobs:

- **User groups** -- a list of IdP group names. A user must belong to at least one of them to qualify. If the list is empty, any authenticated user qualifies.
- **Admin groups** -- a list of IdP group names. Membership in any of these groups maps the user to the `administrator` role, overriding the default role.

Both lists use case-insensitive matching. An empty user groups list is the broadest guard rail: any IdP user can be provisioned.

For group claims to work, the IdP must include a `groups` claim in its ID tokens. Some providers (Authentik, Keycloak) support this; others (Okta, Azure AD in some configurations) may not include it by default. If admin group mapping has no effect, check whether your IdP is emitting group claims.

## Default role

When provisioning fires and the guard rail passes, Stillwater assigns the new account a role. The role comes from two sources in priority order:

1. **Role mapping** -- if the provider signals admin status (Emby/Jellyfin: `IsAdministrator`; OIDC: `adminGroups` membership), the account is created as `administrator`.
2. **Default role setting** -- otherwise, the configured default role for the connection is used. Both Emby and Jellyfin connections have a default role setting (see [`settings-auth-providers-auth-default-role-emby`](../reference/settings-by-tab.md#settings-auth-providers-auth-default-role-emby) and [`settings-auth-providers-auth-default-role-jellyfin`](../reference/settings-by-tab.md#settings-auth-providers-auth-default-role-jellyfin)); the OIDC provider has its own (see [`settings-auth-providers-auth-default-role-oidc`](../reference/settings-by-tab.md#settings-auth-providers-auth-default-role-oidc)).

The default roles are `administrator` and `operator`. Operators have full access to library management but cannot change system settings.

## Display name sync

After a federated user signs in to their existing account, Stillwater checks whether their display name changed on the upstream provider. If it did, the local account's display name is updated to match. This is a best-effort operation and non-fatal if it fails; the session is issued regardless.

## What happens when an upstream user is removed

Stillwater does not delete or deactivate local accounts when the upstream account disappears. The user's next sign-in attempt will fail at step 1 (credential check) because the upstream provider rejects the credentials. The local account stays in the database and must be deactivated manually from Settings > Users if you want to revoke access. Sessions that were issued before the upstream removal remain valid until they expire naturally (24 hours from issue).

## Enabling auto-provisioning

Auto-provisioning is disabled by default for every provider. To enable it:

- For Emby: Settings > Auth providers > Emby > Enable auto-provisioning (see [`settings-auth-providers-auth-auto-provision-emby`](../reference/settings-by-tab.md#settings-auth-providers-auth-auto-provision-emby))
- For Jellyfin: Settings > Auth providers > Jellyfin > Enable auto-provisioning (see [`settings-auth-providers-auth-auto-provision-jellyfin`](../reference/settings-by-tab.md#settings-auth-providers-auth-auto-provision-jellyfin))
- For OIDC: Settings > Auth providers > OIDC > Enable auto-provisioning (see [`settings-auth-providers-auth-auto-provision-oidc`](../reference/settings-by-tab.md#settings-auth-providers-auth-auto-provision-oidc))

Each provider's auto-provision toggle is independent. You can enable it for Emby without enabling it for Jellyfin or OIDC.
