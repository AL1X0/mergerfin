using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Models;
using Jellyfin.Plugin.MergerFin.Services;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Entities.Movies;
using MediaBrowser.Controller.Entities.TV;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Dto;
using MediaBrowser.Model.Entities;
using MediaBrowser.Model.MediaInfo;

namespace Jellyfin.Plugin.MergerFin.Providers;

/// <summary>
/// Injects remote Jellyfin media sources directly into local Jellyfin items' MediaSources[]
/// so multi-version selection works natively across all Jellyfin clients without ffprobe overhead.
/// </summary>
public class MergerFinMediaSourceProvider : IMediaSourceProvider
{
    private readonly RemoteCatalogStore _store;

    public MergerFinMediaSourceProvider(RemoteCatalogStore store)
    {
        _store = store;
    }

    public Task<IEnumerable<MediaSourceInfo>> GetMediaSources(BaseItem item, CancellationToken cancellationToken)
    {
        var cfg = Plugin.Instance?.Configuration;
        if (cfg == null || !cfg.EnableMultiVersionInjection)
        {
            return Task.FromResult(Enumerable.Empty<MediaSourceInfo>());
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
            return Task.FromResult(Enumerable.Empty<MediaSourceInfo>());
        }

        if (matches.Count == 0)
        {
            return Task.FromResult(Enumerable.Empty<MediaSourceInfo>());
        }

        // If this local item is itself a .strm file pointing to one of our sources, skip duplicating that exact source.
        var existingStrmSourceId = ExtractStrmSourceId(item.Path);
        var localBase = (cfg.LocalBaseUrl ?? "http://127.0.0.1:8096").Trim().TrimEnd('/');
        var results = new List<MediaSourceInfo>(matches.Count);

        foreach (var src in matches)
        {
            if (!string.IsNullOrEmpty(existingStrmSourceId) &&
                string.Equals(existingStrmSourceId, src.SourceId, StringComparison.OrdinalIgnoreCase))
            {
                continue;
            }

            var streams = src.Streams.Select(st => new MediaStream
            {
                Type = ParseStreamType(st.Type),
                Index = st.Index,
                Codec = st.Codec,
                Language = st.Language,
                Title = st.Title,
                IsDefault = st.IsDefault,
                IsForced = st.IsForced,
                IsExternal = st.IsExternal,
                Width = st.Width,
                Height = st.Height,
                BitRate = st.BitRate,
                Channels = st.Channels,
                SampleRate = st.SampleRate
            }).ToList();

            results.Add(new MediaSourceInfo
            {
                Id = "mf_" + src.SourceId,
                Name = src.Label,
                Path = $"{localBase}/MergerFin/Stream/{src.SourceId}",
                Protocol = MediaProtocol.Http,
                Type = MediaSourceType.Default,
                Container = string.IsNullOrWhiteSpace(src.Container) ? "mkv" : src.Container,
                Size = src.Size > 0 ? src.Size : null,
                Bitrate = src.Bitrate > 0 ? src.Bitrate : null,
                RunTimeTicks = src.RunTimeTicks > 0 ? src.RunTimeTicks : item.RunTimeTicks,
                SupportsDirectPlay = true,
                SupportsDirectStream = true,
                SupportsTranscoding = true,
                IsRemote = true,
                MediaStreams = streams
            });
        }

        return Task.FromResult<IEnumerable<MediaSourceInfo>>(results);
    }

    public Task<ILiveStream> OpenMediaSource(string openToken, List<ILiveStream> currentLiveStreams, CancellationToken cancellationToken)
    {
        throw new NotSupportedException("MergerFin HTTP media sources do not require live stream opening.");
    }

    private static string? ExtractStrmSourceId(string? itemPath)
    {
        if (string.IsNullOrWhiteSpace(itemPath) || !itemPath.EndsWith(".strm", StringComparison.OrdinalIgnoreCase))
        {
            return null;
        }

        try
        {
            if (!File.Exists(itemPath))
            {
                return null;
            }

            var content = File.ReadAllText(itemPath).Trim();
            const string marker = "/MergerFin/Stream/";
            var idx = content.IndexOf(marker, StringComparison.OrdinalIgnoreCase);
            if (idx < 0)
            {
                return null;
            }

            var tail = content[(idx + marker.Length)..];
            var qIdx = tail.IndexOf('?');
            return qIdx >= 0 ? tail[..qIdx] : tail;
        }
        catch
        {
            return null;
        }
    }

    private static MediaStreamType ParseStreamType(string type)
    {
        return type.ToLowerInvariant() switch
        {
            "video" => MediaStreamType.Video,
            "audio" => MediaStreamType.Audio,
            "subtitle" => MediaStreamType.Subtitle,
            "embeddedimage" => MediaStreamType.EmbeddedImage,
            _ => MediaStreamType.Data
        };
    }
}
