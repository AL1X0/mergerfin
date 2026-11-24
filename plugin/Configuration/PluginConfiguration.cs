using System;
using System.Collections.Generic;
using MediaBrowser.Model.Plugins;

namespace Jellyfin.Plugin.MergerFin.Configuration;

/// <summary>
/// Persistent configuration for the MergerFin plugin (Full API Key mode).
/// </summary>
public class PluginConfiguration : BasePluginConfiguration
{
    /// <summary>
    /// Configured remote Jellyfin servers to federate via API Key.
    /// </summary>
    public List<RemoteServerConfig> Servers { get; set; } = [];

    /// <summary>
    /// Optional directory where .strm files are generated for remote-only movies and episodes.
    /// Leave empty to only inject remote versions onto existing local items without creating .strm files.
    /// </summary>
    public string StrmOutputPath { get; set; } = string.Empty;

    /// <summary>
    /// Local loopback base URL used inside generated .strm files and MediaSourceInfo paths.
    /// Defaults to http://127.0.0.1:8096.
    /// </summary>
    public string LocalBaseUrl { get; set; } = "http://127.0.0.1:8096";

    /// <summary>
    /// Propagate local playback progress, played state, and favorites back to remote servers
    /// by automatically matching remote users by username via API Key.
    /// </summary>
    public bool SyncWatchStateToUpstreams { get; set; } = true;

    /// <summary>
    /// Inject remote MediaSources onto matching local movies and episodes via IMediaSourceProvider.
    /// </summary>
    public bool EnableMultiVersionInjection { get; set; } = true;

    /// <summary>
    /// When true, do not create .strm files for movies/episodes already present locally (they are injected as extra versions instead).
    /// Set to false if you use a dedicated separate library (e.g. "Films SYNC") and want all remote items listed there.
    /// </summary>
    public bool SkipLocalDuplicatesInStrm { get; set; } = true;
}

public class RemoteServerConfig
{
    public string Id { get; set; } = Guid.NewGuid().ToString("N");

    public string Name { get; set; } = string.Empty;

    public string BaseUrl { get; set; } = string.Empty;

    /// <summary>
    /// Jellyfin API Key created in Dashboard -> API Keys on the remote server.
    /// </summary>
    public string ApiKey { get; set; } = string.Empty;

    /// <summary>
    /// Legacy fallback for existing XML configs migrated from v0.1.0-v0.1.3.
    /// </summary>
    public string AccessToken { get; set; } = string.Empty;

    public int Priority { get; set; } = 100;

    /// <summary>
    /// "proxy" (stream bytes through local Jellyfin) or "redirect" (HTTP 302 to remote server).
    /// </summary>
    public string StreamMode { get; set; } = "proxy";

    public bool Enabled { get; set; } = true;

    /// <summary>
    /// Optional per-library routing overrides for this remote server.
    /// Libraries not listed (or set to "auto") are automatically routed by name and type
    /// (e.g. libraries containing anime/animé/japanim go to "Anime", movies to "Movies", shows to "Shows").
    /// </summary>
    public List<LibraryMappingConfig> LibraryMappings { get; set; } = [];

    public string GetEffectiveApiKey()
    {
        return !string.IsNullOrWhiteSpace(ApiKey) ? ApiKey.Trim() : (AccessToken ?? string.Empty).Trim();
    }
}

public class LibraryMappingConfig
{
    /// <summary>
    /// Remote Jellyfin library ItemId (from GET /Library/VirtualFolders).
    /// </summary>
    public string RemoteLibraryId { get; set; } = string.Empty;

    /// <summary>
    /// Cached remote library name (used for UI display and fallback matching).
    /// </summary>
    public string RemoteLibraryName { get; set; } = string.Empty;

    /// <summary>
    /// Target subfolder under StrmOutputPath:
    /// "" or "auto" = automatic detection, "Movies", "Shows", "Anime", any custom folder name,
    /// or "ignore" = skip this remote library completely.
    /// </summary>
    public string TargetFolder { get; set; } = string.Empty;
}
