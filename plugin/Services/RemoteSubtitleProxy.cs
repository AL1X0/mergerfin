using System;
using System.Collections.Generic;
using System.Linq;
using System.Net.Http;
using System.Text;
using System.Text.RegularExpressions;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Configuration;
using Jellyfin.Plugin.MergerFin.Models;
using Jellyfin.Plugin.MergerFin.Providers;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Entities;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// Serves subtitles of MergerFin remote sources straight from the remote Jellyfin server that owns the file.
/// <para>
/// Without this, local Jellyfin handles /Videos/{id}/{msId}/Subtitles/... itself, which for remote sources:
/// - fails outright for injected "mf_*" versions (non-GUID media source id => no subtitle cache path => 404);
/// - for .strm items, runs ffmpeg over the loopback HTTP proxy and downloads the WHOLE remote file before the
///   first cue can be served (minutes of delay, HLS subtitle segments time out and drift);
/// - cannot see the remote server's external subtitle files.
/// The remote server extracts from its local file (cached), honours the same start/end/copyTimestamps/VTT
/// time-map parameters, and therefore returns cues on exactly the timeline the proxied video uses.
/// </para>
/// </summary>
public partial class RemoteSubtitleProxy
{
    private const int MaxSubtitleBytes = 32 * 1024 * 1024;

    private static readonly HashSet<string> DroppedQueryKeys = new(StringComparer.OrdinalIgnoreCase)
    {
        "api_key",
        "ApiKey",
        "X-Emby-Token",
        "X-MediaBrowser-Token"
    };

    private static readonly HashSet<string> LegacyOverrideQueryKeys = new(StringComparer.OrdinalIgnoreCase)
    {
        "itemId",
        "mediaSourceId",
        "index"
    };

    private readonly RemoteCatalogStore _store;
    private readonly ILibraryManager _libraryManager;
    private readonly IMediaSourceManager _mediaSourceManager;
    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<RemoteSubtitleProxy> _logger;

    public RemoteSubtitleProxy(
        RemoteCatalogStore store,
        ILibraryManager libraryManager,
        IMediaSourceManager mediaSourceManager,
        IHttpClientFactory httpClientFactory,
        ILogger<RemoteSubtitleProxy> logger)
    {
        _store = store;
        _libraryManager = libraryManager;
        _mediaSourceManager = mediaSourceManager;
        _httpClientFactory = httpClientFactory;
        _logger = logger;
    }

    [GeneratedRegex(
        @"/Videos/(?<item>[^/]+)/(?<ms>[^/]+)/Subtitles/(?<idx>\d+)/(?:(?<start>\d+)/)?Stream\.(?<fmt>[A-Za-z0-9]+)$",
        RegexOptions.IgnoreCase | RegexOptions.CultureInvariant)]
    private static partial Regex SubtitleRouteRegex();

    /// <summary>
    /// Proxies the request when it targets a subtitle of a MergerFin remote source.
    /// Returns null when the request is not ours (or the remote failed) so local Jellyfin handles it.
    /// </summary>
    public async Task<IActionResult?> TryProxyAsync(HttpRequest request, CancellationToken ct)
    {
        if (!HttpMethods.IsGet(request.Method))
        {
            return null;
        }

        var path = request.Path.Value;
        if (string.IsNullOrEmpty(path) || path.IndexOf("/Subtitles/", StringComparison.OrdinalIgnoreCase) < 0)
        {
            return null;
        }

        var m = SubtitleRouteRegex().Match(path);
        if (!m.Success || !int.TryParse(m.Groups["idx"].Value, out var clientIndex))
        {
            return null;
        }

        // Legacy clients can override route values with query params; let Jellyfin handle those as-is.
        if (request.Query.Keys.Any(k => LegacyOverrideQueryKeys.Contains(k)))
        {
            return null;
        }

        var resolved = Resolve(m.Groups["ms"].Value, clientIndex);
        if (resolved == null)
        {
            return null;
        }

        var (src, remoteIndex, srv) = resolved.Value;
        var apiKey = srv.GetEffectiveApiKey();
        var format = m.Groups["fmt"].Value;
        var startSegment = m.Groups["start"].Success ? m.Groups["start"].Value + "/" : string.Empty;

        var url = new StringBuilder(256)
            .Append(srv.BaseUrl.Trim().TrimEnd('/'))
            .Append("/Videos/").Append(Uri.EscapeDataString(src.RemoteItemId))
            .Append('/').Append(Uri.EscapeDataString(src.RemoteMediaSourceId))
            .Append("/Subtitles/").Append(remoteIndex)
            .Append('/').Append(startSegment)
            .Append("Stream.").Append(Uri.EscapeDataString(format))
            .Append("?ApiKey=").Append(Uri.EscapeDataString(apiKey));

        foreach (var kv in request.Query)
        {
            if (DroppedQueryKeys.Contains(kv.Key))
            {
                continue;
            }

            foreach (var v in kv.Value)
            {
                url.Append('&').Append(Uri.EscapeDataString(kv.Key)).Append('=').Append(Uri.EscapeDataString(v ?? string.Empty));
            }
        }

        try
        {
            using var req = new HttpRequestMessage(HttpMethod.Get, url.ToString());
            req.Headers.TryAddWithoutValidation("Authorization", UpstreamClient.BuildMediaBrowserHeader(apiKey));

            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(60);
            using var resp = await client.SendAsync(req, HttpCompletionOption.ResponseHeadersRead, ct).ConfigureAwait(false);
            if (!resp.IsSuccessStatusCode)
            {
                _logger.LogWarning(
                    "MergerFin remote subtitle {Index} for source {SourceId} on {Server} returned HTTP {Status}; falling back to local handling",
                    remoteIndex,
                    src.SourceId,
                    srv.Name,
                    (int)resp.StatusCode);
                return null;
            }

            if (resp.Content.Headers.ContentLength is > MaxSubtitleBytes)
            {
                return null;
            }

            var body = await resp.Content.ReadAsByteArrayAsync(ct).ConfigureAwait(false);
            var contentType = resp.Content.Headers.ContentType?.ToString();
            if (string.IsNullOrWhiteSpace(contentType))
            {
                contentType = "text/plain; charset=utf-8";
            }

            return new FileContentResult(body, contentType);
        }
        catch (OperationCanceledException) when (ct.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "MergerFin remote subtitle proxy failed for source {SourceId} on {Server}; falling back to local handling", src.SourceId, srv.Name);
            return null;
        }
    }

    private (IndexedRemoteSource Source, int RemoteIndex, RemoteServerConfig Server)? Resolve(string mediaSourceId, int clientIndex)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null)
        {
            return null;
        }

        IndexedRemoteSource? src;
        int? remoteIndex;

        if (mediaSourceId.StartsWith("mf_", StringComparison.OrdinalIgnoreCase))
        {
            // Injected version: client-facing indexes are container positions (see BuildMediaStreams).
            src = _store.GetBySourceId(mediaSourceId);
            if (src == null)
            {
                return null;
            }

            remoteIndex = MergerFinMediaSourceProvider.MapEmbeddedPositionToRemoteIndex(src, clientIndex, "Subtitle");
        }
        else
        {
            // .strm stub: media source id == the .strm item's id; indexes come from local ffprobe of the stream.
            if (!Guid.TryParse(mediaSourceId, out var msGuid))
            {
                return null;
            }

            var item = _libraryManager.GetItemById(msGuid);
            var sourceId = StrmSourceResolver.TryGetSourceId(item?.Path);
            if (sourceId == null)
            {
                return null;
            }

            src = _store.GetBySourceId(sourceId);
            if (src == null)
            {
                return null;
            }

            var localStreams = _mediaSourceManager.GetMediaStreams(msGuid);
            if (localStreams.Count == 0)
            {
                // Never probed locally: the client saw our catalog-built streams (container positions).
                remoteIndex = MergerFinMediaSourceProvider.MapEmbeddedPositionToRemoteIndex(src, clientIndex, "Subtitle");
            }
            else
            {
                var requested = localStreams.FirstOrDefault(s => s.Type == MediaStreamType.Subtitle && s.Index == clientIndex);
                if (requested == null || requested.IsExternal)
                {
                    // Unknown index, or a local sidecar subtitle next to the .strm: let Jellyfin serve it.
                    return null;
                }

                var position = localStreams
                    .Where(s => !s.IsExternal)
                    .OrderBy(s => s.Index)
                    .ToList()
                    .IndexOf(requested);
                remoteIndex = MergerFinMediaSourceProvider.MapEmbeddedPositionToRemoteIndex(src, position, "Subtitle");
            }
        }

        if (remoteIndex == null)
        {
            return null;
        }

        var srv = cfg.Servers.FirstOrDefault(s =>
            s.Enabled && string.Equals(s.Id, src.ServerId, StringComparison.OrdinalIgnoreCase));
        if (srv == null || string.IsNullOrWhiteSpace(srv.BaseUrl) || string.IsNullOrWhiteSpace(srv.GetEffectiveApiKey()))
        {
            return null;
        }

        return (src, remoteIndex.Value, srv);
    }
}
