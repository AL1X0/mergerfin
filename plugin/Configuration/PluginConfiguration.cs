using System;
using System.Collections.Generic;
using MediaBrowser.Model.Plugins;

namespace Jellyfin.Plugin.MergerFin.Configuration;

/// <summary>
/// Persistent configuration for the MergerFin plugin.
/// </summary>
public class PluginConfiguration : BasePluginConfiguration
{
    /// <summary>
    /// Configured remote Jellyfin servers to federate.
    /// </summary>
    public List<RemoteServerConfig> Servers { get; set; } = [];

    /// <summary>
    /// Optional per-user remote account mappings (Local Jellyfin User -> Remote Server Account).
    /// </summary>
    public List<UserAccountMapping> UserMappings { get; set; } = [];

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
    /// Propagate local playback progress, played state, and favorites back to remote servers.
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

    public string Username { get; set; } = string.Empty;

    public string RemoteUserId { get; set; } = string.Empty;

    public string AccessToken { get; set; } = string.Empty;

    public int Priority { get; set; } = 100;

    /// <summary>
    /// "proxy" (stream bytes through local Jellyfin) or "redirect" (HTTP 302 to remote server).
    /// </summary>
    public string StreamMode { get; set; } = "proxy";

    public bool Enabled { get; set; } = true;
}

public class UserAccountMapping
{
    public string LocalUserId { get; set; } = string.Empty;

    public string LocalUsername { get; set; } = string.Empty;

    public string ServerId { get; set; } = string.Empty;

    public string RemoteUsername { get; set; } = string.Empty;

    public string RemoteUserId { get; set; } = string.Empty;

    public string AccessToken { get; set; } = string.Empty;
}
