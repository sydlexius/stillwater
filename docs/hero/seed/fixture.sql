PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE goose_db_version (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		version_id INTEGER NOT NULL,
		is_applied INTEGER NOT NULL,
		tstamp TIMESTAMP DEFAULT (datetime('now'))
	);
INSERT INTO goose_db_version VALUES(1,0,1,'2026-04-24 03:47:21');
INSERT INTO goose_db_version VALUES(2,1,1,'2026-04-24 03:47:21');
INSERT INTO goose_db_version VALUES(3,2,1,'2026-05-06 00:55:51');
INSERT INTO goose_db_version VALUES(4,3,1,'2026-05-06 00:55:51');
INSERT INTO goose_db_version VALUES(5,4,1,'2026-05-07 22:45:06');
INSERT INTO goose_db_version VALUES(6,5,1,'2026-05-14 03:23:16');
INSERT INTO goose_db_version VALUES(7,7,1,'2026-05-20 00:54:14');
INSERT INTO goose_db_version VALUES(8,8,1,'2026-05-20 01:50:52');
INSERT INTO goose_db_version VALUES(9,9,1,'2026-05-22 06:59:58');
INSERT INTO goose_db_version VALUES(10,10,1,'2026-05-22 23:44:20');
INSERT INTO goose_db_version VALUES(11,11,1,'2026-05-23 02:34:08');
INSERT INTO goose_db_version VALUES(12,12,1,'2026-05-25 22:16:27');
INSERT INTO goose_db_version VALUES(13,13,1,'2026-05-26 02:29:12');
INSERT INTO goose_db_version VALUES(14,14,1,'2026-05-28 16:26:42');
INSERT INTO goose_db_version VALUES(15,15,1,'2026-05-28 16:26:42');
INSERT INTO goose_db_version VALUES(16,16,1,'2026-07-01 04:40:29');
INSERT INTO goose_db_version VALUES(17,17,1,'2026-07-05 05:08:25');
INSERT INTO goose_db_version VALUES(18,18,1,'2026-07-05 05:08:25');
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'operator',
    auth_provider TEXT NOT NULL DEFAULT 'local',
    provider_id   TEXT NOT NULL DEFAULT '',
    is_active     INTEGER NOT NULL DEFAULT 1,
    is_protected  INTEGER NOT NULL DEFAULT 0,
    invited_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
, last_login TEXT);
INSERT INTO users VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','herofixture-admin','admin','$2a$10$/8ETaRxXrTvrh7cBMqglwOn7aJNO7zLULi6k6P6sQl/orUZ1AOxt6','administrator','local','',1,1,NULL,'2026-04-24T03:48:01Z','2026-04-24T03:48:01Z','2026-07-06T00:41:17Z');
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL DEFAULT 'read,write',
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at TEXT,
    revoked_at   TEXT,
    status       TEXT NOT NULL DEFAULT 'active'
);
CREATE TABLE audit_log (
    id         TEXT PRIMARY KEY,
    action     TEXT NOT NULL,
    token_id   TEXT REFERENCES api_tokens(id) ON DELETE SET NULL,
    token_name TEXT NOT NULL,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
, actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL, target_user_id TEXT REFERENCES users(id) ON DELETE SET NULL);
CREATE TABLE invites (
    id          TEXT PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    role        TEXT NOT NULL DEFAULT 'operator',
    created_by  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TEXT NOT NULL,
    redeemed_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    redeemed_at TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE connections (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL,
    url TEXT NOT NULL,
    encrypted_api_key TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'unknown',
    status_message TEXT NOT NULL DEFAULT '',
    feature_library_import INTEGER NOT NULL DEFAULT 1,
    feature_nfo_write INTEGER NOT NULL DEFAULT 1,
    feature_image_write INTEGER NOT NULL DEFAULT 1,
    feature_metadata_push INTEGER NOT NULL DEFAULT 0,
    feature_trigger_refresh INTEGER NOT NULL DEFAULT 0,
    feature_manage_server_files INTEGER NOT NULL DEFAULT 0,
    platform_user_id TEXT,
    platform_server_id TEXT NOT NULL DEFAULT '',
    pre_stillwater_config_json TEXT NOT NULL DEFAULT '',
    last_checked_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO connections VALUES('df9c1c55-4824-4a9e-8d21-4c84adcebe68','Emby','emby','http://emby.example.local:8096','',1,'ok','',1,1,1,0,0,1,'','','','2026-07-01T07:25:25Z','2026-04-24T03:48:24Z','2026-07-01T07:25:25Z');
INSERT INTO connections VALUES('ac7b8025-295b-4c2c-86e6-df359196beff','Jellyfin','jellyfin','http://jellyfin.example.local:8096','',1,'ok','',1,1,1,0,0,1,'','','','2026-05-28T17:15:32Z','2026-04-24T03:48:24Z','2026-06-04T07:08:34Z');
INSERT INTO connections VALUES('f6e83fb7-ff1c-4159-83f0-47a92abd9b9d','Lidarr','lidarr','http://lidarr.example.local:8686','',1,'ok','',1,1,1,0,0,1,'','','','2026-05-19T18:37:57Z','2026-04-24T03:48:24Z','2026-05-19T18:37:57Z');
CREATE TABLE IF NOT EXISTS "settings" (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO settings VALUES('provider.priority.biography','["audiodb","wikipedia","discogs","lastfm","genius"]','2026-04-26 01:54:00');
INSERT INTO settings VALUES('provider.priority.genres','["musicbrainz","lastfm","audiodb","discogs","spotify","wikipedia"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.styles','["audiodb","discogs","lastfm","musicbrainz"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.moods','["audiodb","lastfm"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.members','["musicbrainz","wikidata","wikipedia"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.formed','["musicbrainz","wikidata","audiodb"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.born','["musicbrainz","wikidata","wikipedia"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.died','["musicbrainz","wikidata","wikipedia"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.disbanded','["musicbrainz","wikidata","wikipedia"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.years_active','["wikipedia","audiodb","musicbrainz"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.type','["musicbrainz","wikidata","discogs"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.gender','["musicbrainz","wikidata"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.origin','["wikipedia","audiodb","wikidata","musicbrainz"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.thumb','["fanarttv","audiodb","deezer","spotify","duckduckgo"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.fanart','["fanarttv","audiodb","duckduckgo"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.logo','["fanarttv","audiodb","duckduckgo"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('provider.priority.banner','["fanarttv","audiodb","duckduckgo"]','2026-04-24 03:48:24');
INSERT INTO settings VALUES('musicbrainz.contributions','disabled','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('auth.method','local','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('multi_user.enabled','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('onboarding.completed','true','2026-05-27T01:26:00Z');
INSERT INTO settings VALUES('auth.providers.jellyfin.default_role','administrator','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('auto_fetch_images','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('sidebar_show_violations','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('logging.file_path','/config/logs/stillwater.log','2026-04-26T05:41:40Z');
INSERT INTO settings VALUES('ui.artists_view','grid','2026-07-06T00:41:26Z');
INSERT INTO settings VALUES('updater.channel','stable','2026-05-08T01:04:07Z');
INSERT INTO settings VALUES('logging.file_max_files','5','2026-04-26T05:41:40Z');
INSERT INTO settings VALUES('auth.providers.emby.auto_provision','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('rule_schedule.interval_minutes','360','2026-04-24T04:12:21Z');
INSERT INTO settings VALUES('auth.providers.oidc.user_groups','','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('cache.image.max_size_mb','512','2026-06-02T05:51:24Z');
INSERT INTO settings VALUES('auth.providers.emby.default_role','operator','2026-04-24T03:50:42Z');
INSERT INTO settings VALUES('updater.auto_check','true','2026-05-08T01:04:07Z');
INSERT INTO settings VALUES('logging.level','debug','2026-04-26T05:41:40Z');
INSERT INTO settings VALUES('logging.file_max_age_days','30','2026-04-26T05:41:40Z');
INSERT INTO settings VALUES('logging.format','text','2026-04-26T05:41:40Z');
INSERT INTO settings VALUES('onboarding.step','1','2026-05-27T01:25:56Z');
INSERT INTO settings VALUES('provider.wikidata.key_status','ok','2026-05-19 18:22:46');
INSERT INTO settings VALUES('auth.providers.emby.enabled','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('show_platform_debug','false','2026-06-22T03:10:26Z');
INSERT INTO settings VALUES('db_maintenance.last_optimize_at','2026-07-05T00:39:39Z','2026-07-05T00:39:39Z');
INSERT INTO settings VALUES('auth.providers.jellyfin.enabled','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('provider.websearch.duckduckgo.enabled','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('provider.wikipedia.key_status','ok','2026-05-19 18:22:46');
INSERT INTO settings VALUES('auth.providers.emby.guard_rail','admin','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('auth.providers.jellyfin.auto_provision','true','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('auth.providers.oidc.admin_groups','','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('auth.providers.oidc.default_role','operator','2026-04-24T03:48:24Z');
INSERT INTO settings VALUES('auth.providers.oidc.enabled','false','2026-04-24T03:51:36Z');
INSERT INTO settings VALUES('logging.file_max_size_mb','10','2026-04-26T05:41:40Z');
INSERT INTO settings VALUES('provider.fanarttv.key_status','ok','2026-06-13 05:13:44');
INSERT INTO settings VALUES('provider.audiodb.key_status','ok','2026-06-13 05:13:45');
INSERT INTO settings VALUES('provider.lastfm.key_status','ok','2026-05-19 18:22:47');
INSERT INTO settings VALUES('provider.genius.key_status','ok','2026-05-19 18:22:49');
INSERT INTO settings VALUES('provider.name_similarity_threshold','60','2026-04-26T01:56:49Z');
INSERT INTO settings VALUES('onboarding.conflict_check_completed_at','2026-04-28T05:53:27Z','2026-04-28 05:48:08');
INSERT INTO settings VALUES('updater.enabled','true','2026-05-08T01:04:07Z');
INSERT INTO settings VALUES('updater.check_interval_hours','24','2026-05-08T01:04:07Z');
INSERT INTO settings VALUES('auth.providers.local.enabled','true','2026-05-07T22:46:48Z');
INSERT INTO settings VALUES('auth.providers.jellyfin.guard_rail','admin','2026-05-07T22:46:48Z');
INSERT INTO settings VALUES('auth.providers.oidc.auto_provision','false','2026-05-07T22:46:48Z');
INSERT INTO settings VALUES('updater.auto_update','false','2026-05-08T01:04:07Z');
INSERT INTO settings VALUES('metadata_vocab','{"exclude":[],"max_genres":0,"max_styles":0,"max_moods":0}','2026-05-21T03:10:01Z');
INSERT INTO settings VALUES('provider.wikipedia.field_verbosity.biography','intro','2026-05-21 05:06:19');
INSERT INTO settings VALUES('provider.discogs.key_status','ok','2026-06-19 02:25:45');
INSERT INTO settings VALUES('foreign_files.baseline_completed','true','2026-05-27T00:17:16Z');
INSERT INTO settings VALUES('foreign_files.baseline_count','0','2026-05-27T00:17:16Z');
INSERT INTO settings VALUES('provider.musicbrainz.key_status','ok','2026-06-13 05:13:45');
INSERT INTO settings VALUES('provider.musicbrainz.base_url','https://beta.musicbrainz.org/ws/2','2026-06-02 03:01:15');
INSERT INTO settings VALUES('provider.musicbrainz.rate_limit','1','2026-06-02 03:01:15');
INSERT INTO settings VALUES('notif_badge_severity_info','false','2026-06-02T05:38:57Z');
INSERT INTO settings VALUES('provider.spotify.key_status','ok','2026-07-04 02:12:46');
CREATE TABLE libraries (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    path TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'regular' CHECK(type IN ('regular', 'classical')),
    source TEXT NOT NULL DEFAULT 'manual',
    connection_id TEXT REFERENCES connections(id) DEFAULT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    fs_watch INTEGER NOT NULL DEFAULT 0,
    fs_poll_interval INTEGER NOT NULL DEFAULT 60,
    shared_fs_status TEXT NOT NULL DEFAULT '',
    shared_fs_evidence TEXT NOT NULL DEFAULT '',
    shared_fs_peer_library_ids TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
, nfo_lock_data INTEGER NOT NULL DEFAULT 0);
INSERT INTO libraries VALUES('5dc6bbb4-d0b8-4ad3-a5a0-e94e0389a136','Music','/tmp/hero-1756/fixture/library','regular','manual',NULL,'',1,60,'','','','2026-04-24T03:59:37Z','2026-04-30T03:58:02Z',0);
INSERT INTO libraries VALUES('9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','Classical','/tmp/hero-1756/fixture/library','regular','manual',NULL,'',1,60,'','','','2026-04-24T03:59:56Z','2026-05-07T23:30:25Z',0);
INSERT INTO libraries VALUES('a253cc28-8c18-4385-8467-bcd0fd59f1b7','Emby Library','/media/music','regular','emby','df9c1c55-4824-4a9e-8d21-4c84adcebe68','emby-lib-1',0,60,'','','','2026-05-28T15:00:43Z','2026-05-28T15:00:43Z',0);
INSERT INTO libraries VALUES('d025dfd3-4dee-4ef3-ad96-235e0ca7d878','Jellyfin Library','/media/classical','regular','jellyfin','ac7b8025-295b-4c2c-86e6-df359196beff','jellyfin-lib-1',0,60,'','','','2026-05-28T15:00:48Z','2026-05-28T15:00:48Z',0);
CREATE TABLE artist_provider_ids (
    artist_id  TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    provider_id TEXT NOT NULL DEFAULT '',
    fetched_at TEXT,
    PRIMARY KEY (artist_id, provider)
);
INSERT INTO artist_provider_ids VALUES('fd7468af-c24f-456e-aef5-bd2b87de269c','musicbrainz','6f81a7dc-be31-4498-ae95-6d994ffec614',NULL);
INSERT INTO artist_provider_ids VALUES('fd7468af-c24f-456e-aef5-bd2b87de269c','audiodb','113824',NULL);
INSERT INTO artist_provider_ids VALUES('fd7468af-c24f-456e-aef5-bd2b87de269c','discogs','901359',NULL);
INSERT INTO artist_provider_ids VALUES('fd7468af-c24f-456e-aef5-bd2b87de269c','wikidata','Q175044',NULL);
INSERT INTO artist_provider_ids VALUES('fd7468af-c24f-456e-aef5-bd2b87de269c','deezer','5269',NULL);
INSERT INTO artist_provider_ids VALUES('fd7468af-c24f-456e-aef5-bd2b87de269c','spotify','0DrXhci3WAyo0WJv1RBOG6',NULL);
INSERT INTO artist_provider_ids VALUES('40147a87-1eac-4894-8acd-08562bfb3272','musicbrainz','ad79836d-9849-44df-8789-180bbc823f3c',NULL);
INSERT INTO artist_provider_ids VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','musicbrainz','be50643c-0377-4968-b48c-47e06b2e2a3b',NULL);
INSERT INTO artist_provider_ids VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','audiodb','114963',NULL);
INSERT INTO artist_provider_ids VALUES('a17e6b40-759f-4d75-a791-867372deaca6','musicbrainz','27870d47-bb98-42d1-bf2b-c7e972e6befc',NULL);
INSERT INTO artist_provider_ids VALUES('a17e6b40-759f-4d75-a791-867372deaca6','audiodb','115193',NULL);
INSERT INTO artist_provider_ids VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','musicbrainz','c70d12a2-24fe-4f83-a6e6-57d84f8efb51',NULL);
INSERT INTO artist_provider_ids VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','audiodb','118554',NULL);
INSERT INTO artist_provider_ids VALUES('c849e893-f623-4ae5-b477-e37f254d216c','musicbrainz','1f9df192-a621-4f54-8850-2c5373b7eac9',NULL);
INSERT INTO artist_provider_ids VALUES('c849e893-f623-4ae5-b477-e37f254d216c','audiodb','114268',NULL);
INSERT INTO artist_provider_ids VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','musicbrainz','b972f589-fb0e-474e-b64a-803b0364fa75',NULL);
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','musicbrainz','24f1766e-9635-4d58-a4d4-9413f9f98a4c','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','audiodb','114265','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','discogs','95537','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','wikidata','Q1339','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','deezer','1900','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','spotify','5NN2Gn4lgn6pBEF35ti7dP','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','wikipedia','','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','lastfm','','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','genius','','2026-07-06T00:10:04Z');
INSERT INTO artist_provider_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','fanarttv','','2026-07-06T00:10:04Z');
CREATE TABLE artist_images (
    id          TEXT NOT NULL PRIMARY KEY,
    artist_id   TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    image_type  TEXT NOT NULL,
    slot_index  INTEGER NOT NULL DEFAULT 0,
    exists_flag INTEGER NOT NULL DEFAULT 0,
    low_res     INTEGER NOT NULL DEFAULT 0,
    placeholder TEXT NOT NULL DEFAULT '',
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    phash       TEXT NOT NULL DEFAULT '',
    file_format TEXT NOT NULL DEFAULT '',
    source         TEXT NOT NULL DEFAULT '',
    last_written_at TEXT NOT NULL DEFAULT '',
    -- Per-image lock flag. When set, automated refresh and replacement operations
    -- must not overwrite this slot. Round-trips to Emby's LockData for artist images.
    locked         INTEGER NOT NULL DEFAULT 0,
    UNIQUE(artist_id, image_type, slot_index)
);
INSERT INTO artist_images VALUES('3bf3b99f-7987-4d7b-b124-5aede5cec73d','fd7468af-c24f-456e-aef5-bd2b87de269c','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AM5o2KJnawxwUPJJ7GrP294sq0QBI47mqShCcliPSmsACpyT60rDuf/Z',1000,1000,'','','','',0);
INSERT INTO artist_images VALUES('4cf82c1f-7960-4834-b738-bff062e65ca3','fd7468af-c24f-456e-aef5-bd2b87de269c','fanart',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AIIbUbcNw/XHtgf41FPAQTgdKfHcxKZWaU7nHHyng5yRTLq6SXIj+7u3D696BWP/2Q==',1920,1080,'','','','',0);
INSERT INTO artist_images VALUES('d0a0d7e7-5380-4fd2-97e3-582a056845b4','fd7468af-c24f-456e-aef5-bd2b87de269c','fanart',1,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('9f8283fa-5deb-4b29-918c-897efa496adc','fd7468af-c24f-456e-aef5-bd2b87de269c','fanart',2,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('bd141ef1-eb30-4913-97ec-3ee9e7a4d6b0','fd7468af-c24f-456e-aef5-bd2b87de269c','fanart',3,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('ca6478da-68aa-4bed-a48c-40e787019003','fd7468af-c24f-456e-aef5-bd2b87de269c','fanart',4,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('9dd6e819-4105-4c15-8d5e-ec7478b8f061','fd7468af-c24f-456e-aef5-bd2b87de269c','fanart',5,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('f6520e85-d359-4b81-8932-7ea1ac1b10e3','fd7468af-c24f-456e-aef5-bd2b87de269c','logo',0,1,1,'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAADpElEQVR4nITTW2gcVQDG8XPOzszZnezO7CWb7jWbrLU0CW3SRtGCeKm2DxK8gIJQkiLUUsGXPvkiiIi+aTAiQktrSSVKjC2UBEU00tiktpuQLLSNNt1Lbs11szs79zlnZsTbS1/6Pf7he/whAAA4N/jpY0NDX/SdPNnNgv82OPh568DAAAYPW3s74G7dnrqamx13R0e/fun/Pj+fOz09PRF/2B/Oz984WiiXv5yYuPrL4Reejvz8w1hfsDHa2fPiK6cKhcL4+v2lRY7jYK1WuSGK4iEboLBLHcNx0AqELs8omnq4VCr96mXR95hj3332+SNPxWLpi6IYxAe6unusjjavJNe219aW3+po6/xmbX2V+Hz+IKVg03YIYHRd232gq/NQQHjymGlZalCIZJuiiaDjOCxiEfA1YMAwXFhT9XcSiUxoq1LRBVHgJElJ2TYwGAg8Pmrb7MZGFWKMDBfTaD6fn6GUBkzLtAmxpGQ63erzNjx37dpU2YE0vLS4pEeiUSd3c/p3RlFURVal63JdMUJhURAEnkvEE9M6MT3QBfdVVV1xXPqaqhgihHAyFU+dbk6mzdxcbjYYEc+iRCLFhYLhYqal1bUpnBSFqAuRx88gFiMEAAKIuMAQEOMup1LNJs/zHKWOoirab+lkIoJclyLHsUM+jOMW0e9Uq1ULY87rQQyuVmtcpbZNMOaCDMPq1ep2Q7FYsM6fv/BVx96220Ig0oQ8DPRwLNOayWT2t7aku0xTl1wA/Lqh4lgshiKRMM9AVvQgpPl5v8hybPGz/jPv+4MhHQAUYCyTylOTU3dnZ/Pepli4w8vz44RQAUFo2I6tAGifsogbAQCqdXWH93mxOHLp7Im6tLWpyhZCxCEbnQf3h7J7Mo4oCjVN1uytyqZfqku6pilLmZbkE3JdrkBoK64NlEhjo9J18PE3ieXuIoToyDS1NWrbcUmSw6ZpZfmAH/DeBjOZTFoLf/x5Zy5/q76wcO+nQrG0I4jB2urK2s3F8jLFHN6n1NV1tL6+eZ338Y3AdldZFjuJXemOUrmUNwyNiiK/ceHMUO/QxcsfvX3ivcvUJXqxXBz5bnj4EgDoyExuZs4zMnxlqbfvjZd3P/Lo0dHRHz9xHDcwdmXsW4aF+o6sVAb6z02urm7UCCHKvs69nC5b27FsTLt3t7Dn4w/7+/8Rdfz4613Hel/9AAAQEAQh/K/Sdn93d7f4AD4GAMD39DzTmM3Gm/8OfwUAAP//o0jDRokMlesAAAAASUVORK5CYII=',399,79,'','','','',0);
INSERT INTO artist_images VALUES('11b3c531-1ce7-4e03-b849-555311169627','fd7468af-c24f-456e-aef5-bd2b87de269c','banner',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AEtIwsQ3Yyef0FRXqoRlQBt5PvU6lTM6k4CAj+QqK5wFLnoWC0CP/9k=',1000,185,'','','','',0);
INSERT INTO artist_images VALUES('9056f082-8d4e-434c-a07e-a76820b7f950','40147a87-1eac-4894-8acd-08562bfb3272','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AIdOwbsZGcAkfWlvSGmyOpHI9Kpq7o6tGcEd6tTyI/Me0k8uTwSfQUhtH//Z',1000,1000,'','','','',0);
INSERT INTO artist_images VALUES('c352d02b-cd9f-4d62-a357-5191a78ff06f','40147a87-1eac-4894-8acd-08562bfb3272','logo',0,0,0,'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAAC1ElEQVR4nJyTXUhbVxzAz/1MXNQkxswtzq85Bw5BFJXpw8aY6DZkUyZs4NjDxkCUPQzGyAbDsacIAwf1oSktpS3VIj4U6oPEB1v8SIUg3tBCtWg0jcnN1703N/fr5N6bUxQUEaXg/+nw//hx+J3zJ8ElgRDCFpmNERzFPiMwK6Uh6vpAW//6+b5LAWvb279zyezGSP/wz2MTE6Vulwu/qA87OQQYxlbhdqMOj0cJH7LjbCLB9XW0zZ7UNxN7dQUtM4hwbQXgyn53zRfcUf6U2lxb+z+eh017IvwQN4zOs8NhIeyEYuqv2EE6VY4XfR4Lmj6pHQM2o7sdOoRdzPrjbShzA2wy7j97TVVQHRrMz3376VfzkoKtC7yiBYM3K04dsInIe8hZda+6vZ1IZxJ6b1dX8Cwgq2fjNIb1hSJBD5fLM0CRn1cSuGMOoRy2tDrr0anSFqfdaSlgNjYdY+Hw51+Gz8uafzLT9L67ZFrhxQOUTe4XiqQjDKsCJF1QWqw20q4q/A4i4EcuN5a+yHZzJUmlksKyIEgviJzk0k0sXi2nGRzLiR7a1EkbYb5DF0VVlKTs6rNHrSvhBac/FKJuLy9bjwBFzRiWRV0aGvr1IYFANK6AAAtoiC3cmvje/UEjLpu4lTP0lbdKy1stRf0TmiK30qJhNJShXhKAJcLUv9nY4f90WVC9JnKAE2Bq1DvJ4IpuCZs52EgoOqizV4wYsvFulYliSlp101CP4LJhQWJh8JCV/qspL3GYUO/hs9LHuqq1Hj8jEvl6KEpNgkmt8bxMmqoRwXCqfTcau/E2DToTHNxKZWQGM8hqAmp/k0WQj2bFJQkW48c/cc3/mx+jylDPT/+OHiUWF2e6cxkZfffDL099Pp/d6/XmAnevNUDT+PElK9x/lRIPp6am1FO7Ow/GQpuzf3x92U68KUhO1PCoRgSvCgB3Jsf/ufIwAOB1AAAA//9/JnHwZLUsvAAAAABJRU5ErkJggg==',686,296,'','','','',0);
INSERT INTO artist_images VALUES('634a09dd-b923-4ad0-b71c-55583a2a79dc','40147a87-1eac-4894-8acd-08562bfb3272','banner',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AGRlRoTLnliTj8apsgEbjn5QD+NWPM26Oqf3if51XmmBRlCj61KGz//Z',1000,185,'','','','',0);
INSERT INTO artist_images VALUES('614f8e06-fd04-4df2-94ba-02591f9b2f4e','ff6b4132-cfb2-46cf-b476-643c9ea00069','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AHXWWjIVsMORzUdg0zyMj5IUZz6VHOQ8gycYGTz2p0F4YpmO0CNv4R2qCj//2Q==',1000,1000,'','','','',0);
INSERT INTO artist_images VALUES('ac4d2500-8cab-4fd8-b813-15ab1522b32a','ff6b4132-cfb2-46cf-b476-643c9ea00069','fanart',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AEul3xOMZ9D6VW07d57IRwVzT7p2DALyTxViwRY55EYgybQTjt7UAf/Z',1920,1080,'','','','',0);
INSERT INTO artist_images VALUES('25c0034a-3530-4084-a8f0-d865a1dc2c76','ff6b4132-cfb2-46cf-b476-643c9ea00069','logo',0,0,1,'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAACd0lEQVR4nJySzUsbQRjG52MTdzfraj5IDBtik1pCkXqwlTYLLZS21B68Wf8Jz73l1nMPVelFomeNeCkoeKj0opSeUi+7BzFL0tbSJTFZdt0dJ5kyEqQWDyUPLDy8M/N7P/ZFjDG0tbX1Znt7O8kYgwAAsLm5idfX1x9zv7+/L66tremgL8aYAP4SWl1dnXIcR2w0Gg9XVlYKPNjpdEZ833/A/dHR0QvLst6WSqXLs+Xl5Zc8wRXA9/08xlgZHx+P5fP5ywokSRpVVbXD/fDwsFsoFN6FQqEpXqGiKLmFhYXuFYBnn5iY+EAIERljl488z0tSSpt9QML3fRkhFFQqlZQsy91rLXieF+i6bjmOozDGWjwIIRyFENb7Pd93HCdNKT2zbVsjhLSvAYIgwOVyudhqtVpzc3MeDzabTQFC2OM+Ho9/7na7v0RRRENDQ5O9Xs+sVquRnZ2dR4yxMCyVSoupVOoOQuirpmmeLMs/qtXqjCRJqqqq78fGxoBhGNMQQj7U6XQ6/dF13WYQBDlRFHtCNBp1EonE2enp6ZNarRa9uLiA4XA4EATBkCRpxjTN26FQ6AtjLAYAIMfHx4uZTMbAGI/U63VJUBSFX27IsnwLY0zOz89HEEL8T2Tb7fZd13VfZ7PZHITwJyHkniAINdd1I/xeMpmMCbFYbFJRlL35+fkyn1//48vS3d3dfaVp2h4A4Nvs7Oyng4ODiq7rtmmaEcuyRg3DeA5PTk7EXC7ngxt0eHjIywbFYrH579nS0lKGEPIM3vTwf7SxsfGUUpoQBgXw3QAAWGhQAKU0btv22cAAxlgEY/x7YADG+Luqqq0/AQAA//9oVhcKosq1vAAAAABJRU5ErkJggg==',400,113,'','','','',0);
INSERT INTO artist_images VALUES('0bc46491-fd18-4a76-91a4-5e2b9c1e4e2b','a17e6b40-759f-4d75-a791-867372deaca6','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AK+lohWVnUMeAM1DchGlLIAPpSQStCuE69TTZ2JG8KAGPOOgNR1K6H//2Q==',1000,1000,'','','','',0);
INSERT INTO artist_images VALUES('3ce40a39-bdd5-4aea-95ba-ac10cb3c6d15','a17e6b40-759f-4d75-a791-867372deaca6','fanart',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AKdjGrly+MdOainAExK9PaprEkBsdc1DKTk7utT1K6H/2Q==',1920,1080,'','','','',0);
INSERT INTO artist_images VALUES('a1f41afa-75b4-4333-a03c-d4355ac109a7','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AE0+KMWysVDM2SeM1VvUXzSVGKXTpv3qRM2F55qGWZTOx6qTwaizuXdWP//Z',500,649,'','jpg','','',0);
INSERT INTO artist_images VALUES('f68c6b9a-1ad3-4b58-ba6b-3158ef3414da','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','fanart',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AGxbPtEnmKCCec/SnzzCGH5OrcD2qBwoZmLkE/wiq5kMiEHruzQB/9k=',1920,1080,'','','','',0);
INSERT INTO artist_images VALUES('40dcd41f-25ed-4c2c-b815-042caae6c56c','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','logo',0,1,1,'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAACxUlEQVR4nIyTT0jbbBjA37xt0yZt8lnTP6nYou33ffB9bhOLGwxalLjD3C6boKB0x3nwIBOhVShKb1IRRsHhabsIgr14qmcPg4LCxFs7HIJba0tDm8YkTdJ0I2UTlXXbc3nC73n4kYfneSHoELFYrG91dfXeLYwwDOO+DjoKSJIMEQQxcJ2tr68PzMzMRP9EgNRqtbIsy7kbzRD6zs7O3k9OThp+KVhYWLDYbDYEwzDjdS5J0iWGYZ/C4bDxhmBzczMEALiCLpfLoigKfnJyIi0uLv6/vLxM7e7uGjiOQ87PzwmapptXgtnZWZPT6Xy4sbHxQP/WodVqtZIkeRkIBDzBYJBBUVRIp9MGj8czwDDMs6mpKW1ubo5uCwYHB6lCoWAGAHg9Ho+mQwzDTKqqWjiOc+TzeTSRSDTS6TQMBAL/dnd3671gaGjoqZ6N9Xrd5nK5nCzLHicSidb3WWGj0TCbzWYERVEsGo0SEEKjKIp9uVzuzcjIiO309PRr+w/sdjsqy/JfPp9P+TFXuVw2OhwOnKIo1ev11pLJJN/f32+hKOrz/v7+l+np6Yjf7y+3BTRNG+x2u7tarfYAAGz6CjVN+8ftdj9CEOQ/WZbvb21tvRNFsSubzR6Fw+EXAIBhHMc/tkfgeb7SbDYFh8PxMpPJvKpUKjGWZZ8LgtDj9XrvXFxcfKBp+i6EMEgQxGOe5/skSTo2mUya0+m0GSYmJkKqqo5yHFcVRTEvCEKD4ziiVCrtiaLIHh4evl1ZWXk9NjbWkmW5S1GUMoQwhCAI09vbKyFra2vxQqGwl0ql9KtrRSIRl6ZprZ2dnVI0Gh1OJpNHtw9tfn7+id/vHyVJUjayLAuLxWIeAKDqxe3t7aKex8fHzfV6vfGzS02lUhkAQGZpaelvEI/HYx3eg0FfV4faVUAcx7MdatrBwcHl7wTfAgAA//+xlhWLNjenPQAAAABJRU5ErkJggg==',400,83,'','','','',0);
INSERT INTO artist_images VALUES('19d744f2-c9cc-4d19-a3cb-6a555e52e089','179b3e46-daae-418e-a964-14b08c7a6b17','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/ALc7CMqCOGptwqFPlHtVN55p5QM4APCgVNdyny0XG1uuR3rGxof/2Q==',1000,1000,'','','','',0);
INSERT INTO artist_images VALUES('f280ea22-6fe7-4971-aaae-2209375355b3','179b3e46-daae-418e-a964-14b08c7a6b17','fanart',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/ANKVtqdOpApsJyGHZWIHNQ32/wAn5CRzzinWxbDKxzg8H1rIs//Z',1920,1080,'','','','',0);
INSERT INTO artist_images VALUES('ad1adcb7-6304-4c96-a89e-07833446edd0','179b3e46-daae-418e-a964-14b08c7a6b17','logo',0,0,0,'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAACLklEQVR4nJySwW7TTBDHd9eb+HPGdtZVPm9ibKsoJHEOPaSmEoIDfQZOnOhb8GJ9AagQp3LlgKpIldrSyobESkwt25tFjkgJlVUk/iePZuY33vkPQb9FBoPBGNVLcV1Xq0tsA0BKOagrCoKAjcdj90FAEAS653lZXRFj7L9ut5s8CGCMAQAs6ooopZAkye0mDsOwhRDCfwAwxgZC6EcdQAgBZVnCJlYUJZRSovsAnRCS1gGKojDLsuTVd7vdthBCLYzxmkC3qC1FUeoAOE1Tnuf5ej/D4dDhnH+/24Ft288qiwghOEmSVRAEw/sAy7KaGGPn1yDfMIxvdwDP8yac8yZj7DpNU82yrM52dxiGim3b8yzLPNd191RV7biuG1U5zrlNqt/e2dkpsyxbmqap+74/255OCHlMKb0tigIDwOFqtRpfXl6uHfA87w2VUmaqqso4jh8BQNcwjHeb5n6/f5jn+f/T6bQAAL/X60Xz+bx9cnLydjQa3VS3ofi+7wshvgLAQAjhnZ2dWa1W67mmaU8B4IWqqtdxHDPLsm6iKBpqmrZqNBr9sixHtm2/p4SQuWEY9Pz8/AlC6GWz2bwyTbNnmmbDcZwPu7u7ny4uLuhiseh2Op0rXdelpml0NpsZcRxnNIqiWyGEzjmf7u/vf1wul9XGxdHR0ZeDg4MEY1xW75FSft54X+n09LRxfHwMyHGc15PJ5JWUUqk7or+JMMb2GGOsmvovgJ8BAAD//0+muo9nQ1ttAAAAAElFTkSuQmCC',784,169,'','','','',0);
INSERT INTO artist_images VALUES('319b7218-4396-448d-bfd0-1fcf06cf9245','c849e893-f623-4ae5-b477-e37f254d216c','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AKFoqyXSh+lSXcO184bB4BPepPsrQMroxyoyxBxioZpTIwBJwKB20P/Z',1000,1000,'','','','',0);
INSERT INTO artist_images VALUES('1ee4098d-bae4-4145-b610-51c7a5486a49','c849e893-f623-4ae5-b477-e37f254d216c','fanart',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AKkP724VfWkuVKyEEg444pttj7QuWAB6k1JdCMBGB+ck5AHAFILaH//Z',1920,1080,'f3535371f3f373f3','jpeg','fanarttv','2026-04-24T04:30:24Z',0);
INSERT INTO artist_images VALUES('1d85a5d4-157d-4a52-a605-181856c76373','c849e893-f623-4ae5-b477-e37f254d216c','logo',0,0,1,'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAAC10lEQVR4nIyTzWsrVRjGz2Q+O5E5MpkoV4NBM4O9ev24RLhVQSrppggKdeGiUFz5H2TR7voHFNKVzUq6yGa2ddsuhLiwLcPYtIiTTtKZNtCkM/mcnJPJTEYGVFqaRZ/F4Xk57/nB+xzeBHiCDMPIaZr2euyr1Wru/t3/ANu2RU3TlHkAXde/aTQamdhTFJU7ODjgHwHOz8/ls7OzN++9I+JDVVWx2WxWIYTdKIoYAEAGIfTWI0C/35coigKapn0Q16ZpKicnJ88oimJlWfYxxlSn02FmsxmPMR48AtA0LdM0nTk9PX0tri3Lets0TXo8HpOSJD1vtVoJx3HY6XRKAQC8B4B4/mQyKUEICY7jErquJxFC2Xa77UEIEwghynXdCUKI7fV6YGNjAz0ABEGQ5jiujhC6EUWRdRznVRAEwvHxsScIAkmSZHZxcRFcX1+zCKEwiqLoQVCapn1Rr9cXcrncXa/XSwVBADDGLxmG+QVjnPR9fy2bzbYAAJ83m81LRVH+whgvOI5zlPh3/lQmk3l+e3v7A0EQ75EkCXmefyZJ0trh4WE7CII/PM/7fjAYfCxJEhWG4XcIoQKE8AUZAwqFwoccx33p+/6LyWQiDIdDM5VKLU2nUwQhNAVByPq+/046nfZ5nid93/+IYZj3Pc/7m7Rte8F13ZeWZeHJZNIfjUY3URTZEMJPPc9rEARBiqL442w2MzDGfhiGiX6/HwwGA7vb7f5O6Lr+qlar8evr678BAGZxsOVy+bOLiwtEEMRoeXl5yTCMoaIoX0VRFLIsmx6PxwbP829YlvVz4urqKm/b9iUAIAQARKqqvttut+92d3f/LJVKpuu6zWKx+GulUimFYVir1+tH29vb5U6no8uy/DXY398vrq6usv99i6qq325ubqbn7cTKygqMM4+9LMvs1tZWAVQqlZ/uN+3s7HySz+fpeYC52tvbKzy5eY7+CQAA//9FBWxqZvViZgAAAABJRU5ErkJggg==',783,127,'','','','',0);
INSERT INTO artist_images VALUES('1c741cb5-e384-4408-a688-06402d9f4c5b','c849e893-f623-4ae5-b477-e37f254d216c','banner',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AKbITGnTlc/rTCmFJ54p7OEWPYeq80gncAjjnrSA/9k=',1000,185,'','','','',0);
INSERT INTO artist_images VALUES('ce4ddf59-06d4-4074-9dbc-b9572fb285f5','f1a5358a-d455-4004-921e-f01dfbcb427b','thumb',0,1,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/ADTY4jGdy5OM+9VbyJUmYICAPWn2EpgG5h96mXTlmLH+Lmp6lOLSP//Z',1000,1000,'','','','',0);
INSERT INTO artist_images VALUES('3e717b24-aba1-4923-9140-79f229d0f9a0','f1a5358a-d455-4004-921e-f01dfbcb427b','logo',0,0,0,'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAACPElEQVR4nJxTT2vTYBh/3zRZ3tGYltAq3dxQHMw5KbaM6mmwQbaDZczrvoH7DpKTX2BnL14cjF0EHWxDwkgwKy2UYVsxWw8tFNS2tknTP2nyJpKyllEvdc8heXj+8/v9XhLc0rLZ7GuKomzfbZpFUSRbrRaDENokJuyBgiCMrq1Wq24gEACVSuXdqCKdTs8NfUEQiLGNoXw+v+W67ih+enq6VSwWA4OAoihLAIDYMJlMJje9/9nZ2UNvcyQSiXAcF4UQOtf1D/x+f7NcLreJw8PD567rbpIkCXO53IYsyzMEQcyJosiEQqHFaDQ6izF+aRgGeXBwwHkDgsHgC4Zh7q+trdlQkqRdAMBdhNA90zTt6enpLkJofmpqKkgQhKtp2hsI4QaE8CdFUY/a7fYHkiRf0TT9bXl5+SNpWdYTCOGVpmnfu93ul1qttlSv1/WFhYVkr9fLu67rcBxXpWn6sc/nm2dZdtcwDN1xHNu7hlQU5e36+voiAGCWpmmMMWa2t7clAMDFNYDPTNO8TCQS74+OjrZYll1CCLGWZf0Z0ON9MplMvNFozFmW9ZVhmMTq6urnGyR4QDs3WTk+PubD4fDveDx+MWBBVdUyz/OfUqlUq1Qq/RjTgDMuCpZlOxDCzoQa+tfOz895VVVnh+f9t9m2PaPrujHxgL29vfD+/v6M55+cnPh1Xb9aWVnRJh4gSZLW6XSeyrJ8p9/vBzHGI5wmeo2FQgHHYrFfGGO+2WzCnZ2dy2HubwAAAP//QkDxgvXRy+0AAAAASUVORK5CYII=',785,172,'','','','',0);
INSERT INTO artist_images VALUES('a8bf8f51-1959-4773-a613-2ea5716a1b24','f1a5358a-d455-4004-921e-f01dfbcb427b','banner',0,0,0,'data:image/jpeg;base64,/9j/2wCEACgcHiMeGSgjISMtKygwPGRBPDc3PHtYXUlkkYCZlo+AjIqgtObDoKrarYqMyP/L2u71////m8H////6/+b9//gBKy0tPDU8dkFBdviljKX4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+Pj4+P/AABEIABAAEAMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/AKbAvYQoBzknNVxAwG4gEDrg1K7EafFz/EarA4yc0kNn/9k=',1000,185,'','','','',0);
INSERT INTO artist_images VALUES('24cae6ca-0418-4ee5-98ee-62f3187eef86','9bebfd87-47be-4a0b-9f8b-6763b2b73c42','thumb',0,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('b6e73fe1-acb5-45ac-b5f7-f3d3707bc42a','519c7d95-48f6-4a8f-b714-89d7f6e812a8','thumb',0,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('2188dc03-341f-487b-bedb-b7b8242d8051','de0c50cb-2b00-471f-842b-d38181fc684c','thumb',0,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('a80f7482-4ca1-4392-91c5-1dad59a29ebf','e258a056-61b9-4e88-af6d-55fda6255f4f','thumb',0,1,0,'',0,0,'','','','',0);
INSERT INTO artist_images VALUES('e6b6131e-2711-46ac-a3ce-7453b2438ac6','a7efd0cc-572a-4ed3-93cc-d7cf21d752e6','thumb',0,1,0,'',0,0,'','','','',0);
CREATE TABLE artist_platform_ids (
    artist_id     TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    platform_artist_id TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (artist_id, connection_id)
);
INSERT INTO artist_platform_ids VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','df9c1c55-4824-4a9e-8d21-4c84adcebe68','6651','2026-05-28T16:33:12Z','2026-06-04T01:57:43Z');
INSERT INTO artist_platform_ids VALUES('40147a87-1eac-4894-8acd-08562bfb3272','df9c1c55-4824-4a9e-8d21-4c84adcebe68','6717','2026-05-28T16:33:13Z','2026-06-04T01:57:44Z');
INSERT INTO artist_platform_ids VALUES('c849e893-f623-4ae5-b477-e37f254d216c','df9c1c55-4824-4a9e-8d21-4c84adcebe68','6757','2026-05-28T16:33:13Z','2026-06-04T01:57:44Z');
INSERT INTO artist_platform_ids VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','df9c1c55-4824-4a9e-8d21-4c84adcebe68','6777','2026-05-28T16:33:13Z','2026-06-04T01:57:44Z');
INSERT INTO artist_platform_ids VALUES('a17e6b40-759f-4d75-a791-867372deaca6','df9c1c55-4824-4a9e-8d21-4c84adcebe68','6781','2026-05-28T16:33:13Z','2026-06-04T01:57:44Z');
INSERT INTO artist_platform_ids VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','df9c1c55-4824-4a9e-8d21-4c84adcebe68','6811','2026-05-28T16:33:13Z','2026-06-04T01:57:44Z');
INSERT INTO artist_platform_ids VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','df9c1c55-4824-4a9e-8d21-4c84adcebe68','6817','2026-05-28T16:33:13Z','2026-06-04T01:57:44Z');
CREATE TABLE artist_aliases (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    alias TEXT NOT NULL,
    source TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE band_members (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    member_name TEXT NOT NULL,
    member_mbid TEXT,
    instruments TEXT NOT NULL DEFAULT '[]',
    vocal_type TEXT NOT NULL DEFAULT '',
    date_joined TEXT NOT NULL DEFAULT '',
    date_left TEXT NOT NULL DEFAULT '',
    is_original_member INTEGER NOT NULL DEFAULT 0,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE nfo_snapshots (
    id TEXT PRIMARY KEY,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE metadata_changes (
    id         TEXT PRIMARY KEY,
    artist_id  TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    field      TEXT NOT NULL,
    old_value  TEXT NOT NULL DEFAULT '',
    new_value  TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'manual',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
INSERT INTO metadata_changes VALUES('747846e4-2764-4832-b164-1990b6d3436f','40147a87-1eac-4894-8acd-08562bfb3272','rule_fix','','populated discography for Antonio Vivaldi: added 500, kept 0, skipped 0 of 500 release groups','rule:discography_populated','2026-05-28T17:56:17Z');
INSERT INTO metadata_changes VALUES('5a9ff65b-10d5-44f3-abf3-470eec3f7c48','ff6b4132-cfb2-46cf-b476-643c9ea00069','rule_fix','','populated discography for Claude Debussy: added 500, kept 0, skipped 0 of 500 release groups','rule:discography_populated','2026-05-28T18:31:46Z');
INSERT INTO metadata_changes VALUES('d3cadd50-d221-48e6-bddf-c5b2965883ee','ff6b4132-cfb2-46cf-b476-643c9ea00069','rule_fix','','populated origin ''French'' from audiodb for Claude Debussy','rule:origin_missing','2026-05-28T18:31:51Z');
INSERT INTO metadata_changes VALUES('af1e9e97-f4a3-4f30-a114-ec8dac324f54','ff6b4132-cfb2-46cf-b476-643c9ea00069','origin','','French','manual','2026-05-28T18:31:55Z');
INSERT INTO metadata_changes VALUES('703a71d0-cf23-4fd1-96f8-03945e3fe69d','a17e6b40-759f-4d75-a791-867372deaca6','rule_fix','','populated discography for George Frideric Handel: added 500, kept 0, skipped 0 of 500 release groups','rule:discography_populated','2026-05-28T18:59:35Z');
INSERT INTO metadata_changes VALUES('341e18a8-d803-4b03-9c01-b633adbec7c2','a17e6b40-759f-4d75-a791-867372deaca6','rule_fix','','populated origin ''Halle, Brandenburg–Prussia, Holy Roman Empire'' from wikipedia for George Frideric Handel','rule:origin_missing','2026-05-28T18:59:40Z');
INSERT INTO metadata_changes VALUES('9acd51b0-3afe-417c-897f-d8be358c5b61','a17e6b40-759f-4d75-a791-867372deaca6','origin','','Halle, Brandenburg–Prussia, Holy Roman Empire','manual','2026-05-28T18:59:44Z');
INSERT INTO metadata_changes VALUES('e8ed94d4-22f7-4131-82d9-9eeb10c3d38b','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','rule_fix','','populated origin ''Eisenach'' from audiodb for Johann Sebastian Bach','rule:origin_missing','2026-05-28T19:16:16Z');
INSERT INTO metadata_changes VALUES('24c99608-294d-49fd-941c-aa1b120a7abd','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','origin','','Eisenach','manual','2026-05-28T19:16:23Z');
INSERT INTO metadata_changes VALUES('540d6e22-52e3-4625-9efc-c72107117468','179b3e46-daae-418e-a964-14b08c7a6b17','rule_fix','','populated discography for Johannes Brahms: added 500, kept 0, skipped 0 of 500 release groups','rule:discography_populated','2026-05-28T19:16:55Z');
INSERT INTO metadata_changes VALUES('8d8fe292-58db-49cc-8137-bfac01832a6a','179b3e46-daae-418e-a964-14b08c7a6b17','rule_fix','','populated origin ''Deutch, Hamburg'' from audiodb for Johannes Brahms','rule:origin_missing','2026-05-28T19:17:00Z');
INSERT INTO metadata_changes VALUES('ca47caa4-74b4-4302-b13b-a80bdd3492f8','179b3e46-daae-418e-a964-14b08c7a6b17','origin','','Deutch, Hamburg','manual','2026-05-28T19:17:04Z');
INSERT INTO metadata_changes VALUES('465cbfd8-8933-4d01-a235-5fc9c1f7fbde','c849e893-f623-4ae5-b477-e37f254d216c','rule_fix','','populated discography for Ludwig van Beethoven: added 500, kept 0, skipped 0 of 500 release groups','rule:discography_populated','2026-05-28T19:36:52Z');
INSERT INTO metadata_changes VALUES('46021ab9-ded7-4036-be32-fe412594a81c','c849e893-f623-4ae5-b477-e37f254d216c','rule_fix','','populated origin ''Bonn, Germany'' from audiodb for Ludwig van Beethoven','rule:origin_missing','2026-05-28T19:36:57Z');
INSERT INTO metadata_changes VALUES('50f89234-c6d2-496f-ac00-d3deb34ec708','c849e893-f623-4ae5-b477-e37f254d216c','origin','','Bonn, Germany','manual','2026-05-28T19:37:01Z');
INSERT INTO metadata_changes VALUES('f326f1a3-1c30-4eb2-8509-5b13ac7d85f1','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','rule_fix','','populated discography for Johann Sebastian Bach: added 500, kept 0, skipped 0 of 500 release groups','rule:discography_populated','2026-05-29T12:16:16Z');
INSERT INTO metadata_changes VALUES('aed688bc-aaed-4093-b69b-86e0d0e38002','f1a5358a-d455-4004-921e-f01dfbcb427b','rule_fix','','populated discography for Wolfgang Amadeus Mozart: added 500, kept 0, skipped 0 of 500 release groups','rule:discography_populated','2026-05-29T13:33:35Z');
INSERT INTO metadata_changes VALUES('3ce1e70f-9039-4855-8536-b657da48612d','f1a5358a-d455-4004-921e-f01dfbcb427b','rule_fix','','populated origin ''Salzburg, Austrian'' from audiodb for Wolfgang Amadeus Mozart','rule:origin_missing','2026-05-29T13:33:39Z');
INSERT INTO metadata_changes VALUES('b52ee3ed-a5bc-4613-81fa-fbaa1cca3e22','f1a5358a-d455-4004-921e-f01dfbcb427b','origin','','Salzburg, Austrian','manual','2026-05-29T13:33:44Z');
INSERT INTO metadata_changes VALUES('ec3c640d-0534-4c38-8e4b-db2252bfc5df','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','biography',unistr('Johann Sebastian Bach (31 March 1685 – 28 July 1750) was a German composer and musician of the late Baroque period. He composed a wide range of music across various instruments and forms, including orchestral works such as the Brandenburg Concertos; solo instrumental pieces like the cello suites and the sonatas and partitas for solo violin; keyboard compositions including the Goldberg Variations and The Well-Tempered Clavier; organ works such as the Schübler Chorales and the Toccata and Fugue in D minor; and choral music including the St Matthew Passion and the Mass in B minor. Since the 19th century, Bach has been recognised as an influential figure in Western classical music.\u000a\u000aBach was born into a family with several musicians. After becoming an orphan at the age of 10, he lived with his eldest brother Johann Christoph and continued his musical studies in Lüneburg. He returned to Thuringia in 1703, working in various positions for Protestant churches in Arnstadt and Mühlhausen, and later at courts in Weimar and Köthen. In 1723, he was appointed cantor at St Thomas Church in Leipzig, where he composed for local churches and the university''s student ensemble Collegium Musicum. He began publishing keyboard and organ music in 1726. During his career, Bach experienced some difficulties in his professional relationships. In 1736, Augustus III of Poland granted him the title of court composer. In his later years, Bach revised and expanded many earlier works. He died in 1750 following complications from eye surgery. Bach had 20 children with two wives, Maria Barbara and Anna Magdalena, with 10 surviving into adulthood. Four of his sons became composers.\u000a\u000aBach''s work incorporates German musical traditions and techniques such as counterpoint, harmonic and motivic organisation, and includes influences from Italian and French music. His output comprises hundreds of cantatas, both sacred and secular, as well as Latin church music, Passions, oratorios, and motets. He frequently used Lutheran hymns in his vocal music, including four-part chorales and sacred songs. His compositions cover organ and other keyboard instruments, concertos for violin and harpsichord, and chamber and orchestral suites. His music often employs contrapuntal methods such as canon and fugue.\u000a\u000aDuring the 18th century, Bach was mainly known as an organist, and his keyboard works, such as The Well-Tempered Clavier, were valued for their educational aspects. The 19th century brought renewed interest in his music, with biographies published and his complete works printed. Scholarship on Bach has continued through dedicated periodicals, catalogues such as the Bach-Werke-Verzeichnis, and critical editions. His music has been popularised through arrangements like "Air on the G String" and "Jesu, Joy of Man''s Desiring," as well as recordings, including complete collections released for the 250th anniversary of his death.'),unistr('Johann Sebastian Bach (31 March  1685 – 28 July 1750) was a German composer, organist, harpsichordist, violist, and violinist of the Baroque period. He enriched many established German styles through his skill in counterpoint, harmonic and motivic organisation, and the adaptation of rhythms, forms, and textures from abroad, particularly from Italy and France. Many of Bach''s works are still known today, such as the Brandenburg Concertos, the Mass in B minor, the The Well-Tempered Clavier, his cantatas, chorales, partitas, Passions, and organ works. His music is revered for its intellectual depth, technical command, and artistic beauty.\u000d\u000aBach was born in Eisenach, Saxe-Eisenach, into a very musical family; his father, Johann Ambrosius Bach was the director of the town musicians, and all of his uncles were professional musicians. His father taught him to play violin and harpsichord, and his brother, Johann Christoph Bach, taught him the clavichord and exposed him to much contemporary music. Bach also went to St Michael''s School in Lüneburg because of his singing skills. After graduating, he held several musical posts across Germany: he served as Kapellmeister (director of music) to Leopold, Prince of Anhalt-Köthen, Cantor of Thomasschule in Leipzig, and Royal Court Composer to August III. Bach''s health and vision declined in 1749, and he died on 28 July 1750. Modern historians believe that his death was caused by a combination of stroke and pneumonia.\u000d\u000aBach''s abilities as an organist were highly respected throughout Europe during his lifetime, although he was not widely recognised as a great composer until a revival of interest and performances of his music in the first half of the 19th century. He is now generally regarded as one of the main composers of the Baroque period, and as one of the greatest composers of all time.'),'manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('b12ed1f3-e217-4894-85e2-d39e43862d08','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','genres','Classical','baroque, cantata, Classical, mass, motet, Orchestral','manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('523e10a2-0e0a-4f1b-950c-1f6578a05ea4','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','styles','Chamber Music, Choral, Concerto, Keyboard, Orchestral','Classical','manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('155bc725-ad59-44ed-8d83-2643c03b78d0','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','born','','1685-03-21','manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('a0f0b023-6192-4003-b4fc-2c5c59d7de3e','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','died','','1750-07-28','manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('acc2cd94-9b18-49cd-b74b-d55b71afa469','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','years_active','','1685-1750','manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('9d75fd6b-1e63-47b1-afcb-966a48c76d81','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','type','','solo','manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('0e0e064e-d052-4a7e-9488-45296f3d8ebc','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','gender','','male','manual','2026-07-06T00:04:47Z');
INSERT INTO metadata_changes VALUES('ba4cc1cf-1c25-49fb-8517-f533b32affd8','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','rule_fix','','saved logo from audiodb ([logo.png])','rule:logo_exists','2026-07-06T00:04:49Z');
CREATE TABLE mb_snapshots (
    id          TEXT PRIMARY KEY,
    artist_id   TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    field       TEXT NOT NULL,
    mb_value    TEXT NOT NULL DEFAULT '',
    fetched_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(artist_id, field)
);
INSERT INTO mb_snapshots VALUES('28cbbd87-ab38-45dc-807f-04f16211e755','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','genres','["Classical","Orchestral","baroque","cantata","mass","motet"]','2026-07-06T00:10:04Z');
INSERT INTO mb_snapshots VALUES('2791a70a-5202-4d2a-b62f-f7164a377651','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','born','1685-03-21','2026-07-06T00:10:04Z');
INSERT INTO mb_snapshots VALUES('15b0886d-68a0-45ac-8e55-ed36a61c99dc','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','died','1750-07-28','2026-07-06T00:10:04Z');
CREATE TABLE rules (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    category TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    config TEXT NOT NULL DEFAULT '{}',
    automation_mode TEXT NOT NULL DEFAULT 'auto',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO rules VALUES('extraneous_images','Extraneous image files','Flags image files that do not match filenames configured in the active platform profile. Extra files can cause duplicate or incorrect artwork on media servers. Auto-fix deletes them; manual mode lets you review changes first.','image',1,'{"severity":"warning"}','manual','2026-04-24 03:47:21','2026-05-06T00:58:22Z');
INSERT INTO rules VALUES('nfo_exists','NFO file exists','Artist directory must contain an artist.nfo file','nfo',1,'{"severity":"error"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('nfo_has_mbid','NFO has MusicBrainz ID','The artist.nfo file must contain a MusicBrainz artist ID','nfo',1,'{"severity":"error"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('thumb_exists','Thumbnail image exists','Artist directory must contain a thumbnail image (folder.jpg/png)','image',1,'{"severity":"error"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('thumb_square','Thumbnail is square','Thumbnail must be approximately square (1:1 ratio). Violations are fixed by fetching a square replacement from providers; the existing image is not cropped.','image',1,'{"aspect_ratio":1,"tolerance":0.1,"severity":"warning"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('thumb_min_res','Thumbnail minimum resolution','Thumbnail must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.','image',1,'{"min_width":500,"min_height":500,"severity":"warning"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('fanart_exists','Fanart image exists','Artist directory must contain a fanart/backdrop image','image',1,'{"severity":"warning"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('logo_exists','Logo image exists','Artist directory must contain a logo image (logo.png)','image',1,'{"severity":"info"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('bio_exists','Biography exists','Artist must have a biography populated','metadata',1,'{}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('fanart_min_res','Fanart minimum resolution','Fanart/backdrop must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.','image',0,'{"min_width":1920,"min_height":1080,"severity":"warning"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('fanart_aspect','Fanart aspect ratio','Fanart/backdrop should match the target aspect ratio. Violations are fixed by fetching a correctly-proportioned replacement from providers; the existing image is not cropped.','image',0,'{"aspect_ratio":1.7777777777777777,"tolerance":0.1,"severity":"info"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('logo_min_res','Logo minimum width','Logo should meet the minimum width for legibility. Violations are fixed by fetching a higher-resolution logo from providers.','image',0,'{"min_width":400,"severity":"info"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('banner_exists','Banner image exists','Artist directory should contain a banner image','image',0,'{"severity":"info"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('banner_min_res','Banner minimum resolution','Banner must meet the minimum resolution. Violations are fixed by fetching a higher-resolution replacement from providers.','image',0,'{"min_width":1000,"min_height":185,"severity":"info"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('artist_id_mismatch','Artist/ID mismatch','Detects when an artist''s filesystem folder name differs from their stored metadata name. Uses fuzzy matching to allow minor variations while flagging significant divergences.','metadata',1,'{"tolerance":0.8,"severity":"warning"}','manual','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('directory_name_mismatch','Directory name matches artist','Artist directory name should match the canonical artist name','metadata',1,'{"severity":"warning","article_mode":"prefix"}','manual','2026-04-24T03:47:21Z','2026-05-06T00:58:39Z');
INSERT INTO rules VALUES('image_duplicate','No duplicate images','Different image slots should not contain visually similar images (default threshold: 90%)','image',1,'{"tolerance":0.9,"severity":"warning"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('metadata_quality','Metadata quality','Detects placeholder or junk metadata values (e.g. biography of just ''?'' or ''N/A''). Violations are fixed by clearing the junk value and re-fetching from providers.','metadata',1,'{"severity":"warning"}','manual','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('backdrop_sequencing','Backdrop/fanart sequencing','Detects gaps in backdrop/fanart image sequences and incorrect numbering. Violations are fixed by renaming files to fill gaps.','image',1,'{"severity":"warning"}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('backdrop_min_count','Minimum backdrop count','Flags artists with fewer backdrops than the configured minimum. This rule is detection-only; resolving violations requires manual upload or multiple evaluation passes.','image',0,'{"severity":"warning","min_count":1}','manual','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('logo_padding','Logo excessive padding','Detects logo images where excessive transparent (PNG) or whitespace (JPG) padding surrounds the content. If the padding area exceeds the configured threshold (default 15%) of the total image area, a violation is raised. Auto-fix trims to content bounds with a configurable margin. Replaces the former logo_trimmable rule.','image',1,'{"severity":"info","threshold_percent":15,"trim_margin":2}','auto','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
INSERT INTO rules VALUES('name_language_pref','Artist name matches preferred language','Flags artists whose stored Name or SortName does not match the user''s preferred metadata languages. When MusicBrainz provides a preferred-locale alias, the violation is fixable and Fix/auto mode can promote it; otherwise the violation is informational and can be edited manually or dismissed.','metadata',1,'{"severity":"info"}','auto','2026-04-24T03:47:21Z','2026-05-26T04:29:39Z');
INSERT INTO rules VALUES('origin_missing','Origin is populated','Flags artists with an empty origin field. Violations are fixed by fetching the origin (city, region, or country) from the configured provider priority list. Auto mode applies the highest-priority non-empty result; manual mode surfaces the violation so you can pick a provider value or edit it.','metadata',1,'{"severity":"info"}','auto','2026-05-20T02:29:32Z','2026-05-26T04:29:24Z');
INSERT INTO rules VALUES('discography_populated','Discography is populated','Flags artists whose artist.nfo has no album entries, or materially fewer than MusicBrainz lists. Violations are fixed by fetching release groups from MusicBrainz and merging them into the NFO; user-added albums are always preserved. Auto mode applies the merge automatically; manual mode surfaces the violation so you can review and fix it individually.','metadata',1,'{"severity":"info","coverage_threshold":50,"release_types":"Album,EP"}','auto','2026-05-22T03:41:23Z','2026-05-26T04:29:29Z');
CREATE TABLE rule_violations (
    id TEXT PRIMARY KEY,
    rule_id TEXT NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    artist_name TEXT NOT NULL,
    severity TEXT NOT NULL DEFAULT 'warning',
    message TEXT NOT NULL,
    fixable INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'open',
    dismissed_at TEXT,
    resolved_at TEXT,
    candidates TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(rule_id, artist_id)
);
INSERT INTO rule_violations VALUES('60091040-e796-4b90-8a45-73a026229a43','fanart_exists','40147a87-1eac-4894-8acd-08562bfb3272','Antonio Vivaldi','warning','artist Antonio Vivaldi has no fanart image',1,'pending_choice',NULL,NULL,'[{"url":"https://assets.fanart.tv/fanart/vivaldi-antonio-516a7eb05e409.jpg","width":0,"height":0,"source":"fanarttv","image_type":"fanart"},{"url":"https://assets.fanart.tv/fanart/vivaldi-antonio-516aacb2cac4d.jpg","width":0,"height":0,"source":"fanarttv","image_type":"fanart"}]','2026-05-28T16:29:12Z','2026-07-05T00:40:39Z');
INSERT INTO rule_violations VALUES('1976e06b-520b-4978-a884-07c6b226cf6c','discography_populated','40147a87-1eac-4894-8acd-08562bfb3272','Antonio Vivaldi','info','artist Antonio Vivaldi has no discography in artist.nfo',1,'resolved',NULL,'2026-05-28T17:56:31Z','[]','2026-05-28T16:29:12Z','2026-05-28T17:56:31Z');
INSERT INTO rule_violations VALUES('1b73ae93-70d4-4bd6-a25d-cf0c7ab0f421','logo_exists','a17e6b40-759f-4d75-a791-867372deaca6','George Frideric Handel','info','artist George Frideric Handel has no logo image',1,'open',NULL,NULL,'[]','2026-05-28T16:30:54Z','2026-07-05T00:45:31Z');
INSERT INTO rule_violations VALUES('8d5326cd-60f0-4ad0-a2d6-c5863922ee40','discography_populated','a17e6b40-759f-4d75-a791-867372deaca6','George Frideric Handel','info','artist George Frideric Handel discography covers 1 of 500 MusicBrainz release groups (0%, below the 50% threshold)',1,'resolved',NULL,'2026-05-28T18:59:44Z','[]','2026-05-28T16:30:54Z','2026-05-28T18:59:44Z');
INSERT INTO rule_violations VALUES('69a386b6-601c-4023-bb15-18386e5df9b0','origin_missing','a17e6b40-759f-4d75-a791-867372deaca6','George Frideric Handel','info','artist George Frideric Handel has no origin',1,'resolved',NULL,'2026-05-28T18:59:44Z','[]','2026-05-28T16:30:54Z','2026-05-28T18:59:44Z');
INSERT INTO rule_violations VALUES('5c4e93fc-7552-4584-b8fa-cc45c0b55fcf','discography_populated','c849e893-f623-4ae5-b477-e37f254d216c','Ludwig van Beethoven','info','artist Ludwig van Beethoven has no discography in artist.nfo',1,'resolved',NULL,'2026-05-28T19:37:01Z','[]','2026-05-28T16:31:01Z','2026-05-28T19:37:01Z');
INSERT INTO rule_violations VALUES('692164ca-d354-4eba-ab40-2a23d33484dd','origin_missing','c849e893-f623-4ae5-b477-e37f254d216c','Ludwig van Beethoven','info','artist Ludwig van Beethoven has no origin',1,'resolved',NULL,'2026-05-28T19:37:01Z','[]','2026-05-28T16:31:01Z','2026-05-28T19:37:01Z');
INSERT INTO rule_violations VALUES('87297272-2855-44f6-bf2c-a8f95ed029c2','discography_populated','ff6b4132-cfb2-46cf-b476-643c9ea00069','Claude Debussy','info','artist Claude Debussy discography covers 1 of 500 MusicBrainz release groups (0%, below the 50% threshold)',1,'resolved',NULL,'2026-05-28T18:31:55Z','[]','2026-05-28T16:31:17Z','2026-05-28T18:31:55Z');
INSERT INTO rule_violations VALUES('edecfa04-f20e-4af8-bc0c-1593157b8f0b','origin_missing','ff6b4132-cfb2-46cf-b476-643c9ea00069','Claude Debussy','info','artist Claude Debussy has no origin',1,'resolved',NULL,'2026-05-28T18:31:55Z','[]','2026-05-28T16:31:17Z','2026-05-28T18:31:55Z');
INSERT INTO rule_violations VALUES('ba2e65d4-ee76-4156-a21f-3b7c5fdb354b','origin_missing','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','Johann Sebastian Bach','info','artist Johann Sebastian Bach has no origin',1,'resolved',NULL,'2026-05-28T19:16:23Z','[]','2026-05-28T16:32:38Z','2026-05-28T19:16:23Z');
INSERT INTO rule_violations VALUES('0d208487-9a11-4142-b55f-c9c6dabbd6fa','discography_populated','179b3e46-daae-418e-a964-14b08c7a6b17','Johannes Brahms','info','artist Johannes Brahms discography covers 1 of 500 MusicBrainz release groups (0%, below the 50% threshold)',1,'resolved',NULL,'2026-05-28T19:17:04Z','[]','2026-05-28T16:34:21Z','2026-05-28T19:17:04Z');
INSERT INTO rule_violations VALUES('2b94b080-5980-41e8-883f-321be17b5e98','origin_missing','179b3e46-daae-418e-a964-14b08c7a6b17','Johannes Brahms','info','artist Johannes Brahms has no origin',1,'resolved',NULL,'2026-05-28T19:17:04Z','[]','2026-05-28T16:34:21Z','2026-05-28T19:17:04Z');
INSERT INTO rule_violations VALUES('d9d60a87-d17b-4350-8e4c-64b83d683fa9','fanart_exists','f1a5358a-d455-4004-921e-f01dfbcb427b','Wolfgang Amadeus Mozart','warning','artist Wolfgang Amadeus Mozart has no fanart image',1,'pending_choice',NULL,NULL,'[{"url":"https://assets.fanart.tv/fanart/mozart-wolfgang-amadeus-527a7a17c1157.jpg","width":0,"height":0,"source":"fanarttv","image_type":"fanart"},{"url":"https://assets.fanart.tv/fanart/mozart-wolfgang-amadeus-5075ebf921453.jpg","width":0,"height":0,"source":"fanarttv","image_type":"fanart"},{"url":"https://assets.fanart.tv/fanart/mozart-wolfgang-amadeus-527a7dc20dd00.jpg","width":0,"height":0,"source":"fanarttv","image_type":"fanart"},{"url":"https://assets.fanart.tv/fanart/mozart-wolfgang-amadeus-527a7dc20e9e0.jpg","width":0,"height":0,"source":"fanarttv","image_type":"fanart"}]','2026-05-28T16:34:32Z','2026-07-05T00:57:05Z');
INSERT INTO rule_violations VALUES('f4532861-0a12-4a94-a50d-5f5cbf240572','discography_populated','f1a5358a-d455-4004-921e-f01dfbcb427b','Wolfgang Amadeus Mozart','info','artist Wolfgang Amadeus Mozart has no discography in artist.nfo',1,'resolved',NULL,'2026-05-29T13:33:44Z','[]','2026-05-28T16:34:32Z','2026-05-29T13:33:44Z');
INSERT INTO rule_violations VALUES('81987764-c834-4138-81d3-8d7712fe3c01','origin_missing','f1a5358a-d455-4004-921e-f01dfbcb427b','Wolfgang Amadeus Mozart','info','artist Wolfgang Amadeus Mozart has no origin',1,'resolved',NULL,'2026-05-29T13:33:44Z','[]','2026-05-28T16:34:32Z','2026-05-29T13:33:44Z');
INSERT INTO rule_violations VALUES('6c8b97d2-9a8f-4b68-853e-723830d847be','discography_populated','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','Johann Sebastian Bach','info','artist Johann Sebastian Bach discography covers 1 of 500 MusicBrainz release groups (0%, below the 50% threshold)',1,'resolved',NULL,'2026-05-29T12:16:20Z','[]','2026-05-28T16:37:04Z','2026-05-29T12:16:20Z');
INSERT INTO rule_violations VALUES('10c55238-ad15-4d49-aa2c-34c28d8d9eb3','fanart_exists','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','Johann Sebastian Bach','warning','artist Johann Sebastian Bach has no fanart image',1,'open',NULL,NULL,'[]','2026-07-06T00:04:48Z','2026-07-06T00:10:06Z');
INSERT INTO rule_violations VALUES('012b822a-4e19-43f6-b94c-9233a51839dd','thumb_square','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','Johann Sebastian Bach','warning','artist Johann Sebastian Bach thumbnail aspect ratio 0.77 does not match expected 1.00',1,'open',NULL,NULL,'[]','2026-07-06T00:04:49Z','2026-07-06T00:10:06Z');
INSERT INTO rule_violations VALUES('bdb46b0f-c248-4489-a592-c3845ed9ad08','logo_exists','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','Johann Sebastian Bach','info','artist Johann Sebastian Bach has no logo image',1,'resolved',NULL,'2026-07-06T00:04:49Z','[]','2026-07-06T00:04:49Z','2026-07-06T00:04:49Z');
INSERT INTO rule_violations VALUES('e3cc9b30-be39-4ee1-a078-0e17e97a7516','logo_padding','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','Johann Sebastian Bach','info','artist Johann Sebastian Bach logo has 48.6% padding (threshold 15%)',1,'open',NULL,NULL,'[]','2026-07-06T00:04:49Z','2026-07-06T00:10:06Z');
CREATE TABLE rule_results (
    artist_id         TEXT    NOT NULL REFERENCES artists(id)           ON DELETE CASCADE,
    rule_id           TEXT    NOT NULL REFERENCES rules(id)             ON DELETE CASCADE,
    passed            INTEGER NOT NULL DEFAULT 0,
    violation_id      TEXT             REFERENCES rule_violations(id)   ON DELETE SET NULL,
    evaluated_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    violation_message TEXT,
    first_failed_at   TEXT,
    last_passed_at    TEXT,
    PRIMARY KEY (artist_id, rule_id)
);
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','backdrop_sequencing',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','extraneous_images',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','fanart_exists',0,'60091040-e796-4b90-8a45-73a026229a43','2026-07-05T00:40:39Z','artist Antonio Vivaldi has no fanart image','2026-05-28T16:29:12Z',NULL);
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','logo_padding',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','logo_exists',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','image_duplicate',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','thumb_exists',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','thumb_square',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','thumb_min_res',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','name_language_pref',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','artist_id_mismatch',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','bio_exists',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','directory_name_mismatch',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','discography_populated',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','metadata_quality',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','origin_missing',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','nfo_exists',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('40147a87-1eac-4894-8acd-08562bfb3272','nfo_has_mbid',1,NULL,'2026-07-05T00:40:36Z',NULL,NULL,'2026-07-05T00:40:36Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','backdrop_sequencing',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','extraneous_images',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','fanart_exists',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','logo_padding',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','logo_exists',0,'1b73ae93-70d4-4bd6-a25d-cf0c7ab0f421','2026-07-05T00:45:31Z','artist George Frideric Handel has no logo image','2026-05-28T16:30:54Z',NULL);
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','image_duplicate',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','thumb_exists',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','thumb_square',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','thumb_min_res',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','name_language_pref',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','artist_id_mismatch',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','bio_exists',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','directory_name_mismatch',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','discography_populated',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','metadata_quality',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','origin_missing',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','nfo_exists',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('a17e6b40-759f-4d75-a791-867372deaca6','nfo_has_mbid',1,NULL,'2026-07-05T00:45:23Z',NULL,NULL,'2026-07-05T00:45:23Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','backdrop_sequencing',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','extraneous_images',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','fanart_exists',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','logo_padding',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','logo_exists',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','image_duplicate',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','thumb_exists',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','thumb_square',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','thumb_min_res',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','name_language_pref',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','artist_id_mismatch',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','bio_exists',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','directory_name_mismatch',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','discography_populated',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','metadata_quality',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','origin_missing',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','nfo_exists',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('c849e893-f623-4ae5-b477-e37f254d216c','nfo_has_mbid',1,NULL,'2026-07-05T00:50:04Z',NULL,NULL,'2026-07-05T00:50:04Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','backdrop_sequencing',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','extraneous_images',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','fanart_exists',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','logo_padding',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','logo_exists',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','image_duplicate',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','thumb_exists',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','thumb_square',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','thumb_min_res',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','name_language_pref',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','artist_id_mismatch',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','bio_exists',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','directory_name_mismatch',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','discography_populated',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','metadata_quality',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','origin_missing',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','nfo_exists',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','nfo_has_mbid',1,NULL,'2026-07-05T00:42:51Z',NULL,NULL,'2026-07-05T00:42:51Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','backdrop_sequencing',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','extraneous_images',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','fanart_exists',0,'10c55238-ad15-4d49-aa2c-34c28d8d9eb3','2026-07-06T00:10:06Z','artist Johann Sebastian Bach has no fanart image','2026-07-06T00:04:48Z','2026-07-05T00:47:42Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','logo_padding',0,'e3cc9b30-be39-4ee1-a078-0e17e97a7516','2026-07-06T00:10:06Z','artist Johann Sebastian Bach logo has 48.6% padding (threshold 15%)','2026-07-06T00:04:49Z','2026-07-05T00:47:42Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','logo_exists',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','image_duplicate',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','thumb_exists',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','thumb_square',0,'012b822a-4e19-43f6-b94c-9233a51839dd','2026-07-06T00:10:06Z','artist Johann Sebastian Bach thumbnail aspect ratio 0.77 does not match expected 1.00','2026-07-06T00:04:49Z','2026-07-05T00:47:42Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','thumb_min_res',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','name_language_pref',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','artist_id_mismatch',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','bio_exists',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','directory_name_mismatch',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','discography_populated',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','metadata_quality',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','origin_missing',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','nfo_exists',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','nfo_has_mbid',1,NULL,'2026-07-06T00:10:06Z',NULL,NULL,'2026-07-06T00:10:06Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','backdrop_sequencing',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','extraneous_images',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','fanart_exists',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','logo_padding',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','logo_exists',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','image_duplicate',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','thumb_exists',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','thumb_square',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','thumb_min_res',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','name_language_pref',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','artist_id_mismatch',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','bio_exists',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','directory_name_mismatch',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','discography_populated',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','metadata_quality',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','origin_missing',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','nfo_exists',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','nfo_has_mbid',1,NULL,'2026-07-05T00:47:50Z',NULL,NULL,'2026-07-05T00:47:50Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','backdrop_sequencing',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','extraneous_images',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','fanart_exists',0,'d9d60a87-d17b-4350-8e4c-64b83d683fa9','2026-07-05T00:57:05Z','artist Wolfgang Amadeus Mozart has no fanart image','2026-05-28T16:34:32Z',NULL);
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','logo_padding',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','logo_exists',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','image_duplicate',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','thumb_exists',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','thumb_square',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','thumb_min_res',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','name_language_pref',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','artist_id_mismatch',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','bio_exists',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','directory_name_mismatch',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','discography_populated',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','metadata_quality',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','origin_missing',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','nfo_exists',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
INSERT INTO rule_results VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','nfo_has_mbid',1,NULL,'2026-07-05T00:56:58Z',NULL,NULL,'2026-07-05T00:56:58Z');
CREATE TABLE health_history (
    id TEXT PRIMARY KEY,
    total_artists INTEGER NOT NULL,
    compliant_artists INTEGER NOT NULL,
    score REAL NOT NULL,
    recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO health_history VALUES('9326998f-8f9c-4800-89eb-0b661c61bf62',607,2,83.24909390444855716,'2026-04-24T04:00:36Z');
INSERT INTO health_history VALUES('9a7d3b62-65b3-47c5-9bd5-fc58a779f48e',605,27,89.79999999999999716,'2026-04-24T04:03:14Z');
INSERT INTO health_history VALUES('2f49ab62-b09e-43f4-b9aa-d818c33075e0',605,53,90.20000000000000284,'2026-04-24T04:05:06Z');
INSERT INTO health_history VALUES('bb5c73f6-4127-4dbe-b9da-d6d55bbffe0a',605,78,90.90000000000000569,'2026-04-24T04:13:35Z');
INSERT INTO health_history VALUES('c49a194b-416a-4f69-b81a-81b5c70dd6b4',605,64,90.70000000000000284,'2026-04-24T04:21:05Z');
INSERT INTO health_history VALUES('8250aef5-beda-4627-bf01-5ab01a5fe274',605,54,90.59999999999999431,'2026-04-24T04:30:08Z');
INSERT INTO health_history VALUES('a6df6746-c69e-4000-ba07-0444b2bdee98',606,4,89.30957095709631233,'2026-04-25T04:40:19Z');
INSERT INTO health_history VALUES('ddc04ab9-9ae1-4b0c-8c76-26d333cb626f',605,58,90.59999999999999431,'2026-04-25T07:22:24Z');
INSERT INTO health_history VALUES('2fb133ac-67e5-41a6-8fa7-301c847e7e1f',1501,308,88.99173884077346486,'2026-04-26T02:38:26Z');
INSERT INTO health_history VALUES('e269d4a8-c3ac-41cc-92d3-d7083cf3aa1b',1501,270,88.73817455030045665,'2026-04-26T02:40:44Z');
INSERT INTO health_history VALUES('d027af01-6e79-4308-8b44-4fad56836555',602,31,90.40000000000000569,'2026-04-26T05:58:47Z');
INSERT INTO health_history VALUES('d6d150fa-ebcc-42df-854c-6cb672584fdb',602,32,90.40000000000000569,'2026-04-26T19:00:12Z');
INSERT INTO health_history VALUES('817ee37b-6c04-4601-89dd-72d7ec752ada',602,32,90.40000000000000569,'2026-04-27T01:00:12Z');
INSERT INTO health_history VALUES('a632b663-b763-4c40-9591-7cd853137104',602,32,90.40000000000000569,'2026-04-27T02:57:24Z');
INSERT INTO health_history VALUES('2a55d038-b737-454f-9a99-47d68a7744c8',601,34,90.40000000000000569,'2026-04-27T05:18:41Z');
INSERT INTO health_history VALUES('c1eb1c7b-3042-49c2-8e36-cbc6aa73e783',601,243,93.2999999999999972,'2026-04-27T21:38:27Z');
INSERT INTO health_history VALUES('31f6205d-4cf6-40f1-b956-1fe0ed17ce35',601,243,93.2999999999999972,'2026-04-27T22:37:58Z');
INSERT INTO health_history VALUES('0bb09d4a-f1d6-4488-8dcf-392618dc4d53',601,243,93.2999999999999972,'2026-04-28T04:47:09Z');
INSERT INTO health_history VALUES('35dbffbb-650f-4ffb-9209-39996c4c5158',601,243,93.2999999999999972,'2026-04-29T02:11:26Z');
INSERT INTO health_history VALUES('8e0a3adc-7beb-4dbe-a4f7-56d706a11339',601,243,93.2999999999999972,'2026-04-29T16:20:22Z');
INSERT INTO health_history VALUES('0e481975-0fdd-44e5-91f9-d28c8cc74c15',601,243,93.2999999999999972,'2026-04-30T00:34:01Z');
INSERT INTO health_history VALUES('5a927900-b883-4380-8c12-9e5350cac446',601,243,93.2999999999999972,'2026-04-30T19:33:53Z');
INSERT INTO health_history VALUES('9005e2c2-2b04-42b2-9499-0765828904bd',601,243,93.2999999999999972,'2026-05-01T02:18:19Z');
INSERT INTO health_history VALUES('ec3de098-6b48-4bba-ab4c-516d9250c9a3',601,243,93.2999999999999972,'2026-05-01T19:59:19Z');
INSERT INTO health_history VALUES('cc8e053d-d4d5-4d28-a168-2024fdde34c0',601,243,93.2999999999999972,'2026-05-02T02:00:06Z');
INSERT INTO health_history VALUES('d4525a80-23e2-4f19-a671-cfb23c137f30',601,243,93.2999999999999972,'2026-05-03T15:09:54Z');
INSERT INTO health_history VALUES('fd839d94-99b6-45be-8e3f-10374824cf99',601,243,93.2999999999999972,'2026-05-04T04:12:58Z');
INSERT INTO health_history VALUES('0ad620b9-39e7-410f-9e47-26f1b1241069',601,243,93.2999999999999972,'2026-05-04T16:34:52Z');
INSERT INTO health_history VALUES('00cdac53-9bd5-4a11-9389-ff50aad4cde2',606,242,92.3701320132016975,'2026-05-06T01:00:22Z');
INSERT INTO health_history VALUES('75f38bbf-d2e3-4292-9f7f-11842d362401',600,243,93.2999999999999972,'2026-05-09T07:47:59Z');
INSERT INTO health_history VALUES('d9cb90f6-f141-45d3-85b6-8a4b6c47c83d',600,243,93.2999999999999972,'2026-05-09T23:18:28Z');
INSERT INTO health_history VALUES('2cb584e6-660f-4212-8234-8edd39565e55',600,243,93.2999999999999972,'2026-05-10T05:18:29Z');
INSERT INTO health_history VALUES('c21d7fde-f95d-440c-8d9b-9bf9804ae62e',600,243,93.2999999999999972,'2026-05-10T11:18:28Z');
INSERT INTO health_history VALUES('991ce04b-1c80-46d0-9a74-921d131f6378',600,243,93.2999999999999972,'2026-05-10T17:18:28Z');
INSERT INTO health_history VALUES('21dac9d8-ca0f-475b-8d62-30b8b9f5111a',600,243,93.2999999999999972,'2026-05-10T23:18:32Z');
INSERT INTO health_history VALUES('6efaf214-dc26-4be9-af09-97d76fd4dc3a',600,243,93.2999999999999972,'2026-05-11T05:18:33Z');
INSERT INTO health_history VALUES('8b2d6eab-2237-4605-95d5-f6cde1c873b1',600,243,93.2999999999999972,'2026-05-11T11:18:29Z');
INSERT INTO health_history VALUES('4dbff3d3-bb0a-4e7f-837a-b12775172fe1',600,243,93.2999999999999972,'2026-05-11T17:18:33Z');
INSERT INTO health_history VALUES('479bcf78-2e97-493f-ade5-aec067b65fcb',600,243,93.2999999999999972,'2026-05-11T23:18:28Z');
INSERT INTO health_history VALUES('a567c963-df13-4559-87c7-aa00646dc778',1114,441,89.97369838420185317,'2026-05-21T22:20:31Z');
INSERT INTO health_history VALUES('6eef9615-74a4-49e8-8857-ca314f653457',1114,461,93.2730700179541117,'2026-05-22T02:32:31Z');
INSERT INTO health_history VALUES('f3c1c9cd-c234-4ad4-afea-e5005c654a41',605,243,93.2565289256202448,'2026-05-22T03:07:02Z');
INSERT INTO health_history VALUES('9397cb27-7a5c-4c2e-8176-a0fb6425967f',615,244,93.2000000000000028,'2026-05-22T13:38:14Z');
INSERT INTO health_history VALUES('a5ba1633-9668-44c0-9e78-2378140f47cf',615,244,93.2000000000000028,'2026-05-22T19:37:52Z');
INSERT INTO health_history VALUES('9e66f8ae-cc11-4941-9625-809c55ac5c62',615,244,93.2000000000000028,'2026-05-23T11:48:40Z');
INSERT INTO health_history VALUES('71e4882a-1180-420a-bddb-d44e60c8e7b2',615,244,93.2000000000000028,'2026-05-23T18:50:41Z');
INSERT INTO health_history VALUES('36c1c1f9-c3c8-4973-abcc-ed55d1e103c4',605,243,93.2193388429756027,'2026-05-24T00:11:07Z');
INSERT INTO health_history VALUES('c2f9e026-0e3c-4f18-b59b-fc117f43743c',616,1,86.27954545454494451,'2026-05-24T00:16:59Z');
INSERT INTO health_history VALUES('0131ec30-4d5e-47e0-95bd-51eca8dd550f',616,3,86.26996753246702099,'2026-05-24T00:19:01Z');
INSERT INTO health_history VALUES('d6c05674-3a0b-413c-9fa7-dc021a695798',615,3,87.90000000000000569,'2026-05-24T13:09:19Z');
INSERT INTO health_history VALUES('1dd20ec0-6380-43cb-9c2a-359c436af68f',615,3,87.90000000000000569,'2026-05-24T19:09:22Z');
INSERT INTO health_history VALUES('b05ab16f-eabf-4ec9-af6b-4f599ac4af66',605,2,87.70231404958626343,'2026-05-25T01:41:04Z');
INSERT INTO health_history VALUES('8371f792-4f57-47ae-9324-b66e222b860b',604,25,88.5,'2026-05-25T01:43:32Z');
INSERT INTO health_history VALUES('065f339a-3914-4a68-8b2f-cafaf6a51d2d',604,2,87.79999999999999716,'2026-05-25T10:52:26Z');
INSERT INTO health_history VALUES('0db4e55b-67a8-40eb-992e-28e3dcd289bb',604,1,87.79999999999999716,'2026-05-25T16:47:00Z');
INSERT INTO health_history VALUES('20dd4f29-d6c9-478b-b60b-6514981a1de1',605,1,87.83851239669370158,'2026-05-26T04:17:02Z');
INSERT INTO health_history VALUES('590c2c06-204a-4dc9-8d74-47356a2e5b25',605,1,87.82876033057799247,'2026-05-26T04:20:55Z');
INSERT INTO health_history VALUES('e7b48d94-4bd1-4b7d-9c4c-732c112fde36',603,1,87.70033167495802217,'2026-05-26T04:33:26Z');
INSERT INTO health_history VALUES('9112ce1a-393b-4d84-a3b9-847e7583eaf7',602,3,83.20000000000000284,'2026-05-26T08:41:55Z');
INSERT INTO health_history VALUES('e23bf372-ffe4-455b-90aa-ef98363d2192',602,3,83.20000000000000284,'2026-05-26T14:37:20Z');
INSERT INTO health_history VALUES('1b7e4b69-e935-40e7-9238-31845d33a66a',603,3,83.23499170812655734,'2026-05-26T20:00:44Z');
INSERT INTO health_history VALUES('d64f0362-08b7-4acd-b268-8690db52e264',603,4,83.23532338308508826,'2026-05-28T06:15:36Z');
INSERT INTO health_history VALUES('1115a8d3-076c-4c42-b342-ef117a44043d',602,4,83.20000000000000284,'2026-05-28T10:09:19Z');
INSERT INTO health_history VALUES('130b03aa-5e70-47f1-a4ad-0daa1ee8165e',614,4,81.74397394136856576,'2026-05-28T15:07:29Z');
INSERT INTO health_history VALUES('1b22aa23-4e76-43f0-a01e-9c97ffc81994',614,20,83.98876221498424855,'2026-05-28T16:29:04Z');
INSERT INTO health_history VALUES('f88be5e8-cc2c-41af-884d-fb8c71fbd271',613,13,83.90000000000000569,'2026-05-28T16:33:12Z');
INSERT INTO health_history VALUES('2218ee52-7c83-4a6b-a22a-b7264128eea6',614,10,83.87117263843703085,'2026-05-28T16:36:35Z');
INSERT INTO health_history VALUES('771c02ea-516c-4deb-9965-38ce42f425bd',613,11,83.90000000000000569,'2026-05-28T17:15:33Z');
INSERT INTO health_history VALUES('21f09111-99a3-4b2e-acca-752ac5edc94b',613,234,93.2000000000000028,'2026-05-29T13:37:42Z');
INSERT INTO health_history VALUES('8931997a-e64b-4590-ba9a-0a195f0720c4',613,238,93.2999999999999972,'2026-05-29T18:59:26Z');
INSERT INTO health_history VALUES('9fa19e1f-f9cb-43f6-b7b6-7c9d8028259c',612,238,93.2999999999999972,'2026-05-30T04:23:25Z');
INSERT INTO health_history VALUES('6b93bfd6-3741-4f16-a592-336362d40b56',614,238,93.1775244299680025,'2026-05-30T05:28:19Z');
INSERT INTO health_history VALUES('10cd2c93-6c8a-4f0c-b979-7a877df4571c',614,238,93.1506514657986174,'2026-05-30T23:27:28Z');
INSERT INTO health_history VALUES('34bc6da1-e4ed-4bf8-8553-3558d4b91227',614,239,93.2864820846911301,'2026-05-31T00:31:06Z');
INSERT INTO health_history VALUES('6d12a878-dbd4-4c35-9284-ac0c96e80f53',614,238,93.2682410423458492,'2026-05-31T01:03:06Z');
INSERT INTO health_history VALUES('82db1386-c18b-431f-9ef4-3cb33beea4c7',614,238,93.2682410423458492,'2026-05-31T01:16:56Z');
INSERT INTO health_history VALUES('27828523-1078-470e-8841-1532a4657098',614,238,93.2682410423458492,'2026-05-31T01:46:44Z');
INSERT INTO health_history VALUES('b9c88ec6-3589-4592-b1ad-38f7932e145c',613,247,93.7000000000000028,'2026-06-01T14:29:47Z');
INSERT INTO health_history VALUES('6b6bee58-acdc-4379-bfcf-1c77d5c05bd1',613,248,93.7000000000000028,'2026-06-02T22:10:06Z');
INSERT INTO health_history VALUES('c64e1d97-e9b3-4e1a-a455-22d26fa885ae',614,248,93.6881107491862224,'2026-06-03T20:28:01Z');
INSERT INTO health_history VALUES('c739cffa-df9c-416f-ba11-31e237b97730',614,249,93.6882736156357368,'2026-06-03T21:04:20Z');
INSERT INTO health_history VALUES('69c623a3-2d74-4500-9faf-2c28d16eca6f',613,248,93.7000000000000028,'2026-06-03T21:33:24Z');
INSERT INTO health_history VALUES('3d89355b-d5b8-4e4b-8900-9ed66881b613',614,249,93.6882736156357368,'2026-06-04T02:41:30Z');
INSERT INTO health_history VALUES('8cc420af-c609-4082-9548-391f6cb086cf',614,245,93.6066775244305234,'2026-06-04T06:02:37Z');
INSERT INTO health_history VALUES('f8ac3f7f-b576-49ce-af6c-f0466807670a',614,245,93.6066775244305234,'2026-06-04T06:13:07Z');
INSERT INTO health_history VALUES('8fef61cb-9cb7-4f17-9a2c-16eb020633ea',614,245,93.6066775244305234,'2026-06-04T06:24:13Z');
INSERT INTO health_history VALUES('05bec670-37d2-4205-ad87-d817fc73b8cf',614,245,93.5977198697074045,'2026-06-04T06:40:22Z');
INSERT INTO health_history VALUES('5fb31cce-ff34-4c3b-a4c0-d253d526ab37',614,245,93.5977198697074045,'2026-06-04T06:44:26Z');
INSERT INTO health_history VALUES('10e93ed5-456f-436d-bc63-7f6c0309cd10',614,245,93.5977198697074045,'2026-06-04T06:59:16Z');
INSERT INTO health_history VALUES('59530ecc-b7f4-4e46-9430-b4b7116db7b5',614,246,93.6157980456031567,'2026-06-04T07:20:46Z');
INSERT INTO health_history VALUES('d5b5a393-5a1b-4bc6-95ae-c744c084eb40',614,246,93.6157980456031567,'2026-06-04T07:24:08Z');
INSERT INTO health_history VALUES('7222a504-a6d2-4d0b-b8dd-7be68c0d7e54',614,245,93.6066775244305234,'2026-06-04T07:27:52Z');
INSERT INTO health_history VALUES('567aee32-e73c-467b-a2dc-8213dd02dc09',613,252,93.7999999999999972,'2026-06-04T14:48:34Z');
INSERT INTO health_history VALUES('0f5f2f9c-05fb-4905-8958-cd405ea630f3',614,252,93.771824104235094,'2026-06-05T00:19:25Z');
INSERT INTO health_history VALUES('bb36c3be-ec12-412c-a62d-b122e28f7992',614,248,93.6724755700331286,'2026-06-05T03:57:32Z');
INSERT INTO health_history VALUES('16a44176-996a-42ac-89bf-bcb899e5be3f',614,248,93.6724755700331286,'2026-06-05T04:03:34Z');
INSERT INTO health_history VALUES('7a3d6d2b-f093-4ce2-b421-273d7c6ee58e',614,248,93.6724755700331286,'2026-06-05T04:59:22Z');
INSERT INTO health_history VALUES('f531d106-4314-41f5-ae90-8d894f534d21',614,248,93.6724755700331286,'2026-06-05T05:00:34Z');
INSERT INTO health_history VALUES('c818dc19-8227-4870-8006-072b4b0ea10e',614,248,93.6814332247562618,'2026-06-05T07:19:34Z');
INSERT INTO health_history VALUES('a408ac5c-5dea-4452-8678-a44d6328e3b1',614,248,93.6724755700331286,'2026-06-05T09:56:51Z');
INSERT INTO health_history VALUES('48bf436c-5b05-4757-91ef-cd5ef2a4fffe',613,252,93.7999999999999972,'2026-06-05T21:43:57Z');
INSERT INTO health_history VALUES('a6092eab-1da9-46f7-8a3f-dabaaa0627a3',613,252,93.7999999999999972,'2026-06-06T03:44:11Z');
INSERT INTO health_history VALUES('4fc75433-e338-48a8-95b8-dc4b656400ed',613,252,93.7999999999999972,'2026-06-06T22:02:46Z');
INSERT INTO health_history VALUES('bbf0c145-7c93-4343-b869-3a1364cdfee8',613,252,93.7999999999999972,'2026-06-07T04:07:16Z');
INSERT INTO health_history VALUES('954897bd-be17-4fe4-a4b5-bd2dc3592678',614,252,93.7809446254077273,'2026-06-07T06:39:09Z');
INSERT INTO health_history VALUES('9bd176f8-5de2-4a0f-bbee-08f7dc359988',614,246,93.6452768729647146,'2026-06-07T07:18:02Z');
INSERT INTO health_history VALUES('71f1c2f4-81c8-4136-8f0c-68e2b88be79c',614,246,93.6452768729647146,'2026-06-07T07:26:16Z');
INSERT INTO health_history VALUES('531407b1-bd7e-4ec6-9f1e-6e9a5424073d',614,246,93.6452768729647146,'2026-06-07T07:49:51Z');
INSERT INTO health_history VALUES('4c2dfac0-eb96-4ee6-afd1-a3d3cf07b48b',614,246,93.6452768729647146,'2026-06-07T08:15:13Z');
INSERT INTO health_history VALUES('e06dad8d-e803-42bf-aaab-22d33ad0e946',614,246,93.6452768729647146,'2026-06-07T09:10:02Z');
INSERT INTO health_history VALUES('39b91eb0-2bcb-4511-a479-59d79fc5189d',614,246,93.6452768729647146,'2026-06-07T10:00:59Z');
INSERT INTO health_history VALUES('d8fcbf82-7df6-4730-84dc-6fdd21335919',613,252,93.7999999999999972,'2026-06-07T11:32:05Z');
INSERT INTO health_history VALUES('7eeaa621-8140-434d-9646-347f97fc0dd7',613,251,93.7999999999999972,'2026-06-07T17:32:44Z');
INSERT INTO health_history VALUES('c4c69be2-b5a5-4e56-a681-e8903104f0eb',613,252,93.7999999999999972,'2026-06-07T23:31:39Z');
INSERT INTO health_history VALUES('e1fdf4d2-8d75-4719-b3eb-195ee7f2685b',613,252,93.7999999999999972,'2026-06-08T05:31:26Z');
INSERT INTO health_history VALUES('41b1d21d-3cb1-411b-a2f1-81de0cdb9713',613,252,93.7999999999999972,'2026-06-08T11:40:27Z');
INSERT INTO health_history VALUES('a772305c-c052-4453-a097-e58d52a96108',613,252,93.7999999999999972,'2026-06-08T17:31:52Z');
INSERT INTO health_history VALUES('16494626-e305-446f-8f08-a5961c5cfbf6',613,252,93.7999999999999972,'2026-06-08T23:31:37Z');
INSERT INTO health_history VALUES('11358a43-9122-4015-9680-f64802ab8a75',613,252,93.7999999999999972,'2026-06-09T05:32:10Z');
INSERT INTO health_history VALUES('0c0ded54-39d3-46ee-b6ab-2b8f443c138d',613,252,93.7999999999999972,'2026-06-09T11:31:53Z');
INSERT INTO health_history VALUES('d24bef2b-4bff-46a3-acca-594123ff3200',613,252,93.7999999999999972,'2026-06-09T17:31:27Z');
INSERT INTO health_history VALUES('86850828-943f-4b82-ab93-4c695ee0b056',613,252,93.7999999999999972,'2026-06-09T23:31:28Z');
INSERT INTO health_history VALUES('62db7555-ca9b-4a72-8f10-137058cb6d49',614,252,93.7809446254077273,'2026-06-10T00:10:45Z');
INSERT INTO health_history VALUES('6eff9935-cf8b-48f0-9788-5ab7d2b299fb',614,251,93.771824104235094,'2026-06-10T00:14:19Z');
INSERT INTO health_history VALUES('a732bd3a-6c8f-4156-8ca2-776e63c5a3a2',614,246,93.6542345276878478,'2026-06-10T02:49:14Z');
INSERT INTO health_history VALUES('9ec1ed8e-70e1-4e27-b9e3-aba4f952c2fa',614,247,93.6543973941373621,'2026-06-10T04:25:34Z');
INSERT INTO health_history VALUES('2fbdec6c-0aa9-4001-985b-8896717285d4',614,250,93.7358306188930755,'2026-06-10T05:17:03Z');
INSERT INTO health_history VALUES('1d3b7b10-5bc1-47b9-b7f4-39bcd4e32339',613,249,93.7000000000000028,'2026-06-10T05:39:59Z');
INSERT INTO health_history VALUES('4eaad4d8-6acb-4492-9739-beae2a6f92b0',614,246,93.6452768729647146,'2026-06-10T22:58:20Z');
INSERT INTO health_history VALUES('520352e1-2481-46e1-beee-3f3e3c522126',614,249,93.6726384364826287,'2026-06-11T00:06:59Z');
INSERT INTO health_history VALUES('1dac37c4-9bdd-470f-aecb-82ad766ad882',614,247,93.6545602605868766,'2026-06-11T00:36:57Z');
INSERT INTO health_history VALUES('c359242f-9c85-4b1f-88f7-0920660da1c4',614,245,93.6092833876227103,'2026-06-11T02:08:05Z');
INSERT INTO health_history VALUES('9d5b65f6-df5f-4813-8a84-9e1e9cc2a6e7',614,245,93.6092833876227103,'2026-06-11T02:16:59Z');
INSERT INTO health_history VALUES('2bbae466-afdd-4a7d-a935-9b5bac127385',614,245,93.6092833876227103,'2026-06-11T02:38:21Z');
INSERT INTO health_history VALUES('dd70da78-92c6-4aad-a573-d33190fc15de',614,245,93.6182410423458294,'2026-06-11T02:41:16Z');
INSERT INTO health_history VALUES('a523dc32-ddda-48de-b48c-dd0f1533b734',614,245,93.6092833876227103,'2026-06-11T03:36:04Z');
INSERT INTO health_history VALUES('f6ec3849-a73f-4b0b-b9bb-69d92f4b8e98',613,251,93.7000000000000028,'2026-06-12T06:26:31Z');
INSERT INTO health_history VALUES('eaffe356-9769-4006-a28a-b999473838fc',613,251,93.7999999999999972,'2026-06-12T23:55:23Z');
INSERT INTO health_history VALUES('20e46a49-30f6-4239-9fe8-0733f66e3f5e',613,251,93.7999999999999972,'2026-06-18T04:37:03Z');
INSERT INTO health_history VALUES('1dac8b3e-2548-4619-8cea-27277740c054',614,251,93.7807817589582128,'2026-06-18T05:51:04Z');
INSERT INTO health_history VALUES('d066b25b-4853-4bef-bcc9-6ff7b75154fe',614,251,93.8169381107497458,'2026-06-18T21:02:57Z');
INSERT INTO health_history VALUES('924f4722-c53f-43a2-993e-be73fb4c50da',614,251,93.8169381107497458,'2026-06-18T22:20:15Z');
INSERT INTO health_history VALUES('2397287c-1c5e-4245-8ec6-3920c5079a84',614,251,93.8169381107497458,'2026-06-18T23:34:55Z');
INSERT INTO health_history VALUES('cfe9205b-ed6c-4f2f-aaaa-d125617887f6',614,251,93.8169381107497458,'2026-06-19T00:52:23Z');
INSERT INTO health_history VALUES('644297d2-b7e6-4ebc-8792-3a44103fdeb3',613,251,93.7999999999999972,'2026-06-19T01:58:24Z');
INSERT INTO health_history VALUES('e6d17552-6ef9-41b5-80db-2a5e600dd376',614,251,93.8169381107497458,'2026-06-19T02:21:12Z');
INSERT INTO health_history VALUES('e9f18fb0-c5a8-4311-a45a-24a35c71082d',614,251,93.8169381107497458,'2026-06-19T03:22:43Z');
INSERT INTO health_history VALUES('7db97bfb-263a-4e91-8ad9-8e40f8e52952',614,251,93.8169381107497458,'2026-06-19T03:27:01Z');
INSERT INTO health_history VALUES('5000d85e-179e-46a0-8181-c12ea4a810f3',614,251,93.8169381107497458,'2026-06-19T04:04:57Z');
INSERT INTO health_history VALUES('75ebe65c-6c9a-452d-b399-c127c1b2a6b4',613,251,93.7999999999999972,'2026-06-19T09:59:25Z');
INSERT INTO health_history VALUES('f7ead84b-f1d0-4c8c-9617-98e45d3533c1',613,251,93.7999999999999972,'2026-06-19T15:56:58Z');
INSERT INTO health_history VALUES('e1dc5b8d-0cb2-4db6-a74a-e83efc77e975',613,251,93.7999999999999972,'2026-06-19T21:54:24Z');
INSERT INTO health_history VALUES('f74f2689-fa15-41e7-bec0-14fe5f53c035',614,251,93.8169381107497458,'2026-06-20T01:02:24Z');
INSERT INTO health_history VALUES('352cb47c-4ef9-44f0-b655-ba09da3b8b23',613,251,93.7999999999999972,'2026-06-20T03:54:21Z');
INSERT INTO health_history VALUES('029237f6-745e-484d-832f-a101ec284942',613,251,93.7999999999999972,'2026-06-20T09:54:20Z');
INSERT INTO health_history VALUES('4f859a6f-9f6f-4642-9e4f-a565b0dd3342',613,251,93.7999999999999972,'2026-06-20T15:54:22Z');
INSERT INTO health_history VALUES('84beecf2-315c-47ed-b652-bb2121245db4',613,251,93.7999999999999972,'2026-06-20T21:54:25Z');
INSERT INTO health_history VALUES('991aa7d2-9a95-4b44-9c4b-b1e12ab845bb',613,251,93.7999999999999972,'2026-06-21T03:54:21Z');
INSERT INTO health_history VALUES('fc97943e-154e-4e4c-86c9-f4b2c70ff954',613,251,93.7999999999999972,'2026-06-21T09:54:36Z');
INSERT INTO health_history VALUES('260dccb9-e00f-4edd-a8d1-7e93559303f1',613,251,93.7999999999999972,'2026-06-21T15:54:29Z');
INSERT INTO health_history VALUES('ebf4a211-fa8d-4892-9151-ef58540778e8',614,251,93.8169381107497458,'2026-06-21T23:16:17Z');
INSERT INTO health_history VALUES('371885b9-7b51-4382-a2c7-eae17bbaa081',614,251,93.8169381107497458,'2026-06-22T00:05:43Z');
INSERT INTO health_history VALUES('cff31833-9bb6-4562-8381-699e8bdad629',613,251,93.7999999999999972,'2026-06-22T00:08:39Z');
INSERT INTO health_history VALUES('8d936c7b-715d-4aa1-b09c-25f45d2a80c1',613,251,93.8249592169662919,'2026-06-22T03:09:21Z');
INSERT INTO health_history VALUES('3498df3c-b492-4f7a-9839-844d48d3e0d6',613,252,93.83409461664003,'2026-06-22T03:15:33Z');
INSERT INTO health_history VALUES('9ebe3337-edff-45aa-96d2-789f3c817235',613,252,93.8520391517134414,'2026-06-22T05:52:05Z');
INSERT INTO health_history VALUES('a15ce716-9e02-4153-80b7-b26a8e861755',612,252,93.9000000000000056,'2026-06-22T06:00:42Z');
INSERT INTO health_history VALUES('769347b5-770a-45cc-ac04-1442004db055',612,252,93.9000000000000056,'2026-06-22T12:58:47Z');
INSERT INTO health_history VALUES('dbd1837e-52cb-44a3-8d8e-18476607ed0a',612,252,93.9000000000000056,'2026-06-22T19:42:25Z');
INSERT INTO health_history VALUES('466bec8a-9d2b-48c3-92de-56b4938396d1',612,252,93.9000000000000056,'2026-06-23T01:32:19Z');
INSERT INTO health_history VALUES('ab371095-703d-4860-a730-9091a5d6ee2b',612,252,93.9000000000000056,'2026-06-23T08:09:46Z');
INSERT INTO health_history VALUES('8208d5d3-88a9-48e0-9551-9aa83be7b2e5',612,252,93.9000000000000056,'2026-06-23T15:01:26Z');
INSERT INTO health_history VALUES('043de4b1-2606-4aad-a42c-1cb294189c9b',612,252,93.9000000000000056,'2026-06-23T21:35:11Z');
INSERT INTO health_history VALUES('9339437d-4784-465a-b9f4-49e8c6696d1a',612,225,93.4000000000000056,'2026-06-24T03:36:00Z');
INSERT INTO health_history VALUES('2ec93cc7-22cc-4be5-9e40-15efe2e14b8a',613,225,93.4349102773252013,'2026-06-24T05:59:45Z');
INSERT INTO health_history VALUES('2332e7c1-57a3-4bbb-ac95-fe19e4defa52',613,140,92.5323001631328595,'2026-07-01T20:15:51Z');
INSERT INTO health_history VALUES('b14e0b82-cc54-4598-8434-35426e09507d',613,148,92.6412724306695594,'2026-07-01T21:46:37Z');
INSERT INTO health_history VALUES('354eff2f-0ff5-4f7f-b7d7-79e902c3f39c',613,228,93.5879282218602953,'2026-07-02T01:51:22Z');
INSERT INTO health_history VALUES('82c50ac5-2ae4-4e6b-93f6-d0751139ca1a',613,228,93.5879282218602953,'2026-07-02T02:37:57Z');
INSERT INTO health_history VALUES('5b689e03-5a4f-467c-9096-c0f283b7ee77',613,228,93.5879282218602953,'2026-07-02T02:56:41Z');
INSERT INTO health_history VALUES('a3be4357-646f-4e3a-89cb-a33dbafa84ea',613,228,93.5879282218602953,'2026-07-02T03:43:01Z');
INSERT INTO health_history VALUES('d83b2f43-684f-48b0-934a-b04c5778d5a1',612,242,93.7999999999999972,'2026-07-02T04:10:10Z');
INSERT INTO health_history VALUES('c0ec83c2-8960-4f56-9596-4e66c620d9c1',613,242,93.7696574225127933,'2026-07-02T06:05:47Z');
INSERT INTO health_history VALUES('9beca68e-ab95-4f94-bf08-b5dba599f088',613,228,93.596900489397001,'2026-07-02T06:58:29Z');
INSERT INTO health_history VALUES('3263d252-5127-4232-a881-7e30ec36f817',612,248,93.7999999999999972,'2026-07-02T09:58:35Z');
INSERT INTO health_history VALUES('2c7f8f44-1e53-4c15-b3ce-c3da25adb93d',612,244,93.7999999999999972,'2026-07-02T15:58:37Z');
INSERT INTO health_history VALUES('ff165420-265d-4316-a09c-f1b403c012d3',612,248,93.7999999999999972,'2026-07-02T21:58:38Z');
INSERT INTO health_history VALUES('d1ff5ccb-8600-4277-80dd-be86a81ea856',613,249,93.8336052202289465,'2026-07-03T04:00:45Z');
INSERT INTO health_history VALUES('b323c3d4-f432-47ce-a4de-75f4bb67fdb9',612,250,93.7999999999999972,'2026-07-03T10:39:34Z');
INSERT INTO health_history VALUES('ae6b6b6b-0102-45f1-9e9d-2c85b095011b',612,250,93.7999999999999972,'2026-07-03T17:25:16Z');
INSERT INTO health_history VALUES('cdd21800-ddfa-4cea-98a8-1ca5f9d14060',613,250,93.8427406199026705,'2026-07-03T21:01:50Z');
INSERT INTO health_history VALUES('62379aaf-aad5-41cf-8fa8-3710a99931ff',612,252,93.9000000000000056,'2026-07-03T23:19:43Z');
INSERT INTO health_history VALUES('f516a706-ee3f-4d83-b90c-a5f4975a39f0',612,251,93.9000000000000056,'2026-07-04T05:19:47Z');
INSERT INTO health_history VALUES('b1d142be-1fe6-43d1-8b26-312ae02f5b86',612,252,93.9000000000000056,'2026-07-04T12:34:35Z');
INSERT INTO health_history VALUES('acb021ce-6cdd-4d20-b630-b6aa2e4c5357',612,252,93.9000000000000056,'2026-07-04T18:57:47Z');
INSERT INTO health_history VALUES('06a13032-874c-4665-a045-b9312d4f9ee8',614,252,93.7081433224761185,'2026-07-04T20:16:53Z');
INSERT INTO health_history VALUES('3c81277a-de58-41d3-a0db-54a8d7c5dfdb',613,252,93.7999999999999972,'2026-07-05T00:57:33Z');
CREATE TABLE webhooks (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    type TEXT NOT NULL DEFAULT 'generic',
    events TEXT NOT NULL DEFAULT '[]',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE platform_profiles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    is_builtin INTEGER NOT NULL DEFAULT 0,
    is_active INTEGER NOT NULL DEFAULT 0,
    nfo_enabled INTEGER NOT NULL DEFAULT 1,
    nfo_format TEXT NOT NULL DEFAULT 'kodi',
    image_naming TEXT NOT NULL DEFAULT '{}',
    use_symlinks INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO platform_profiles VALUES('emby','Emby',1,1,1,'kodi','{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}',0,'2026-04-24 03:47:21','2026-04-24T03:48:24Z');
INSERT INTO platform_profiles VALUES('jellyfin','Jellyfin',1,0,1,'kodi','{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}',0,'2026-04-24 03:47:21','2026-04-24T03:48:24Z');
INSERT INTO platform_profiles VALUES('kodi','Kodi',1,0,1,'kodi','{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}',0,'2026-04-24 03:47:21','2026-04-24T03:48:24Z');
INSERT INTO platform_profiles VALUES('plex','Plex',1,0,0,'kodi','{"thumb":["artist.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}',0,'2026-04-24 03:47:21','2026-04-24T03:48:24Z');
INSERT INTO platform_profiles VALUES('custom','Custom',1,0,1,'kodi','{"thumb":["folder.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}',1,'2026-04-24 03:47:21','2026-04-24T03:48:24Z');
CREATE TABLE bulk_jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'prompt_no_match',
    status TEXT NOT NULL DEFAULT 'pending',
    total_items INTEGER NOT NULL DEFAULT 0,
    processed_items INTEGER NOT NULL DEFAULT 0,
    fixed_items INTEGER NOT NULL DEFAULT 0,
    skipped_items INTEGER NOT NULL DEFAULT 0,
    failed_items INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    started_at TEXT,
    completed_at TEXT
);
CREATE TABLE bulk_job_items (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES bulk_jobs(id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL,
    artist_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    message TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE scraper_config (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL UNIQUE,
    config_json TEXT NOT NULL DEFAULT '{}',
    overrides_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO scraper_config VALUES('1fc0ff05-aad1-4f91-b248-25be5dddb132','global','{"id":"1fc0ff05-aad1-4f91-b248-25be5dddb132","scope":"global","fields":[{"field":"biography","primary":"lastfm","enabled":true,"category":"metadata"},{"field":"genres","primary":"musicbrainz","enabled":true,"category":"metadata"},{"field":"styles","primary":"discogs","enabled":true,"category":"metadata"},{"field":"moods","primary":"audiodb","enabled":true,"category":"metadata"},{"field":"members","primary":"musicbrainz","enabled":true,"category":"metadata"},{"field":"formed","primary":"musicbrainz","enabled":true,"category":"metadata"},{"field":"born","primary":"musicbrainz","enabled":true,"category":"metadata"},{"field":"died","primary":"musicbrainz","enabled":true,"category":"metadata"},{"field":"disbanded","primary":"musicbrainz","enabled":true,"category":"metadata"},{"field":"thumb","primary":"fanarttv","enabled":true,"category":"images"},{"field":"fanart","primary":"fanarttv","enabled":true,"category":"images"},{"field":"logo","primary":"fanarttv","enabled":true,"category":"images"},{"field":"banner","primary":"fanarttv","enabled":true,"category":"images"}],"fallback_chains":[{"category":"metadata","providers":["musicbrainz","wikipedia","lastfm","discogs","audiodb","wikidata","genius"]},{"category":"images","providers":["fanarttv","audiodb"]}],"created_at":"2026-03-29T03:23:04Z","updated_at":"2026-03-29T03:23:04Z"}','{}','2026-04-24T03:47:21Z','2026-04-24T03:48:24Z');
CREATE TABLE user_preferences (
    user_id    TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (user_id, key),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','auto_fetch_images','true','2026-04-24T03:48:24Z');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','bg_opacity','85','2026-07-01 08:02:15');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','content_width','wide','2026-07-01 07:39:24');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','font_size','large','2026-06-17 22:21:26');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','glass_intensity','light','2026-04-24T03:48:24Z');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','sidebar_state','full','2026-06-05 04:22:11');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','theme','dark','2026-07-01 04:58:47');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','font_family','atkinson','2026-06-10 20:52:15');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','letter_spacing','extra-wide','2026-06-04 21:39:54');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','metadata_languages','["en","fr"]','2026-05-27 00:36:52');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','page_size','30','2026-04-27 05:17:59');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','thumbnail_size','large','2026-06-10 20:57:21');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','lite_mode','auto','2026-06-05 04:22:18');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','metadata_name_romanization_fallback','true','2026-05-20 21:54:30');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','reduced_motion','system','2026-06-05 04:22:17');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','density','compact','2026-07-01 20:23:49');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','mono_font','cascadia','2026-06-17 22:20:56');
INSERT INTO user_preferences VALUES('0c67841a-8a5b-4fb8-bbe3-e1a77ebaac4d','show_platform_debug','false','2026-07-01T04:40:29Z');
CREATE TABLE artist_libraries (
			artist_id TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
			library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			source TEXT NOT NULL CHECK (source IN ('filesystem','emby','jellyfin','lidarr','manual')),
			added_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (artist_id, library_id)
		);
INSERT INTO artist_libraries VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','a253cc28-8c18-4385-8467-bcd0fd59f1b7','emby','2026-05-28 16:33:12');
INSERT INTO artist_libraries VALUES('40147a87-1eac-4894-8acd-08562bfb3272','a253cc28-8c18-4385-8467-bcd0fd59f1b7','emby','2026-05-28 16:33:13');
INSERT INTO artist_libraries VALUES('c849e893-f623-4ae5-b477-e37f254d216c','a253cc28-8c18-4385-8467-bcd0fd59f1b7','emby','2026-05-28 16:33:13');
INSERT INTO artist_libraries VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','a253cc28-8c18-4385-8467-bcd0fd59f1b7','emby','2026-05-28 16:33:13');
INSERT INTO artist_libraries VALUES('a17e6b40-759f-4d75-a791-867372deaca6','a253cc28-8c18-4385-8467-bcd0fd59f1b7','emby','2026-05-28 16:33:13');
INSERT INTO artist_libraries VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','a253cc28-8c18-4385-8467-bcd0fd59f1b7','emby','2026-05-28 16:33:13');
INSERT INTO artist_libraries VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','a253cc28-8c18-4385-8467-bcd0fd59f1b7','emby','2026-05-28 16:33:13');
INSERT INTO artist_libraries VALUES('40147a87-1eac-4894-8acd-08562bfb3272','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-05-31 01:03:05');
INSERT INTO artist_libraries VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-05-31 01:03:05');
INSERT INTO artist_libraries VALUES('a17e6b40-759f-4d75-a791-867372deaca6','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-05-31 01:03:05');
INSERT INTO artist_libraries VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-05-31 01:03:05');
INSERT INTO artist_libraries VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-05-31 01:03:05');
INSERT INTO artist_libraries VALUES('c849e893-f623-4ae5-b477-e37f254d216c','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-05-31 01:03:05');
INSERT INTO artist_libraries VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-05-31 01:03:05');
INSERT INTO artist_libraries VALUES('9bebfd87-47be-4a0b-9f8b-6763b2b73c42','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-07-05 23:59:28');
INSERT INTO artist_libraries VALUES('519c7d95-48f6-4a8f-b714-89d7f6e812a8','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-07-05 23:59:28');
INSERT INTO artist_libraries VALUES('de0c50cb-2b00-471f-842b-d38181fc684c','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-07-05 23:59:28');
INSERT INTO artist_libraries VALUES('e258a056-61b9-4e88-af6d-55fda6255f4f','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-07-05 23:59:28');
INSERT INTO artist_libraries VALUES('a7efd0cc-572a-4ed3-93cc-d7cf21d752e6','9f84cf3b-7d34-4c1d-aec9-a4a0e8dd6dc7','filesystem','2026-07-05 23:59:28');
ANALYZE sqlite_schema;
ANALYZE sqlite_schema;
CREATE TABLE foreign_files (
    id          TEXT NOT NULL PRIMARY KEY,
    artist_id   TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    file_path   TEXT NOT NULL,
    file_name   TEXT NOT NULL,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    detected_at TEXT NOT NULL DEFAULT (datetime('now')), content_hash TEXT,
    UNIQUE(artist_id, file_path)
);
INSERT INTO foreign_files VALUES('c4b7b5c6-d985-463b-a525-52987d497330','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','/music/Johann Sebastian Bach/poster.jpg','poster.jpg',83895,'2026-07-05T06:40:01Z','009686634cbf740698b7e0da93533ab589025290e9c1a2767472b49e713e924b');
INSERT INTO foreign_files VALUES('971aedeb-ae15-4fe4-9d1c-f7df11cd4b69','c14e15f5-4ff4-4415-b2ea-75de8cb4be57','/tmp/hero-1756/fixture/library/Johann Sebastian Bach/poster.jpg','poster.jpg',83895,'2026-07-06T00:03:23Z','009686634cbf740698b7e0da93533ab589025290e9c1a2767472b49e713e924b');
CREATE TABLE foreign_file_allowlist (
    id         TEXT NOT NULL PRIMARY KEY,
    scope      TEXT NOT NULL CHECK (scope IN ('global','artist')),
    artist_id  TEXT REFERENCES artists(id) ON DELETE CASCADE,
    file_name  TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
, content_hash TEXT);
CREATE TABLE IF NOT EXISTS "artists" (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    sort_name TEXT,
    type TEXT NOT NULL DEFAULT '',
    gender TEXT NOT NULL DEFAULT '',
    origin TEXT NOT NULL DEFAULT '',
    disambiguation TEXT NOT NULL DEFAULT '',
    genres TEXT NOT NULL DEFAULT '[]',
    styles TEXT NOT NULL DEFAULT '[]',
    moods TEXT NOT NULL DEFAULT '[]',
    years_active TEXT NOT NULL DEFAULT '',
    born TEXT NOT NULL DEFAULT '',
    formed TEXT NOT NULL DEFAULT '',
    died TEXT NOT NULL DEFAULT '',
    disbanded TEXT NOT NULL DEFAULT '',
    biography TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    nfo_exists INTEGER NOT NULL DEFAULT 0,
    health_score REAL NOT NULL DEFAULT 0.0,
    health_evaluated_at TEXT DEFAULT NULL,
    dirty_since TEXT DEFAULT NULL,
    rules_evaluated_at TEXT DEFAULT NULL,
    is_excluded INTEGER NOT NULL DEFAULT 0,
    exclusion_reason TEXT NOT NULL DEFAULT '',
    is_classical INTEGER NOT NULL DEFAULT 0,
    locked INTEGER NOT NULL DEFAULT 0,
    lock_source TEXT NOT NULL DEFAULT '' CHECK (lock_source IN ('', 'user', 'imported', 'initial_import', 'platform')),
    locked_at TEXT CHECK (locked = 0 OR (locked = 1 AND locked_at IS NOT NULL)),
    locked_fields TEXT NOT NULL DEFAULT '[]',
    metadata_sources TEXT NOT NULL DEFAULT '{}',
    last_scanned_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO artists VALUES('40147a87-1eac-4894-8acd-08562bfb3272','Antonio Vivaldi','Antonio Vivaldi','','','Venice','','[]','[]','[]','','','','','','Antonio Lucio Vivaldi was an Italian composer, virtuoso violinist and impresario of Baroque music. Regarded as one of the greatest Baroque composers, Vivaldi''s influence during his lifetime was widespread across Europe, giving origin to many imitators and admirers. He pioneered many developments in orchestration, violin technique and programmatic music. He consolidated the emerging concerto form, especially the solo concerto, into a widely accepted and followed idiom.','/tmp/hero-1756/fixture/library/Antonio Vivaldi',1,94.4000000000000056,'2026-07-05T00:40:47Z','2026-07-04T20:16:52Z','2026-07-05T00:40:35Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:06Z','2026-07-05T00:40:47Z');
INSERT INTO artists VALUES('ff6b4132-cfb2-46cf-b476-643c9ea00069','Claude Debussy','Claude Debussy','','','French','','["Classical"]','["Ballet","Chamber Music","Keyboard","Orchestral","Vocal Music"]','[]','','','','','','Achille Claude Debussy was a French composer. He is sometimes seen as the first Impressionist composer, although he vigorously rejected the term. He was among the most influential composers of the late 19th and early 20th centuries.','/tmp/hero-1756/fixture/library/Claude Debussy',1,100.0,'2026-07-05T00:42:57Z','2026-07-04T20:16:52Z','2026-07-05T00:42:50Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:06Z','2026-07-05T00:42:57Z');
INSERT INTO artists VALUES('a17e6b40-759f-4d75-a791-867372deaca6','George Frideric Handel','George Frideric Handel','','','Halle, Brandenburg–Prussia, Holy Roman Empire','','[]','["Chamber Music","Choral","Concerto","Keyboard","Opera","Orchestral"]','[]','','','','','',unistr('George Frideric Handel (as he called himself after his change of nationality, as he signed himself, and as he is known in the English-speaking world) (1685–1759) was a German/English baroque composer who was a leading composer of concerti grossi, operas, and oratorios.\u000a\u000aBorn Georg Friedrich Händel on 23rd February 1685 in Halle an der Saale (Germany), Handel lived most of his life in England, and became English by Act of Parliament in 1727. His most famous piece is the Messiah, an oratorio set to texts from the King James Bible; other well-known works are the Water Music and the Music for the Royal Fireworks. He deeply influenced many of the composers who came after him, including Franz Joseph Haydn, Wolfgang Amadeus Mozart, and Ludwig van Beethoven, and his work helped lead the transition from the Baroque to the classical era.  He died on 14th April 1759.'),'/tmp/hero-1756/fixture/library/George Frideric Handel',1,94.4000000000000056,'2026-07-05T00:45:38Z','2026-07-04T20:16:52Z','2026-07-05T00:45:20Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:07Z','2026-07-05T00:45:38Z');
INSERT INTO artists VALUES('c14e15f5-4ff4-4415-b2ea-75de8cb4be57','Johann Sebastian Bach','Bach, Johann Sebastian','solo','male','Eisenach','German Baroque period composer & musician','["baroque","cantata","Classical","mass","motet","Orchestral"]','["Classical"]','[]','1685-1750','1685-03-21','','1750-07-28','',unistr('Johann Sebastian Bach (31 March  1685 – 28 July 1750) was a German composer, organist, harpsichordist, violist, and violinist of the Baroque period. He enriched many established German styles through his skill in counterpoint, harmonic and motivic organisation, and the adaptation of rhythms, forms, and textures from abroad, particularly from Italy and France. Many of Bach''s works are still known today, such as the Brandenburg Concertos, the Mass in B minor, the The Well-Tempered Clavier, his cantatas, chorales, partitas, Passions, and organ works. His music is revered for its intellectual depth, technical command, and artistic beauty.\u000d\u000aBach was born in Eisenach, Saxe-Eisenach, into a very musical family; his father, Johann Ambrosius Bach was the director of the town musicians, and all of his uncles were professional musicians. His father taught him to play violin and harpsichord, and his brother, Johann Christoph Bach, taught him the clavichord and exposed him to much contemporary music. Bach also went to St Michael''s School in Lüneburg because of his singing skills. After graduating, he held several musical posts across Germany: he served as Kapellmeister (director of music) to Leopold, Prince of Anhalt-Köthen, Cantor of Thomasschule in Leipzig, and Royal Court Composer to August III. Bach''s health and vision declined in 1749, and he died on 28 July 1750. Modern historians believe that his death was caused by a combination of stroke and pneumonia.\u000d\u000aBach''s abilities as an organist were highly respected throughout Europe during his lifetime, although he was not widely recognised as a great composer until a revival of interest and performances of his music in the first half of the 19th century. He is now generally regarded as one of the main composers of the Baroque period, and as one of the greatest composers of all time.'),'/tmp/hero-1756/fixture/library/Johann Sebastian Bach',1,83.29999999999999716,'2026-07-06T00:10:06Z','2026-07-06T00:10:04Z','2026-07-06T00:04:47Z',0,'',1,0,'',NULL,'[]','{"biography":"audiodb","born":"musicbrainz","died":"musicbrainz","disbanded":"audiodb","fanart":"fanarttv","genres":"musicbrainz","logo":"audiodb","styles":"audiodb","thumb":"fanarttv"}','2026-07-04T20:16:52Z','2026-04-24T04:00:08Z','2026-07-06T00:10:06Z');
INSERT INTO artists VALUES('179b3e46-daae-418e-a964-14b08c7a6b17','Johannes Brahms','Johannes Brahms','','','Deutch, Hamburg','','["Classical"]','["Chamber Music","Keyboard","Symphony","Vocal Music"]','[]','','','','','','Johannes Brahms was a German composer, virtuoso pianist, and conductor of the mid-Romantic period. His music is noted for its rhythmic vitality and freer treatment of dissonance, often set within studied yet expressive contrapuntal textures. He adapted the traditional structures and techniques of a wide historical range of earlier composers. His œuvre includes four symphonies, four concertos, a Requiem, much chamber music, and hundreds of folk-song arrangements and Lieder, among other works for symphony orchestra, piano, organ, and choir.','/tmp/hero-1756/fixture/library/Johannes Brahms',1,100.0,'2026-07-05T00:47:56Z','2026-07-04T20:16:52Z','2026-07-05T00:47:44Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:08Z','2026-07-05T00:47:56Z');
INSERT INTO artists VALUES('c849e893-f623-4ae5-b477-e37f254d216c','Ludwig van Beethoven','Ludwig van Beethoven','','','Bonn, Germany','','[]','["Chamber Music","Choral","Concerto","Keyboard","Orchestral","Symphony","Vocal Music"]','[]','','','','','','Ludwig van Beethoven was a German composer and pianist. One of the most revered figures in the history of Western music, his works rank among the most performed of the classical music repertoire and span the transition from the Classical period to the Romantic era. Beethoven''s early period, during which he forged his craft, is typically considered to have lasted until 1802. From 1802 to around 1812, his middle period showed an individual development from the styles of Joseph Haydn and Wolfgang Amadeus Mozart, and is sometimes characterised as heroic. During this time, Beethoven began to grow increasingly deaf. In his late period, from 1812 to 1827, he extended his innovations in musical form and expression.','/tmp/hero-1756/fixture/library/Ludwig van Beethoven',1,100.0,'2026-07-05T00:50:14Z','2026-07-04T20:16:52Z','2026-07-05T00:49:59Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:08Z','2026-07-05T00:50:14Z');
INSERT INTO artists VALUES('f1a5358a-d455-4004-921e-f01dfbcb427b','Wolfgang Amadeus Mozart','Wolfgang Amadeus Mozart','','','Salzburg, Austrian','','[]','[]','[]','','','','','','Wolfgang Amadeus Mozart was a prolific and influential composer of the Classical period. Despite his brief life, his rapid pace of composition and proficiency from an early age resulted in more than 800 works representing virtually every classical genre of his time. Many of these compositions are acknowledged as pinnacles of the symphonic, concertante, chamber, opera, and choral repertoires. Mozart is widely regarded as one of the greatest composers in the history of Western music, with his music admired for its "melodic beauty, its formal elegance and its richness of harmony and texture".','/tmp/hero-1756/fixture/library/Wolfgang Amadeus Mozart',1,94.4000000000000056,'2026-07-05T00:57:11Z','2026-07-04T20:16:52Z','2026-07-05T00:56:58Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:09Z','2026-07-05T00:57:11Z');
INSERT INTO artists VALUES('9bebfd87-47be-4a0b-9f8b-6763b2b73c42','Franz Liszt','Franz Liszt','','','Venice','','[]','[]','[]','','','','','','Franz Liszt is a public-domain classical composer. (Fixture bio placeholder for the #1756 README hero; not shown as product copy.)','/tmp/hero-1756/fixture/library/Franz Liszt',1,94.4000000000000056,'2026-07-05T00:40:47Z','2026-07-04T20:16:52Z','2026-07-05T00:40:35Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:06Z','2026-07-05T00:40:47Z');
INSERT INTO artists VALUES('519c7d95-48f6-4a8f-b714-89d7f6e812a8','Franz Schubert','Franz Schubert','','','Venice','','[]','[]','[]','','','','','','Franz Schubert is a public-domain classical composer. (Fixture bio placeholder for the #1756 README hero; not shown as product copy.)','/tmp/hero-1756/fixture/library/Franz Schubert',1,94.4000000000000056,'2026-07-05T00:40:47Z','2026-07-04T20:16:52Z','2026-07-05T00:40:35Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:06Z','2026-07-05T00:40:47Z');
INSERT INTO artists VALUES('de0c50cb-2b00-471f-842b-d38181fc684c','Frederic Chopin','Frederic Chopin','','','Venice','','[]','[]','[]','','','','','','Frederic Chopin is a public-domain classical composer. (Fixture bio placeholder for the #1756 README hero; not shown as product copy.)','/tmp/hero-1756/fixture/library/Frederic Chopin',1,94.4000000000000056,'2026-07-05T00:40:47Z','2026-07-04T20:16:52Z','2026-07-05T00:40:35Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:06Z','2026-07-05T00:40:47Z');
INSERT INTO artists VALUES('e258a056-61b9-4e88-af6d-55fda6255f4f','Joseph Haydn','Joseph Haydn','','','Venice','','[]','[]','[]','','','','','','Joseph Haydn is a public-domain classical composer. (Fixture bio placeholder for the #1756 README hero; not shown as product copy.)','/tmp/hero-1756/fixture/library/Joseph Haydn',1,94.4000000000000056,'2026-07-05T00:40:47Z','2026-07-04T20:16:52Z','2026-07-05T00:40:35Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:06Z','2026-07-05T00:40:47Z');
INSERT INTO artists VALUES('a7efd0cc-572a-4ed3-93cc-d7cf21d752e6','Pyotr Ilyich Tchaikovsky','Pyotr Ilyich Tchaikovsky','','','Venice','','[]','[]','[]','','','','','','Pyotr Ilyich Tchaikovsky is a public-domain classical composer. (Fixture bio placeholder for the #1756 README hero; not shown as product copy.)','/tmp/hero-1756/fixture/library/Pyotr Ilyich Tchaikovsky',1,94.4000000000000056,'2026-07-05T00:40:47Z','2026-07-04T20:16:52Z','2026-07-05T00:40:35Z',0,'',1,0,'',NULL,'[]','{}','2026-07-04T20:16:52Z','2026-04-24T04:00:06Z','2026-07-05T00:40:47Z');
CREATE TABLE ignored_duplicate_groups (
    id         TEXT NOT NULL PRIMARY KEY,
    signature  TEXT NOT NULL UNIQUE,
    group_key  TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO sqlite_sequence VALUES('goose_db_version',18);
CREATE TRIGGER prevent_deactivate_protected_user
BEFORE UPDATE OF is_active ON users
FOR EACH ROW
WHEN OLD.is_protected = 1 AND NEW.is_active = 0
BEGIN
    SELECT RAISE(ABORT, 'cannot deactivate a protected user');
END;
CREATE TRIGGER prevent_role_change_protected_user
BEFORE UPDATE OF role ON users
FOR EACH ROW
WHEN OLD.is_protected = 1
BEGIN
    SELECT RAISE(ABORT, 'cannot change role of a protected user');
END;
CREATE TRIGGER prevent_delete_protected_user
BEFORE DELETE ON users
FOR EACH ROW
WHEN OLD.is_protected = 1
BEGIN
    SELECT RAISE(ABORT, 'cannot delete a protected user');
END;
CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_expires_at ON sessions(expires_at);
CREATE INDEX idx_api_tokens_hash ON api_tokens(token_hash);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);
CREATE INDEX idx_audit_log_token_id ON audit_log(token_id);
CREATE INDEX idx_audit_log_user_id ON audit_log(user_id);
CREATE INDEX idx_invites_code ON invites(code);
CREATE UNIQUE INDEX idx_libraries_connection_external
    ON libraries(connection_id, external_id)
    WHERE connection_id IS NOT NULL AND external_id <> '';
CREATE INDEX idx_provider_ids_lookup ON artist_provider_ids(provider, provider_id);
CREATE INDEX idx_artist_images_artist_id ON artist_images(artist_id);
CREATE INDEX idx_artist_platform_ids_connection
    ON artist_platform_ids(connection_id);
CREATE INDEX idx_artist_aliases_artist_id ON artist_aliases(artist_id);
CREATE INDEX idx_band_members_artist_id ON band_members(artist_id);
CREATE INDEX idx_band_members_member_mbid ON band_members(member_mbid);
CREATE INDEX idx_nfo_snapshots_artist_id ON nfo_snapshots(artist_id);
CREATE INDEX idx_metadata_changes_artist ON metadata_changes(artist_id, created_at DESC);
CREATE INDEX idx_metadata_changes_created ON metadata_changes(created_at DESC);
CREATE INDEX idx_mb_snapshots_artist ON mb_snapshots(artist_id);
CREATE INDEX idx_rule_violations_rule_id ON rule_violations(rule_id);
CREATE INDEX idx_rule_violations_artist_id ON rule_violations(artist_id);
CREATE INDEX idx_rule_violations_status ON rule_violations(status);
CREATE INDEX idx_rule_violations_created_at ON rule_violations(created_at);
CREATE INDEX idx_rule_violations_resolved_at ON rule_violations(resolved_at);
CREATE INDEX idx_rule_results_rule_id      ON rule_results(rule_id);
CREATE INDEX idx_rule_results_artist_id    ON rule_results(artist_id);
CREATE INDEX idx_rule_results_passed       ON rule_results(passed);
CREATE INDEX idx_rule_results_evaluated_at ON rule_results(evaluated_at);
CREATE INDEX idx_health_history_recorded_at ON health_history(recorded_at);
CREATE INDEX idx_bulk_job_items_job_id ON bulk_job_items(job_id);
CREATE INDEX idx_scraper_config_scope ON scraper_config(scope);
CREATE UNIQUE INDEX idx_artist_platform_ids_unique
		ON artist_platform_ids(connection_id, platform_artist_id)
	;
CREATE INDEX idx_rule_results_violation_id ON rule_results(violation_id);
CREATE INDEX idx_artist_libraries_library
		ON artist_libraries(library_id)
	;
CREATE INDEX idx_foreign_files_artist ON foreign_files(artist_id);
CREATE INDEX idx_foreign_files_detected_at ON foreign_files(detected_at);
CREATE INDEX idx_foreign_allowlist_hash
    ON foreign_file_allowlist(scope, artist_id, content_hash);
CREATE UNIQUE INDEX idx_foreign_allowlist_global_hash
    ON foreign_file_allowlist(content_hash)
    WHERE scope = 'global' AND content_hash IS NOT NULL;
CREATE UNIQUE INDEX idx_foreign_allowlist_artist_hash
    ON foreign_file_allowlist(artist_id, content_hash)
    WHERE scope = 'artist' AND content_hash IS NOT NULL;
CREATE INDEX idx_artists_name ON artists(name);
CREATE INDEX idx_artists_path ON artists(path);
CREATE INDEX idx_artists_locked ON artists(locked);
CREATE INDEX idx_artists_dirty_eval ON artists(dirty_since, rules_evaluated_at);
CREATE INDEX idx_artists_name_lower ON artists(LOWER(name));
CREATE INDEX idx_artists_name_id ON artists(name, id);
COMMIT;
