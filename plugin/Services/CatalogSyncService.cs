using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Data.Enums;
using Jellyfin.Plugin.MergerFin.Configuration;
using Jellyfin.Plugin.MergerFin.Models;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Entities.Movies;
using MediaBrowser.Controller.Entities.TV;
using MediaBrowser.Controller.Library;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// Synchronizes catalogs from all enabled remote Jellyfin servers, updates RemoteCatalogStore,
/// and optionally writes .strm stubs for remote-only items not already present locally.
/// </summary>
public class CatalogSyncService
{
    private readonly UpstreamClient _client;
    private readonly RemoteCatalogStore _store;
    private readonly ILibraryManager _libraryManager;
    private readonly ILogger<CatalogSyncService> _logger;
    private readonly SemaphoreSlim _syncLock = new(1, 1);

    private volatile bool _isRunning;

    public CatalogSyncService(
        UpstreamClient client,
        RemoteCatalogStore store,
        ILibraryManager libraryManager,
        ILogger<CatalogSyncService> logger)
    {
        _client = client;
        _store = store;
        _libraryManager = libraryManager;
        _logger = logger;
    }

    public bool IsRunning => _isRunning;

    public async Task SyncAllAsync(IProgress<double>? progress, CancellationToken ct)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null)
        {
            return;
        }

        if (!await _syncLock.WaitAsync(0, ct).ConfigureAwait(false))
        {
            _logger.LogInformation("MergerFin sync already in progress; skipping overlapping trigger.");
            return;
        }

        _isRunning = true;
        try
        {
            var enabledServers = cfg.Servers
                .Where(s => s.Enabled && !string.IsNullOrWhiteSpace(s.BaseUrl) && !string.IsNullOrWhiteSpace(s.AccessToken))
                .OrderBy(s => s.Priority)
                .ToList();

            var allSources = new List<IndexedRemoteSource>();
            var errors = new List<string>();

            for (var i = 0; i < enabledServers.Count; i++)
            {
                ct.ThrowIfCancellationRequested();
                var srv = enabledServers[i];
                try
                {
                    var serverSources = await SyncServerItemsAsync(srv, ct).ConfigureAwait(false);
                    allSources.AddRange(serverSources);
                    _logger.LogInformation("MergerFin indexed {Count} remote sources from {Server}", serverSources.Count, srv.Name);
                }
                catch (Exception ex)
                {
                    _logger.LogWarning(ex, "MergerFin failed to sync server {Server}", srv.Name);
                    errors.Add($"{srv.Name}: {ex.Message}");
                }

                progress?.Report(enabledServers.Count > 0 ? ((i + 1) * 70.0) / enabledServers.Count : 70.0);
            }

            _store.ReplaceAll(allSources, string.Join("; ", errors));

            if (!string.IsNullOrWhiteSpace(cfg.StrmOutputPath))
            {
                WriteStrmStubsForRemoteOnlyItems(cfg, allSources, ct);
            }

            progress?.Report(100.0);
        }
        finally
        {
            _isRunning = false;
            _syncLock.Release();
        }
    }

    private async Task<List<IndexedRemoteSource>> SyncServerItemsAsync(RemoteServerConfig srv, CancellationToken ct)
    {
        var result = new List<IndexedRemoteSource>();

        // 1. Fetch Series first to map SeriesId -> ProviderIds for episode deduplication.
        var seriesList = await _client.FetchAllItemsAsync(srv, ["Series"], ct).ConfigureAwait(false);
        var seriesProvidersById = new Dictionary<string, Dictionary<string, string>>(StringComparer.OrdinalIgnoreCase);
        var seriesNameById = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var s in seriesList)
        {
            if (s.ProviderIds != null && s.ProviderIds.Count > 0)
            {
                seriesProvidersById[s.Id] = new Dictionary<string, string>(s.ProviderIds, StringComparer.OrdinalIgnoreCase);
            }

            if (!string.IsNullOrWhiteSpace(s.Name))
            {
                seriesNameById[s.Id] = s.Name;
            }
        }

        // 2. Fetch Movies & Episodes with MediaSources + ProviderIds.
        var mediaItems = await _client.FetchAllItemsAsync(srv, ["Movie", "Episode"], ct).ConfigureAwait(false);
        foreach (var item in mediaItems)
        {
            if (item.MediaSources == null || item.MediaSources.Count == 0)
            {
                continue;
            }

            var itemProviders = item.ProviderIds != null
                ? new Dictionary<string, string>(item.ProviderIds, StringComparer.OrdinalIgnoreCase)
                : new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);

            var seriesProviders = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            var seriesName = item.SeriesName ?? string.Empty;
            if (!string.IsNullOrWhiteSpace(item.SeriesId))
            {
                if (seriesProvidersById.TryGetValue(item.SeriesId, out var sp))
                {
                    seriesProviders = sp;
                }

                if (string.IsNullOrWhiteSpace(seriesName) && seriesNameById.TryGetValue(item.SeriesId, out var sn))
                {
                    seriesName = sn;
                }
            }

            foreach (var ms in item.MediaSources)
            {
                if (string.IsNullOrWhiteSpace(ms.Id))
                {
                    continue;
                }

                var streams = (ms.MediaStreams ?? []).Select(st => new IndexedMediaStream
                {
                    Type = st.Type,
                    Index = st.Index,
                    Codec = st.Codec ?? string.Empty,
                    Language = st.Language ?? string.Empty,
                    Title = st.Title ?? string.Empty,
                    DisplayTitle = st.DisplayTitle ?? string.Empty,
                    IsDefault = st.IsDefault,
                    IsForced = st.IsForced,
                    IsExternal = st.IsExternal,
                    Width = st.Width,
                    Height = st.Height,
                    BitRate = st.BitRate,
                    Channels = st.Channels,
                    SampleRate = st.SampleRate
                }).ToList();

                result.Add(new IndexedRemoteSource
                {
                    SourceId = RemoteCatalogStore.DeterministicSourceId(srv.Id, ms.Id),
                    ServerId = srv.Id,
                    ServerName = srv.Name,
                    ServerPriority = srv.Priority,
                    ItemType = item.Type,
                    RemoteItemId = item.Id,
                    RemoteMediaSourceId = ms.Id,
                    Title = item.Name,
                    ProductionYear = item.ProductionYear,
                    SeriesName = seriesName,
                    SeasonNumber = item.ParentIndexNumber,
                    EpisodeNumber = item.IndexNumber,
                    ProviderIds = itemProviders,
                    SeriesProviderIds = seriesProviders,
                    Label = BuildSourceLabel(srv.Name, ms, streams),
                    Container = ms.Container ?? string.Empty,
                    Size = ms.Size ?? 0,
                    Bitrate = ms.Bitrate ?? 0,
                    RunTimeTicks = ms.RunTimeTicks ?? item.RunTimeTicks ?? 0,
                    Streams = streams
                });
            }
        }

        return result;
    }

    private static string BuildSourceLabel(string serverName, RemoteMediaSourceDto ms, List<IndexedMediaStream> streams)
    {
        var video = streams.FirstOrDefault(s => string.Equals(s.Type, "Video", StringComparison.OrdinalIgnoreCase));
        var parts = new List<string>();
        if (video?.Height is > 0)
        {
            parts.Add($"{video.Height}p");
        }

        if (!string.IsNullOrWhiteSpace(video?.Codec))
        {
            parts.Add(video.Codec.ToUpperInvariant());
        }

        if (ms.Bitrate is > 0)
        {
            var mbps = ms.Bitrate.Value / 1_000_000.0;
            parts.Add(string.Format(CultureInfo.InvariantCulture, "({0:0.0} Mbps)", mbps));
        }

        var detail = parts.Count > 0 ? string.Join(" ", parts) : (ms.Container ?? "Stream").ToUpperInvariant();
        return $"[{serverName}] {detail}";
    }

    private void WriteStrmStubsForRemoteOnlyItems(PluginConfiguration cfg, List<IndexedRemoteSource> allSources, CancellationToken ct)
    {
        var root = cfg.StrmOutputPath.Trim();
        if (string.IsNullOrWhiteSpace(root))
        {
            return;
        }

        try
        {
            Directory.CreateDirectory(root);
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "MergerFin cannot create StrmOutputPath {Path}", root);
            return;
        }

        var localBase = (cfg.LocalBaseUrl ?? "http://127.0.0.1:8096").Trim().TrimEnd('/');

        // Collect all local (non-STRM) item keys so we NEVER create a .strm duplicate for a movie/episode already on disk.
        var localKeys = CollectLocalNonStrmKeys(root);

        var writtenFiles = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        var handledCanonicalKeys = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        foreach (var src in allSources.OrderBy(s => s.ServerPriority).ThenByDescending(s => s.Bitrate))
        {
            ct.ThrowIfCancellationRequested();

            var keys = RemoteCatalogStore.BuildLookupKeys(
                src.ItemType, src.ProviderIds, src.SeriesProviderIds, src.SeasonNumber, src.EpisodeNumber).ToList();

            // Skip if already present locally (MergerFinMediaSourceProvider will inject it as an extra version!).
            if (keys.Any(k => localKeys.Contains(k)))
            {
                continue;
            }

            // Write only one .strm stub per canonical movie/episode (extra versions from other servers are injected by IMediaSourceProvider!).
            var canonicalKey = keys.FirstOrDefault() ?? src.SourceId;
            if (!handledCanonicalKeys.Add(canonicalKey))
            {
                continue;
            }

            string? strmPath = BuildStrmFilePath(root, src);
            if (string.IsNullOrWhiteSpace(strmPath))
            {
                continue;
            }

            var strmUrl = $"{localBase}/MergerFin/Stream/{src.SourceId}";
            try
            {
                var dir = Path.GetDirectoryName(strmPath);
                if (!string.IsNullOrEmpty(dir))
                {
                    Directory.CreateDirectory(dir);
                }

                if (!File.Exists(strmPath) || !string.Equals(File.ReadAllText(strmPath).Trim(), strmUrl, StringComparison.Ordinal))
                {
                    File.WriteAllText(strmPath, strmUrl + "\n", Encoding.UTF8);
                }

                writtenFiles.Add(Path.GetFullPath(strmPath));
            }
            catch (Exception ex)
            {
                _logger.LogWarning(ex, "MergerFin failed writing .strm file {Path}", strmPath);
            }
        }

        // Clean up stale .strm files exclusively inside StrmOutputPath.
        try
        {
            foreach (var existing in Directory.EnumerateFiles(root, "*.strm", SearchOption.AllDirectories))
            {
                if (!writtenFiles.Contains(Path.GetFullPath(existing)))
                {
                    File.Delete(existing);
                }
            }
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin stale .strm cleanup warning in {Path}", root);
        }
    }

    private HashSet<string> CollectLocalNonStrmKeys(string strmRoot)
    {
        var keys = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        var fullStrmRoot = Path.GetFullPath(strmRoot).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;

        var items = _libraryManager.GetItemList(new InternalItemsQuery
        {
            IncludeItemTypes = [BaseItemKind.Movie, BaseItemKind.Episode],
            Recursive = true
        });

        foreach (var item in items)
        {
            if (!string.IsNullOrWhiteSpace(item.Path))
            {
                var fullItemPath = Path.GetFullPath(item.Path);
                if (fullItemPath.StartsWith(fullStrmRoot, StringComparison.OrdinalIgnoreCase))
                {
                    continue;
                }
            }

            if (item is Movie)
            {
                foreach (var k in RemoteCatalogStore.BuildLookupKeys("Movie", item.ProviderIds))
                {
                    keys.Add(k);
                }
            }
            else if (item is Episode ep)
            {
                var seriesProviders = ep.Series?.ProviderIds;
                foreach (var k in RemoteCatalogStore.BuildLookupKeys("Episode", ep.ProviderIds, seriesProviders, ep.ParentIndexNumber, ep.IndexNumber))
                {
                    keys.Add(k);
                }
            }
        }

        return keys;
    }

    private static string? BuildStrmFilePath(string root, IndexedRemoteSource src)
    {
        if (string.Equals(src.ItemType, "Movie", StringComparison.OrdinalIgnoreCase))
        {
            var title = SanitizeFileName(src.Title);
            if (string.IsNullOrWhiteSpace(title))
            {
                return null;
            }

            var yearPart = src.ProductionYear is > 1800 ? $" ({src.ProductionYear.Value})" : string.Empty;
            var idTag = BuildProviderTag(src.ProviderIds);
            var folder = $"{title}{yearPart}{idTag}";
            return Path.Combine(root, "Movies", folder, $"{title}{yearPart}.strm");
        }

        if (string.Equals(src.ItemType, "Episode", StringComparison.OrdinalIgnoreCase))
        {
            var series = SanitizeFileName(src.SeriesName);
            if (string.IsNullOrWhiteSpace(series))
            {
                return null;
            }

            var seasonNum = src.SeasonNumber ?? 1;
            var epNum = src.EpisodeNumber ?? 1;
            var idTag = BuildProviderTag(src.SeriesProviderIds);
            var seriesFolder = $"{series}{idTag}";
            var seasonFolder = string.Format(CultureInfo.InvariantCulture, "Season {0:00}", seasonNum);
            var fileName = string.Format(CultureInfo.InvariantCulture, "{0} S{1:00}E{2:00}.strm", series, seasonNum, epNum);
            return Path.Combine(root, "Shows", seriesFolder, seasonFolder, fileName);
        }

        return null;
    }

    private static string BuildProviderTag(IDictionary<string, string> providerIds)
    {
        if (providerIds.TryGetValue("Tmdb", out var tmdb) && !string.IsNullOrWhiteSpace(tmdb))
        {
            return $" [tmdbid-{SanitizeFileName(tmdb)}]";
        }

        if (providerIds.TryGetValue("Tvdb", out var tvdb) && !string.IsNullOrWhiteSpace(tvdb))
        {
            return $" [tvdbid-{SanitizeFileName(tvdb)}]";
        }

        if (providerIds.TryGetValue("Imdb", out var imdb) && !string.IsNullOrWhiteSpace(imdb))
        {
            return $" [imdbid-{SanitizeFileName(imdb)}]";
        }

        return string.Empty;
    }

    private static string SanitizeFileName(string raw)
    {
        if (string.IsNullOrWhiteSpace(raw))
        {
            return string.Empty;
        }

        var invalid = Path.GetInvalidFileNameChars();
        var sb = new StringBuilder(raw.Length);
        foreach (var c in raw.Trim())
        {
            if (Array.IndexOf(invalid, c) >= 0 || c == '/' || c == '\\' || c == ':')
            {
                sb.Append(' ');
            }
            else
            {
                sb.Append(c);
            }
        }

        return sb.ToString().Trim().TrimEnd('.');
    }
}
