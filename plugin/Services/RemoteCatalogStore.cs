using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Jellyfin.Plugin.MergerFin.Models;
using MediaBrowser.Common.Configuration;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// Thread-safe local index of remote Jellyfin media sources persisted in the plugin's own data folder.
/// Never modifies Jellyfin's internal library.db.
/// </summary>
public class RemoteCatalogStore
{
    private readonly object _lock = new();
    private readonly string _snapshotPath;
    private readonly ILogger<RemoteCatalogStore> _logger;

    private Dictionary<string, IndexedRemoteSource> _bySourceId = new(StringComparer.OrdinalIgnoreCase);
    private Dictionary<string, List<IndexedRemoteSource>> _byLookupKey = new(StringComparer.OrdinalIgnoreCase);
    private DateTime _lastSyncUtc;
    private string _lastError = string.Empty;

    public RemoteCatalogStore(IApplicationPaths appPaths, ILogger<RemoteCatalogStore> logger)
    {
        _logger = logger;
        var dir = Path.Combine(appPaths.DataPath, "mergerfin");
        Directory.CreateDirectory(dir);
        _snapshotPath = Path.Combine(dir, "catalog-index.json");
        LoadFromDisk();
    }

    public static string DeterministicSourceId(string serverId, string remoteMediaSourceId)
    {
        var hash = SHA256.HashData(Encoding.UTF8.GetBytes($"mf-source:{serverId}:{remoteMediaSourceId}"));
        return Convert.ToHexString(hash[..16]).ToLowerInvariant();
    }

    public static IEnumerable<string> BuildLookupKeys(
        string itemType,
        IDictionary<string, string>? providerIds,
        IDictionary<string, string>? seriesProviderIds = null,
        int? seasonNumber = null,
        int? episodeNumber = null)
    {
        var keys = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        if (string.Equals(itemType, "Movie", StringComparison.OrdinalIgnoreCase))
        {
            if (providerIds != null)
            {
                AddProviderKey(keys, "movie:tmdb:", providerIds, "Tmdb");
                AddProviderKey(keys, "movie:imdb:", providerIds, "Imdb");
                AddProviderKey(keys, "movie:tvdb:", providerIds, "Tvdb");
            }
        }
        else if (string.Equals(itemType, "Episode", StringComparison.OrdinalIgnoreCase))
        {
            if (providerIds != null)
            {
                AddProviderKey(keys, "ep:tvdb:", providerIds, "Tvdb");
                AddProviderKey(keys, "ep:imdb:", providerIds, "Imdb");
                AddProviderKey(keys, "ep:tmdb:", providerIds, "Tmdb");
            }

            if (seriesProviderIds != null && seasonNumber.HasValue && episodeNumber.HasValue)
            {
                var suffix = $":s{seasonNumber.Value}e{episodeNumber.Value}";
                AddProviderKey(keys, "ep:stmdb:", seriesProviderIds, "Tmdb", suffix);
                AddProviderKey(keys, "ep:stvdb:", seriesProviderIds, "Tvdb", suffix);
                AddProviderKey(keys, "ep:simdb:", seriesProviderIds, "Imdb", suffix);
            }
        }

        return keys;
    }

    private static void AddProviderKey(
        HashSet<string> keys,
        string prefix,
        IDictionary<string, string> dict,
        string providerName,
        string suffix = "")
    {
        foreach (var kv in dict)
        {
            if (string.Equals(kv.Key, providerName, StringComparison.OrdinalIgnoreCase) && !string.IsNullOrWhiteSpace(kv.Value))
            {
                keys.Add(prefix + kv.Value.Trim().ToLowerInvariant() + suffix);
            }
        }
    }

    public void ReplaceAll(List<IndexedRemoteSource> sources, string lastError = "")
    {
        var byId = new Dictionary<string, IndexedRemoteSource>(StringComparer.OrdinalIgnoreCase);
        var byKey = new Dictionary<string, List<IndexedRemoteSource>>(StringComparer.OrdinalIgnoreCase);

        foreach (var s in sources)
        {
            byId[s.SourceId] = s;
            foreach (var key in BuildLookupKeys(s.ItemType, s.ProviderIds, s.SeriesProviderIds, s.SeasonNumber, s.EpisodeNumber))
            {
                if (!byKey.TryGetValue(key, out var list))
                {
                    list = [];
                    byKey[key] = list;
                }

                list.Add(s);
            }
        }

        foreach (var list in byKey.Values)
        {
            list.Sort((a, b) =>
            {
                var p = a.ServerPriority.CompareTo(b.ServerPriority);
                return p != 0 ? p : b.Bitrate.CompareTo(a.Bitrate);
            });
        }

        var now = DateTime.UtcNow;
        lock (_lock)
        {
            _bySourceId = byId;
            _byLookupKey = byKey;
            _lastSyncUtc = now;
            _lastError = lastError;
        }

        SaveToDisk(new RemoteCatalogSnapshot
        {
            LastSyncUtc = now,
            LastError = lastError,
            Sources = sources
        });
    }

    public IndexedRemoteSource? GetBySourceId(string sourceId)
    {
        if (string.IsNullOrWhiteSpace(sourceId))
        {
            return null;
        }

        var clean = sourceId.StartsWith("mf_", StringComparison.OrdinalIgnoreCase) ? sourceId[3..] : sourceId;
        lock (_lock)
        {
            return _bySourceId.TryGetValue(clean, out var src) ? src : null;
        }
    }

    public List<IndexedRemoteSource> FindMatches(
        string itemType,
        IDictionary<string, string>? providerIds,
        IDictionary<string, string>? seriesProviderIds = null,
        int? seasonNumber = null,
        int? episodeNumber = null)
    {
        var keys = BuildLookupKeys(itemType, providerIds, seriesProviderIds, seasonNumber, episodeNumber);
        var seen = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        var result = new List<IndexedRemoteSource>();

        lock (_lock)
        {
            foreach (var k in keys)
            {
                if (_byLookupKey.TryGetValue(k, out var list))
                {
                    foreach (var s in list)
                    {
                        if (seen.Add(s.SourceId))
                        {
                            result.Add(s);
                        }
                    }
                }
            }
        }

        result.Sort((a, b) =>
        {
            var p = a.ServerPriority.CompareTo(b.ServerPriority);
            return p != 0 ? p : b.Bitrate.CompareTo(a.Bitrate);
        });

        return result;
    }

    public (int TotalSources, int Movies, int Episodes, DateTime LastSyncUtc, string LastError) GetStats()
    {
        lock (_lock)
        {
            var movies = _bySourceId.Values.Count(s => string.Equals(s.ItemType, "Movie", StringComparison.OrdinalIgnoreCase));
            var episodes = _bySourceId.Values.Count(s => string.Equals(s.ItemType, "Episode", StringComparison.OrdinalIgnoreCase));
            return (_bySourceId.Count, movies, episodes, _lastSyncUtc, _lastError);
        }
    }

    private void LoadFromDisk()
    {
        try
        {
            if (!File.Exists(_snapshotPath))
            {
                return;
            }

            var json = File.ReadAllText(_snapshotPath);
            var snap = JsonSerializer.Deserialize<RemoteCatalogSnapshot>(json);
            if (snap != null && snap.Sources.Count > 0)
            {
                ReplaceAll(snap.Sources, snap.LastError);
                lock (_lock)
                {
                    _lastSyncUtc = snap.LastSyncUtc;
                }
            }
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "MergerFin failed to load cached catalog index from {Path}", _snapshotPath);
        }
    }

    private void SaveToDisk(RemoteCatalogSnapshot snapshot)
    {
        try
        {
            var tmp = _snapshotPath + ".tmp";
            File.WriteAllText(tmp, JsonSerializer.Serialize(snapshot));
            File.Move(tmp, _snapshotPath, overwrite: true);
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "MergerFin failed to save catalog index to {Path}", _snapshotPath);
        }
    }
}
