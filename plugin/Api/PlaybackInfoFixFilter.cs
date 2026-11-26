using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Reflection;
using System.Threading.Tasks;
using Jellyfin.Data.Enums;
using Jellyfin.Plugin.MergerFin.Models;
using Jellyfin.Plugin.MergerFin.Providers;
using Jellyfin.Plugin.MergerFin.Services;
using MediaBrowser.Controller.Entities.Movies;
using MediaBrowser.Controller.Entities.TV;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Dto;
using MediaBrowser.Model.Entities;
using MediaBrowser.Model.MediaInfo;
using MediaBrowser.Model.Querying;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;
using Microsoft.AspNetCore.Mvc.Filters;

namespace Jellyfin.Plugin.MergerFin.Api;

/// <summary>
/// Fixes Jellyfin's PlaybackInfo and item DTO responses for MergerFin HTTP/.strm and mf_* sources:
/// 1. Replaces the 75-byte .strm stub Size and missing Bitrate/RunTimeTicks/Container/MediaStreams
///    with the real remote metadata from RemoteCatalogStore.
/// 2. Undoes Jellyfin's ForceRemoteSourceTranscoding override on MergerFin sources:
///    - When StreamBuilder approved DirectPlay (MediaInfoHelper placed a Static=true URL inside
///      TranscodingUrl and forced SupportsDirectPlay=false), restores SupportsDirectPlay=true +
///      SupportsDirectStream=true + TranscodingUrl=null so BOTH jellyfin-web (no 2x seek jump)
///      AND third-party clients (OptiFin, Wholphin, Jellyfin Android TV, Infuse, Swiftfin, Findroid)
///      Direct Play cleanly via /Videos/{id}/stream?Static=true.
///    - When StreamBuilder genuinely chose HLS/remux/transcoding (no Static=true), strips the forced
///      allowVideoStreamCopy=false / allowAudioStreamCopy=false flags unless the client explicitly
///      requested them, preserving stream-copy remuxing.
/// 3. Presents MergerFin sources in client response DTOs with Protocol=File, IsRemote=false, and a
///    clean non-loopback virtual Path so third-party clients never drop them (e.g. Android TV's
///    protocol==FILE &amp;&amp; !isRemote filter) or attempt to connect directly to http://127.0.0.1:8096.
///    Jellyfin's internal BaseItem/MediaSourceManager keeps Protocol=Http + loopback /MergerFin/Stream/
///    so /Videos/{id}/stream?Static=true proxies through VideosController.GetStaticRemoteStreamResult.
/// </summary>
public class PlaybackInfoFixFilter : IAsyncActionFilter, IAsyncResultFilter
{
    private const string ClientDisallowedVideoCopyKey = "MergerFin.ClientDisallowedVideoCopy";
    private const string ClientDisallowedAudioCopyKey = "MergerFin.ClientDisallowedAudioCopy";

    private static readonly ConcurrentDictionary<Type, (PropertyInfo? VideoCopy, PropertyInfo? AudioCopy)> DtoPropCache = new();

    private readonly RemoteCatalogStore _store;
    private readonly ILibraryManager _libraryManager;
    private readonly RemoteSubtitleProxy _subtitleProxy;

    public PlaybackInfoFixFilter(RemoteCatalogStore store, ILibraryManager libraryManager, RemoteSubtitleProxy subtitleProxy)
    {
        _store = store;
        _libraryManager = libraryManager;
        _subtitleProxy = subtitleProxy;
    }

    public async Task OnActionExecutionAsync(ActionExecutingContext context, ActionExecutionDelegate next)
    {
        // Subtitles of remote sources are served by the remote server that owns the file (exact timeline,
        // correct track, no full-file download through local ffmpeg). Falls through on any failure.
        var subtitleResult = await _subtitleProxy
            .TryProxyAsync(context.HttpContext.Request, context.HttpContext.RequestAborted)
            .ConfigureAwait(false);
        if (subtitleResult != null)
        {
            context.Result = subtitleResult;
            return;
        }

        var path = context.HttpContext.Request.Path.Value;
        if (!string.IsNullOrEmpty(path) &&
            path.EndsWith("/PlaybackInfo", StringComparison.OrdinalIgnoreCase))
        {
            context.HttpContext.Items[ClientDisallowedVideoCopyKey] =
                IsExplicitFalse(context, "allowVideoStreamCopy", "AllowVideoStreamCopy");
            context.HttpContext.Items[ClientDisallowedAudioCopyKey] =
                IsExplicitFalse(context, "allowAudioStreamCopy", "AllowAudioStreamCopy");
        }

        await next().ConfigureAwait(false);
    }

    public Task OnResultExecutionAsync(ResultExecutingContext context, ResultExecutionDelegate next)
    {
        if (context.Result is ObjectResult { Value: { } value })
        {
            if (value is PlaybackInfoResponse playbackInfo && playbackInfo.MediaSources != null)
            {
                var clientDisallowedVideoCopy =
                    context.HttpContext.Items.TryGetValue(ClientDisallowedVideoCopyKey, out var dv) && dv is true;
                var clientDisallowedAudioCopy =
                    context.HttpContext.Items.TryGetValue(ClientDisallowedAudioCopyKey, out var da) && da is true;

                foreach (var ms in playbackInfo.MediaSources)
                {
                    FixMediaSourceForPlayback(ms, clientDisallowedVideoCopy, clientDisallowedAudioCopy);
                }
            }
            else if (value is LiveStreamResponse { MediaSource: { } liveMs })
            {
                FixMediaSourceForPlayback(liveMs, false, false);
            }
            else if (!IsMergerFinCaller(context.HttpContext.Request))
            {
                if (value is BaseItemDto itemDto)
                {
                    FixItemDto(itemDto, isDetailResponse: true);
                }
                else if (value is QueryResult<BaseItemDto> queryResult && queryResult.Items != null)
                {
                    var isSingle = queryResult.Items.Count == 1;
                    foreach (var item in queryResult.Items)
                    {
                        FixItemDto(item, isDetailResponse: isSingle);
                    }
                }
                else if (value is IReadOnlyList<BaseItemDto> itemList)
                {
                    var isSingle = itemList.Count == 1;
                    for (var i = 0; i < itemList.Count; i++)
                    {
                        FixItemDto(itemList[i], isDetailResponse: isSingle);
                    }
                }
            }
        }

        return next();
    }

    private void FixMediaSourceForPlayback(MediaSourceInfo ms, bool clientDisallowedVideoCopy, bool clientDisallowedAudioCopy)
    {
        var sourceId = ExtractSourceId(ms);
        if (string.IsNullOrEmpty(sourceId))
        {
            return;
        }

        var wasRemote = ms.IsRemote;
        var remote = _store.GetBySourceId(sourceId);
        var container = BackfillMetadata(ms, remote, sourceId);

        if (!string.IsNullOrEmpty(ms.TranscodingUrl))
        {
            if (ms.TranscodingUrl.Contains("Static=true", StringComparison.OrdinalIgnoreCase))
            {
                ms.SupportsDirectPlay = true;
                ms.SupportsDirectStream = true;
                ms.TranscodingUrl = null;
                ms.TranscodingContainer = null;
                ms.TranscodingSubProtocol = MediaStreamProtocol.http;
                ms.TranscodeReasons = 0;
            }
            else if (wasRemote)
            {
                if (!clientDisallowedVideoCopy)
                {
                    ms.TranscodingUrl = RemoveQueryFlag(ms.TranscodingUrl, "allowVideoStreamCopy=false");
                }

                if (!clientDisallowedAudioCopy)
                {
                    ms.TranscodingUrl = RemoveQueryFlag(ms.TranscodingUrl, "allowAudioStreamCopy=false");
                }
            }
        }

        SanitizeClientTransportFields(ms, sourceId, container);
    }

    private void FixItemDto(BaseItemDto itemDto, bool isDetailResponse)
    {
        ApplyServerOriginPresentation(itemDto, isDetailResponse);

        if (itemDto.MediaSources == null || itemDto.MediaSources.Length == 0)
        {
            return;
        }

        MediaSourceInfo? primaryFixed = null;
        foreach (var ms in itemDto.MediaSources)
        {
            var sourceId = ExtractSourceId(ms);
            if (string.IsNullOrEmpty(sourceId))
            {
                continue;
            }

            var remote = _store.GetBySourceId(sourceId);
            var container = BackfillMetadata(ms, remote, sourceId);
            SanitizeClientTransportFields(ms, sourceId, container);
            primaryFixed ??= ms;
        }

        if (primaryFixed == null)
        {
            return;
        }

        if (string.IsNullOrWhiteSpace(itemDto.Container) ||
            string.Equals(itemDto.Container, "strm", StringComparison.OrdinalIgnoreCase))
        {
            itemDto.Container = primaryFixed.Container;
        }

        if ((itemDto.RunTimeTicks == null || itemDto.RunTimeTicks <= 0) && primaryFixed.RunTimeTicks is > 0)
        {
            itemDto.RunTimeTicks = primaryFixed.RunTimeTicks;
        }

        if ((itemDto.MediaStreams == null || itemDto.MediaStreams.Length == 0) &&
            primaryFixed.MediaStreams is { Count: > 0 })
        {
            itemDto.MediaStreams = primaryFixed.MediaStreams.ToArray();
        }

        var videoStream = primaryFixed.MediaStreams?.FirstOrDefault(s => s.Type == MediaStreamType.Video);
        if (videoStream != null)
        {
            if ((itemDto.Width == null || itemDto.Width <= 0) && videoStream.Width is > 0)
            {
                itemDto.Width = videoStream.Width;
            }

            if ((itemDto.Height == null || itemDto.Height <= 0) && videoStream.Height is > 0)
            {
                itemDto.Height = videoStream.Height;
            }

            if (itemDto.IsHD != true && videoStream.Height is >= 720)
            {
                itemDto.IsHD = true;
            }
        }
    }

    private void ApplyServerOriginPresentation(BaseItemDto itemDto, bool isDetailResponse)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || (!cfg.TagItemsWithServerName && !cfg.ShowServerInTagline))
        {
            return;
        }

        if (itemDto.Type != BaseItemKind.Movie && itemDto.Type != BaseItemKind.Series)
        {
            return;
        }

        var fullStrmRoot = !string.IsNullOrWhiteSpace(cfg.StrmOutputPath)
            ? Path.GetFullPath(cfg.StrmOutputPath.Trim()).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar
            : string.Empty;

        var hasLocalFile = false;
        List<string> remoteServers;

        if (_store.TryGetCachedItemOrigin(itemDto.Id, out var cached))
        {
            hasLocalFile = cached.HasLocalFile;
            remoteServers = cached.RemoteServers;
        }
        else
        {
            if (itemDto.Type == BaseItemKind.Movie)
            {
                hasLocalFile = !string.IsNullOrWhiteSpace(itemDto.Path) && !CatalogSyncService.IsPathInsideStrmRoot(itemDto.Path, fullStrmRoot);
                remoteServers = _store.FindServerNamesForMovie(itemDto.ProviderIds);
            }
            else
            {
                remoteServers = _store.FindServerNamesForSeries(itemDto.ProviderIds);
            }

            if (remoteServers.Count == 0 && isDetailResponse && itemDto.Id != Guid.Empty)
            {
                var dbItem = _libraryManager.GetItemById(itemDto.Id);
                if (dbItem is Movie m)
                {
                    hasLocalFile = !string.IsNullOrWhiteSpace(m.Path) && !CatalogSyncService.IsPathInsideStrmRoot(m.Path, fullStrmRoot);
                    remoteServers = _store.FindServerNamesForMovie(m.ProviderIds);
                    if (remoteServers.Count == 0 && CatalogSyncService.IsPathInsideStrmRoot(m.Path, fullStrmRoot))
                    {
                        var fallback = CatalogSyncService.ResolveServerNameFromStrmPath(_store, m.Path);
                        if (!string.IsNullOrWhiteSpace(fallback))
                        {
                            remoteServers = [fallback];
                        }
                    }
                }
                else if (dbItem is Series s)
                {
                    remoteServers = _store.FindServerNamesForSeries(s.ProviderIds);
                }
            }
        }

        var localTagName = (cfg.LocalServerTag ?? string.Empty).Trim();
        var rawNames = new List<string>();
        var rawSeen = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

        if (hasLocalFile && !string.IsNullOrWhiteSpace(localTagName) && rawSeen.Add(localTagName))
        {
            rawNames.Add(localTagName);
        }

        foreach (var srvName in remoteServers)
        {
            var trimmed = (srvName ?? string.Empty).Trim();
            if (!string.IsNullOrEmpty(trimmed) && rawSeen.Add(trimmed))
            {
                rawNames.Add(trimmed);
            }
        }

        if (rawNames.Count == 0)
        {
            return;
        }

        var prefix = cfg.ServerTagPrefix ?? string.Empty;

        if (cfg.TagItemsWithServerName)
        {
            var existingTags = itemDto.Tags ?? [];
            var merged = new List<string>(existingTags.Length + rawNames.Count);
            var tagSeen = new HashSet<string>(StringComparer.OrdinalIgnoreCase);

            foreach (var t in existingTags)
            {
                if (!string.IsNullOrWhiteSpace(t) && tagSeen.Add(t))
                {
                    merged.Add(t);
                }
            }

            var addedAny = false;
            foreach (var name in rawNames)
            {
                var formatted = string.IsNullOrEmpty(prefix) ? name : prefix + name;
                if (tagSeen.Add(formatted))
                {
                    merged.Add(formatted);
                    addedAny = true;
                }
            }

            if (addedAny)
            {
                itemDto.Tags = merged.ToArray();
            }
        }

        if (cfg.ShowServerInTagline && isDetailResponse)
        {
            var joined = string.Join(" \u2022 ", rawNames);
            var banner = string.IsNullOrEmpty(prefix) ? joined : prefix + joined;

            if (itemDto.Taglines == null || itemDto.Taglines.Length == 0)
            {
                itemDto.Taglines = [banner];
            }
            else if (!itemDto.Taglines[0].Contains(banner, StringComparison.OrdinalIgnoreCase))
            {
                var updated = new string[itemDto.Taglines.Length];
                Array.Copy(itemDto.Taglines, updated, itemDto.Taglines.Length);
                updated[0] = $"[{banner}] {itemDto.Taglines[0]}";
                itemDto.Taglines = updated;
            }
        }
    }

    private static string BackfillMetadata(MediaSourceInfo ms, IndexedRemoteSource? remote, string sourceId)
    {
        if (remote != null)
        {
            if (!string.IsNullOrWhiteSpace(remote.Label) &&
                (string.IsNullOrWhiteSpace(ms.Name) || !ms.Name.StartsWith("[", StringComparison.Ordinal)))
            {
                ms.Name = remote.Label;
            }

            if ((ms.Size == null || ms.Size < 4096) && remote.Size > 0)
            {
                ms.Size = remote.Size;
            }

            if ((ms.Bitrate == null || ms.Bitrate <= 0) && remote.Bitrate > 0)
            {
                ms.Bitrate = remote.Bitrate;
            }

            if ((ms.RunTimeTicks == null || ms.RunTimeTicks <= 0) && remote.RunTimeTicks > 0)
            {
                ms.RunTimeTicks = remote.RunTimeTicks;
            }

            if ((ms.MediaStreams == null || ms.MediaStreams.Count == 0) && remote.Streams.Count > 0)
            {
                var streams = MergerFinMediaSourceProvider.BuildMediaStreams(remote);
                ms.MediaStreams = streams;

                if (ms.DefaultAudioStreamIndex == null)
                {
                    var defAudio = streams.FirstOrDefault(s => s.Type == MediaStreamType.Audio && s.IsDefault)
                        ?? streams.FirstOrDefault(s => s.Type == MediaStreamType.Audio);
                    if (defAudio != null)
                    {
                        ms.DefaultAudioStreamIndex = defAudio.Index;
                    }
                }

                if (ms.DefaultSubtitleStreamIndex == null)
                {
                    var defSub = streams.FirstOrDefault(s => s.Type == MediaStreamType.Subtitle && (s.IsDefault || s.IsForced));
                    if (defSub != null)
                    {
                        ms.DefaultSubtitleStreamIndex = defSub.Index;
                    }
                }
            }
        }

        var container = !string.IsNullOrWhiteSpace(ms.Container) &&
                        !string.Equals(ms.Container, "strm", StringComparison.OrdinalIgnoreCase)
            ? MergerFinMediaSourceProvider.NormalizeContainer(ms.Container)
            : MergerFinMediaSourceProvider.NormalizeContainer(remote?.Container);

        ms.Container = container;
        ms.VideoType ??= VideoType.VideoFile;
        if (string.IsNullOrWhiteSpace(ms.ETag))
        {
            ms.ETag = sourceId;
        }

        return container;
    }

    private static void SanitizeClientTransportFields(MediaSourceInfo ms, string sourceId, string container)
    {
        ms.Protocol = MediaProtocol.File;
        ms.IsRemote = false;
        ms.Path = $"/mergerfin/{sourceId}.{container}";
    }

    private static string? ExtractSourceId(MediaSourceInfo ms)
    {
        if (!string.IsNullOrWhiteSpace(ms.Id) &&
            ms.Id.StartsWith("mf_", StringComparison.OrdinalIgnoreCase))
        {
            var idTail = ms.Id[3..].Trim();
            if (!string.IsNullOrEmpty(idTail))
            {
                return idTail;
            }
        }

        var mediaPath = ms.Path;
        if (string.IsNullOrWhiteSpace(mediaPath))
        {
            return null;
        }

        const string marker = "/MergerFin/Stream/";
        var idx = mediaPath.IndexOf(marker, StringComparison.OrdinalIgnoreCase);
        if (idx < 0)
        {
            return null;
        }

        var tail = mediaPath[(idx + marker.Length)..];
        var qIdx = tail.IndexOf('?');
        var extracted = (qIdx >= 0 ? tail[..qIdx] : tail).Trim();
        return string.IsNullOrEmpty(extracted) ? null : extracted;
    }

    private static bool IsExplicitFalse(ActionExecutingContext context, string queryArgName, string dtoPropertyName)
    {
        if (context.ActionArguments.TryGetValue(queryArgName, out var argVal) && argVal is false)
        {
            return true;
        }

        if (context.HttpContext.Request.Query.TryGetValue(queryArgName, out var qVal) &&
            string.Equals(qVal.ToString(), "false", StringComparison.OrdinalIgnoreCase))
        {
            return true;
        }

        if (context.ActionArguments.TryGetValue("playbackInfoDto", out var dto) && dto != null)
        {
            var props = DtoPropCache.GetOrAdd(dto.GetType(), static t => (
                t.GetProperty("AllowVideoStreamCopy", BindingFlags.Public | BindingFlags.Instance),
                t.GetProperty("AllowAudioStreamCopy", BindingFlags.Public | BindingFlags.Instance)));

            var prop = string.Equals(dtoPropertyName, "AllowVideoStreamCopy", StringComparison.Ordinal)
                ? props.VideoCopy
                : props.AudioCopy;
            if (prop?.GetValue(dto) is false)
            {
                return true;
            }
        }

        return false;
    }

    private static string RemoveQueryFlag(string url, string flag)
    {
        var ampFlag = "&" + flag;
        var idx = url.IndexOf(ampFlag, StringComparison.OrdinalIgnoreCase);
        while (idx >= 0)
        {
            var end = idx + ampFlag.Length;
            if (end == url.Length || url[end] == '&')
            {
                url = url.Remove(idx, ampFlag.Length);
                idx = url.IndexOf(ampFlag, idx, StringComparison.OrdinalIgnoreCase);
            }
            else
            {
                idx = url.IndexOf(ampFlag, end, StringComparison.OrdinalIgnoreCase);
            }
        }

        var qFlag = "?" + flag + "&";
        idx = url.IndexOf(qFlag, StringComparison.OrdinalIgnoreCase);
        if (idx >= 0)
        {
            url = url.Remove(idx + 1, flag.Length + 1);
        }

        var onlyFlag = "?" + flag;
        if (url.EndsWith(onlyFlag, StringComparison.OrdinalIgnoreCase))
        {
            url = url[..^onlyFlag.Length];
        }

        return url;
    }

    private static bool IsMergerFinCaller(HttpRequest request)
    {
        if (request.Headers.TryGetValue("Authorization", out var auth) &&
            ContainsMergerFin(auth.ToString()))
        {
            return true;
        }

        if (request.Headers.TryGetValue("X-Emby-Authorization", out var embyAuth) &&
            ContainsMergerFin(embyAuth.ToString()))
        {
            return true;
        }

        if (request.Headers.TryGetValue("User-Agent", out var ua) &&
            ContainsMergerFin(ua.ToString()))
        {
            return true;
        }

        return false;
    }

    private static bool ContainsMergerFin(string value)
    {
        return !string.IsNullOrEmpty(value) &&
               (value.Contains("MergerFin", StringComparison.OrdinalIgnoreCase) ||
                value.Contains("MergeFin", StringComparison.OrdinalIgnoreCase));
    }
}
