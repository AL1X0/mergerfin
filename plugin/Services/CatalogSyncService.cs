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
                .Where(s => s.Enabled && !string.IsNullOrWhiteSpace(s.BaseUrl) && !string.IsNullOrWhiteSpace(s.GetEffectiveApiKey()))
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
                await BackfillStrmRuntimesAsync(cfg.StrmOutputPath, ct).ConfigureAwait(false);
            }

            if (cfg.TagItemsWithServerName || cfg.ShowServerInTagline)
            {
                await SyncServerOriginTagsAsync(cfg, ct).ConfigureAwait(false);
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
        List<RemoteVirtualFolderDto> libraries = [];
        try
        {
            libraries = await _client.FetchVirtualFoldersAsync(srv, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin could not list /Library/VirtualFolders on {Server}; falling back to global sync", srv.Name);
        }

        if (libraries.Count == 0)
        {
            return await SyncScopeItemsAsync(srv, parentId: null, remoteLibraryName: string.Empty, explicitTargetFolder: string.Empty, includeSeries: true, seenMediaSourceIds: null, ct).ConfigureAwait(false);
        }

        var result = new List<IndexedRemoteSource>();
        var seenMediaSourceIds = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        foreach (var lib in libraries)
        {
            ct.ThrowIfCancellationRequested();
            var (targetFolder, ignored) = ResolveLibraryTarget(srv, lib);
            if (ignored)
            {
                _logger.LogInformation("MergerFin skipping ignored remote library '{Library}' on {Server}", lib.Name, srv.Name);
                continue;
            }

            var includeSeries = !string.Equals(lib.CollectionType, "movies", StringComparison.OrdinalIgnoreCase);
            try
            {
                var libSources = await SyncScopeItemsAsync(
                    srv,
                    parentId: lib.ItemId,
                    remoteLibraryName: lib.Name,
                    explicitTargetFolder: targetFolder,
                    includeSeries: includeSeries,
                    seenMediaSourceIds: seenMediaSourceIds,
                    ct).ConfigureAwait(false);
                result.AddRange(libSources);
            }
            catch (Exception ex)
            {
                _logger.LogWarning(ex, "MergerFin failed syncing remote library '{Library}' on {Server}", lib.Name, srv.Name);
            }
        }

        return result;
    }

    private async Task<List<IndexedRemoteSource>> SyncScopeItemsAsync(
        RemoteServerConfig srv,
        string? parentId,
        string remoteLibraryName,
        string explicitTargetFolder,
        bool includeSeries,
        HashSet<string>? seenMediaSourceIds,
        CancellationToken ct)
    {
        var result = new List<IndexedRemoteSource>();

        // 1. Fetch Series first to map SeriesId -> ProviderIds for episode deduplication.
        var seriesProvidersById = new Dictionary<string, Dictionary<string, string>>(StringComparer.OrdinalIgnoreCase);
        var seriesNameById = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        if (includeSeries)
        {
            var seriesList = await _client.FetchAllItemsAsync(srv, ["Series"], ct, parentId).ConfigureAwait(false);
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
        }

        // 2. Fetch Movies & Episodes with MediaSources + ProviderIds.
        var mediaItems = await _client.FetchAllItemsAsync(srv, ["Movie", "Episode"], ct, parentId).ConfigureAwait(false);
        foreach (var item in mediaItems)
        {
            if (item.MediaSources == null || item.MediaSources.Count == 0)
            {
                continue;
            }

            // Mesh anti-loop guard: never re-index items that are themselves MergerFin .strm stubs on the remote server!
            if (IsMergerFinStrmItem(item))
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

            var resolvedTargetFolder = !string.IsNullOrWhiteSpace(explicitTargetFolder)
                ? explicitTargetFolder
                : (string.Equals(item.Type, "Movie", StringComparison.OrdinalIgnoreCase) ? "Movies" : "Shows");

            foreach (var ms in item.MediaSources)
            {
                if (string.IsNullOrWhiteSpace(ms.Id) || IsRelayedMediaSource(ms))
                {
                    continue;
                }

                if (seenMediaSourceIds != null && !seenMediaSourceIds.Add(ms.Id))
                {
                    continue;
                }

                var streams = (ms.MediaStreams ?? []).Select(st => new IndexedMediaStream
                {
                    Type = st.Type,
                    Index = st.Index,
                    Codec = st.Codec ?? string.Empty,
                    CodecTag = st.CodecTag,
                    Language = st.Language ?? string.Empty,
                    Title = st.Title ?? string.Empty,
                    DisplayTitle = st.DisplayTitle ?? string.Empty,
                    Profile = st.Profile,
                    Level = st.Level,
                    BitDepth = st.BitDepth,
                    PixelFormat = st.PixelFormat,
                    ChannelLayout = st.ChannelLayout,
                    AverageFrameRate = st.AverageFrameRate,
                    RealFrameRate = st.RealFrameRate,
                    AspectRatio = st.AspectRatio,
                    ColorRange = st.ColorRange,
                    ColorSpace = st.ColorSpace,
                    ColorTransfer = st.ColorTransfer,
                    ColorPrimaries = st.ColorPrimaries,
                    DvVersionMajor = st.DvVersionMajor,
                    DvVersionMinor = st.DvVersionMinor,
                    DvProfile = st.DvProfile,
                    DvLevel = st.DvLevel,
                    RpuPresentFlag = st.RpuPresentFlag,
                    ElPresentFlag = st.ElPresentFlag,
                    BlPresentFlag = st.BlPresentFlag,
                    DvBlSignalCompatibilityId = st.DvBlSignalCompatibilityId,
                    Hdr10PlusPresentFlag = st.Hdr10PlusPresentFlag,
                    IsInterlaced = st.IsInterlaced,
                    IsAnamorphic = st.IsAnamorphic,
                    IsAVC = st.IsAVC,
                    RefFrames = st.RefFrames,
                    IsDefault = st.IsDefault,
                    IsForced = st.IsForced,
                    IsHearingImpaired = st.IsHearingImpaired,
                    IsOriginal = st.IsOriginal,
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
                    TargetFolder = resolvedTargetFolder,
                    RemoteLibraryName = remoteLibraryName,
                    Streams = streams
                });
            }
        }

        return result;
    }

    /// <summary>
    /// Automatically determines the default output subfolder ("Anime", "Movies", "Shows", or "" for mixed per-item routing)
    /// from a remote Jellyfin library's name and collection type.
    /// </summary>
    public static string DetectDefaultTargetFolder(string? libraryName, string? collectionType)
    {
        var normalized = NormalizeForMatch(libraryName);
        if (!string.IsNullOrEmpty(normalized) &&
            (normalized.Contains("anime", StringComparison.Ordinal) ||
             normalized.Contains("japanim", StringComparison.Ordinal)))
        {
            return "Anime";
        }

        if (string.Equals(collectionType, "movies", StringComparison.OrdinalIgnoreCase))
        {
            return "Movies";
        }

        if (string.Equals(collectionType, "tvshows", StringComparison.OrdinalIgnoreCase))
        {
            return "Shows";
        }

        return string.Empty;
    }

    private static (string TargetFolder, bool Ignored) ResolveLibraryTarget(RemoteServerConfig srv, RemoteVirtualFolderDto lib)
    {
        var mappings = srv.LibraryMappings;
        if (mappings != null && mappings.Count > 0)
        {
            var match = mappings.FirstOrDefault(m =>
                !string.IsNullOrWhiteSpace(m.RemoteLibraryId) &&
                string.Equals(m.RemoteLibraryId.Trim(), lib.ItemId, StringComparison.OrdinalIgnoreCase));

            match ??= mappings.FirstOrDefault(m =>
                !string.IsNullOrWhiteSpace(m.RemoteLibraryName) &&
                string.Equals(m.RemoteLibraryName.Trim(), lib.Name, StringComparison.OrdinalIgnoreCase));

            if (match != null)
            {
                var target = (match.TargetFolder ?? string.Empty).Trim();
                if (string.Equals(target, "ignore", StringComparison.OrdinalIgnoreCase))
                {
                    return (string.Empty, true);
                }

                if (!string.IsNullOrWhiteSpace(target) &&
                    !string.Equals(target, "auto", StringComparison.OrdinalIgnoreCase))
                {
                    return (SanitizeFolderSegment(target), false);
                }
            }
        }

        return (DetectDefaultTargetFolder(lib.Name, lib.CollectionType), false);
    }

    private static string NormalizeForMatch(string? raw)
    {
        if (string.IsNullOrWhiteSpace(raw))
        {
            return string.Empty;
        }

        var decomposed = raw.Trim().Normalize(NormalizationForm.FormD);
        var sb = new StringBuilder(decomposed.Length);
        foreach (var c in decomposed)
        {
            if (CharUnicodeInfo.GetUnicodeCategory(c) != UnicodeCategory.NonSpacingMark)
            {
                sb.Append(char.ToLowerInvariant(c));
            }
        }

        return sb.ToString();
    }

    private static string SanitizeFolderSegment(string? raw)
    {
        if (string.IsNullOrWhiteSpace(raw))
        {
            return string.Empty;
        }

        var cleaned = SanitizeFileName(raw).Replace("..", string.Empty, StringComparison.Ordinal).Trim();
        return cleaned;
    }

    private static bool IsMergerFinStrmItem(RemoteItemDto item)
    {
        var path = item.Path ?? string.Empty;
        if (path.EndsWith(".strm", StringComparison.OrdinalIgnoreCase))
        {
            if (path.Contains("mergerfin", StringComparison.OrdinalIgnoreCase))
            {
                return true;
            }

            // If all media sources of a .strm item are HTTP/MergerFin relays, never re-index it.
            if (item.MediaSources != null && item.MediaSources.All(IsRelayedMediaSource))
            {
                return true;
            }
        }

        return false;
    }

    private static bool IsRelayedMediaSource(RemoteMediaSourceDto ms)
    {
        if (ms.Id.StartsWith("mf_", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        var path = ms.Path ?? string.Empty;
        if (path.Contains("/MergerFin/Stream/", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        if (ms.IsRemote && string.Equals(ms.Protocol, "Http", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        return false;
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
        var localKeys = cfg.SkipLocalDuplicatesInStrm ? CollectLocalNonStrmKeys(root) : new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        var writtenFiles = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        var handledCanonicalKeys = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        // If any server places a canonical item in a specialized library (e.g. "Anime" or custom folder)
        // while another server leaves it in default "Shows"/"Movies", prefer the specialized folder.
        var specializedFolderByKey = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        foreach (var src in allSources.OrderBy(s => s.ServerPriority))
        {
            var folder = SanitizeFolderSegment(src.TargetFolder);
            if (string.IsNullOrWhiteSpace(folder) ||
                string.Equals(folder, "Movies", StringComparison.OrdinalIgnoreCase) ||
                string.Equals(folder, "Shows", StringComparison.OrdinalIgnoreCase))
            {
                continue;
            }

            foreach (var k in RemoteCatalogStore.BuildLookupKeys(
                src.ItemType, src.ProviderIds, src.SeriesProviderIds, src.SeasonNumber, src.EpisodeNumber))
            {
                specializedFolderByKey.TryAdd(k, folder);
            }
        }

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

            string? preferredFolder = null;
            foreach (var k in keys)
            {
                if (specializedFolderByKey.TryGetValue(k, out var sf))
                {
                    preferredFolder = sf;
                    break;
                }
            }

            string? strmPath = BuildStrmFilePath(root, src, preferredFolder);
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

                var utf8NoBom = new UTF8Encoding(encoderShouldEmitUTF8Identifier: false);
                if (!File.Exists(strmPath) || !string.Equals(File.ReadAllText(strmPath).Trim().TrimStart('\uFEFF'), strmUrl, StringComparison.Ordinal))
                {
                    File.WriteAllText(strmPath, strmUrl + "\n", utf8NoBom);
                }

                writtenFiles.Add(Path.GetFullPath(strmPath));
            }
            catch (Exception ex)
            {
                _logger.LogWarning(ex, "MergerFin failed writing .strm file {Path}", strmPath);
            }
        }

        // Clean up stale .strm files and empty item subfolders exclusively inside StrmOutputPath
        // (top-level category folders like Movies/, Shows/, Anime/ are preserved).
        try
        {
            foreach (var existing in Directory.EnumerateFiles(root, "*.strm", SearchOption.AllDirectories))
            {
                if (!writtenFiles.Contains(Path.GetFullPath(existing)))
                {
                    File.Delete(existing);
                }
            }

            CleanupEmptyNestedDirectories(root);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin stale .strm cleanup warning in {Path}", root);
        }
    }

    private static void CleanupEmptyNestedDirectories(string root)
    {
        foreach (var topLevelDir in Directory.EnumerateDirectories(root))
        {
            RemoveEmptySubdirectoriesRecursive(topLevelDir, deleteSelf: false);
        }
    }

    private static void RemoveEmptySubdirectoriesRecursive(string dirPath, bool deleteSelf)
    {
        foreach (var sub in Directory.EnumerateDirectories(dirPath))
        {
            RemoveEmptySubdirectoriesRecursive(sub, deleteSelf: true);
        }

        if (deleteSelf && !Directory.EnumerateFileSystemEntries(dirPath).Any())
        {
            try
            {
                Directory.Delete(dirPath, recursive: false);
            }
            catch
            {
                // Ignore transient filesystem locks during cleanup.
            }
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

    private async Task BackfillStrmRuntimesAsync(string strmRoot, CancellationToken ct)
    {
        var fullStrmRoot = Path.GetFullPath(strmRoot).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;
        var items = _libraryManager.GetItemList(new InternalItemsQuery
        {
            IncludeItemTypes = [BaseItemKind.Movie, BaseItemKind.Episode],
            Recursive = true
        });

        var updated = 0;
        foreach (var item in items)
        {
            ct.ThrowIfCancellationRequested();
            if (item.RunTimeTicks is > 0 || string.IsNullOrWhiteSpace(item.Path))
            {
                continue;
            }

            if (!Path.GetFullPath(item.Path).StartsWith(fullStrmRoot, StringComparison.OrdinalIgnoreCase))
            {
                continue;
            }

            List<IndexedRemoteSource> matches;
            if (item is Movie)
            {
                matches = _store.FindMatches("Movie", item.ProviderIds);
            }
            else if (item is Episode ep)
            {
                matches = _store.FindMatches("Episode", ep.ProviderIds, ep.Series?.ProviderIds, ep.ParentIndexNumber, ep.IndexNumber);
            }
            else
            {
                continue;
            }

            var runtime = matches.FirstOrDefault(m => m.RunTimeTicks > 0)?.RunTimeTicks ?? 0;
            if (runtime <= 0)
            {
                continue;
            }

            item.RunTimeTicks = runtime;
            try
            {
                await _libraryManager.UpdateItemAsync(item, item.GetParent(), ItemUpdateType.MetadataEdit, ct).ConfigureAwait(false);
                updated++;
            }
            catch (Exception ex)
            {
                _logger.LogDebug(ex, "MergerFin failed updating RunTimeTicks for {Name}", item.Name);
            }
        }

        if (updated > 0)
        {
            _logger.LogInformation("MergerFin backfilled RunTimeTicks on {Count} remote .strm items", updated);
        }
    }

    private async Task SyncServerOriginTagsAsync(PluginConfiguration cfg, CancellationToken ct)
    {
        var prefix = cfg.ServerTagPrefix ?? string.Empty;
        var localTagName = (cfg.LocalServerTag ?? string.Empty).Trim();
        var fullStrmRoot = !string.IsNullOrWhiteSpace(cfg.StrmOutputPath)
            ? Path.GetFullPath(cfg.StrmOutputPath.Trim()).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar
            : string.Empty;

        // Collect all known MergerFin-managed server names so renamed/removed sources are cleaned up safely
        // without touching any unrelated user or metadata tags.
        var knownManagedRawNames = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        foreach (var s in cfg.Servers)
        {
            if (!string.IsNullOrWhiteSpace(s.Name))
            {
                knownManagedRawNames.Add(s.Name.Trim());
            }
        }

        if (!string.IsNullOrWhiteSpace(localTagName))
        {
            knownManagedRawNames.Add(localTagName);
        }

        // Single pass over episodes to detect which series have real local files (and .strm fallback server names).
        var seriesWithLocalFiles = new HashSet<Guid>();
        var seriesStrmFallbackServers = new Dictionary<Guid, HashSet<string>>();
        var episodes = _libraryManager.GetItemList(new InternalItemsQuery
        {
            IncludeItemTypes = [BaseItemKind.Episode],
            Recursive = true
        });

        foreach (var item in episodes)
        {
            if (item is not Episode ep)
            {
                continue;
            }

            var sid = ep.Series?.Id ?? Guid.Empty;
            if (sid == Guid.Empty)
            {
                continue;
            }

            if (!IsPathInsideStrmRoot(ep.Path, fullStrmRoot))
            {
                seriesWithLocalFiles.Add(sid);
            }
            else
            {
                var strmServer = ResolveServerNameFromStrmPath(_store, ep.Path);
                if (!string.IsNullOrWhiteSpace(strmServer))
                {
                    if (!seriesStrmFallbackServers.TryGetValue(sid, out var set))
                    {
                        set = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
                        seriesStrmFallbackServers[sid] = set;
                    }

                    set.Add(strmServer);
                }
            }
        }

        var items = _libraryManager.GetItemList(new InternalItemsQuery
        {
            IncludeItemTypes = [BaseItemKind.Movie, BaseItemKind.Series],
            Recursive = true
        });

        var originCache = new Dictionary<Guid, (bool HasLocalFile, List<string> RemoteServers)>(items.Count);
        var updated = 0;
        var matchedCount = 0;
        var failedCount = 0;
        foreach (var item in items)
        {
            ct.ThrowIfCancellationRequested();

            List<string> remoteServers;
            bool hasLocalFile;

            if (item is Movie)
            {
                hasLocalFile = !string.IsNullOrWhiteSpace(item.Path) && !IsPathInsideStrmRoot(item.Path, fullStrmRoot);
                remoteServers = _store.FindServerNamesForMovie(item.ProviderIds);
                if (remoteServers.Count == 0 && IsPathInsideStrmRoot(item.Path, fullStrmRoot))
                {
                    var fallback = ResolveServerNameFromStrmPath(_store, item.Path);
                    if (!string.IsNullOrWhiteSpace(fallback))
                    {
                        remoteServers = [fallback];
                    }
                }
            }
            else if (item is Series)
            {
                hasLocalFile = seriesWithLocalFiles.Contains(item.Id);
                remoteServers = _store.FindServerNamesForSeries(item.ProviderIds);
                if (remoteServers.Count == 0 && seriesStrmFallbackServers.TryGetValue(item.Id, out var fallbackSet))
                {
                    remoteServers = fallbackSet.ToList();
                }
            }
            else
            {
                continue;
            }

            originCache[item.Id] = (hasLocalFile, remoteServers);
            if (remoteServers.Count > 0)
            {
                matchedCount++;
            }

            if (!cfg.TagItemsWithServerName)
            {
                continue;
            }

            var desiredManagedTags = new List<string>();
            var desiredSeen = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

            if (hasLocalFile && !string.IsNullOrWhiteSpace(localTagName))
            {
                var formattedLocal = FormatServerTag(prefix, localTagName);
                if (desiredSeen.Add(formattedLocal))
                {
                    desiredManagedTags.Add(formattedLocal);
                }
            }

            foreach (var srvName in remoteServers)
            {
                var formatted = FormatServerTag(prefix, srvName);
                if (desiredSeen.Add(formatted))
                {
                    desiredManagedTags.Add(formatted);
                }
            }

            var existingTags = item.Tags ?? [];
            var nextTags = new List<string>(existingTags.Length + desiredManagedTags.Count);
            var nextSeen = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

            foreach (var tag in existingTags)
            {
                if (string.IsNullOrWhiteSpace(tag))
                {
                    continue;
                }

                if (IsMergerFinManagedTag(tag, prefix, knownManagedRawNames))
                {
                    continue;
                }

                if (nextSeen.Add(tag))
                {
                    nextTags.Add(tag);
                }
            }

            foreach (var desired in desiredManagedTags)
            {
                if (nextSeen.Add(desired))
                {
                    nextTags.Add(desired);
                }
            }

            if (TagsAreEqual(existingTags, nextTags))
            {
                continue;
            }

            item.Tags = nextTags.ToArray();
            try
            {
                await _libraryManager.UpdateItemAsync(item, item.GetParent(), ItemUpdateType.MetadataEdit, ct).ConfigureAwait(false);
                updated++;
            }
            catch (Exception ex)
            {
                failedCount++;
                if (failedCount <= 5)
                {
                    _logger.LogWarning(ex, "MergerFin failed updating server origin tags for {Name}", item.Name);
                }
            }
        }

        _store.ReplaceItemOriginCache(originCache);
        _logger.LogInformation(
            "MergerFin server-origin sync complete: scanned {Total} movies/series, matched {Matched} remote items, updated tags on {Updated} (errors: {Errors})",
            items.Count,
            matchedCount,
            updated,
            failedCount);
    }

    internal static bool IsPathInsideStrmRoot(string? itemPath, string fullStrmRoot)
    {
        if (string.IsNullOrWhiteSpace(itemPath))
        {
            return false;
        }

        if (!string.IsNullOrEmpty(fullStrmRoot))
        {
            try
            {
                if (Path.GetFullPath(itemPath).StartsWith(fullStrmRoot, StringComparison.OrdinalIgnoreCase))
                {
                    return true;
                }
            }
            catch
            {
                // Fall back to extension check below.
            }
        }

        return itemPath.EndsWith(".strm", StringComparison.OrdinalIgnoreCase) &&
               itemPath.Contains("mergerfin", StringComparison.OrdinalIgnoreCase);
    }

    internal static string? ResolveServerNameFromStrmPath(RemoteCatalogStore store, string? strmPath)
    {
        var sourceId = StrmSourceResolver.TryGetSourceId(strmPath);
        return sourceId != null ? store.GetBySourceId(sourceId)?.ServerName : null;
    }

    private static string FormatServerTag(string prefix, string rawName)
    {
        return string.IsNullOrEmpty(prefix) ? rawName.Trim() : prefix + rawName.Trim();
    }

    private static bool IsMergerFinManagedTag(string tag, string prefix, HashSet<string> knownManagedRawNames)
    {
        var trimmed = tag.Trim();
        if (!string.IsNullOrEmpty(prefix) && trimmed.StartsWith(prefix, StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        if (knownManagedRawNames.Contains(trimmed))
        {
            return true;
        }

        return false;
    }

    private static bool TagsAreEqual(string[] existing, List<string> next)
    {
        if (existing.Length != next.Count)
        {
            return false;
        }

        for (var i = 0; i < existing.Length; i++)
        {
            if (!string.Equals(existing[i], next[i], StringComparison.Ordinal))
            {
                return false;
            }
        }

        return true;
    }

    private static string? BuildStrmFilePath(string root, IndexedRemoteSource src, string? preferredFolder = null)
    {
        var baseFolder = SanitizeFolderSegment(preferredFolder);
        if (string.IsNullOrWhiteSpace(baseFolder))
        {
            baseFolder = SanitizeFolderSegment(src.TargetFolder);
        }

        if (string.Equals(src.ItemType, "Movie", StringComparison.OrdinalIgnoreCase))
        {
            var title = SanitizeFileName(src.Title);
            if (string.IsNullOrWhiteSpace(title))
            {
                return null;
            }

            var category = string.IsNullOrWhiteSpace(baseFolder) ? "Movies" : baseFolder;
            var yearPart = src.ProductionYear is > 1800 ? $" ({src.ProductionYear.Value})" : string.Empty;
            var idTag = BuildProviderTag(src.ProviderIds);
            var folder = $"{title}{yearPart}{idTag}";
            return Path.Combine(root, category, folder, $"{title}{yearPart}.strm");
        }

        if (string.Equals(src.ItemType, "Episode", StringComparison.OrdinalIgnoreCase))
        {
            var series = SanitizeFileName(src.SeriesName);
            if (string.IsNullOrWhiteSpace(series))
            {
                return null;
            }

            var category = string.IsNullOrWhiteSpace(baseFolder) ? "Shows" : baseFolder;
            var seasonNum = src.SeasonNumber ?? 1;
            var epNum = src.EpisodeNumber ?? 1;
            var idTag = BuildProviderTag(src.SeriesProviderIds);
            var seriesFolder = $"{series}{idTag}";
            var seasonFolder = string.Format(CultureInfo.InvariantCulture, "Season {0:00}", seasonNum);
            var fileName = string.Format(CultureInfo.InvariantCulture, "{0} S{1:00}E{2:00}.strm", series, seasonNum, epNum);
            return Path.Combine(root, category, seriesFolder, seasonFolder, fileName);
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
