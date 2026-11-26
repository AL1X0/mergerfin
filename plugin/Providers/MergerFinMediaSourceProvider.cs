using System;
using System.Collections.Generic;
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
        var existingStrmSourceId = StrmSourceResolver.TryGetSourceId(item.Path);
        var localBase = (cfg.LocalBaseUrl ?? "http://127.0.0.1:8096").Trim().TrimEnd('/');
        var results = new List<MediaSourceInfo>(matches.Count);

        foreach (var src in matches)
        {
            if (!string.IsNullOrEmpty(existingStrmSourceId) &&
                string.Equals(existingStrmSourceId, src.SourceId, StringComparison.OrdinalIgnoreCase))
            {
                continue;
            }

            var streams = BuildMediaStreams(src);

            results.Add(new MediaSourceInfo
            {
                Id = "mf_" + src.SourceId,
                Name = src.Label,
                Path = $"{localBase}/MergerFin/Stream/{src.SourceId}",
                Protocol = MediaProtocol.Http,
                Type = MediaSourceType.Default,
                VideoType = VideoType.VideoFile,
                Container = NormalizeContainer(src.Container),
                Size = src.Size > 0 ? src.Size : null,
                Bitrate = src.Bitrate > 0 ? src.Bitrate : null,
                RunTimeTicks = src.RunTimeTicks > 0 ? src.RunTimeTicks : item.RunTimeTicks,
                ETag = src.SourceId,
                SupportsDirectPlay = true,
                SupportsDirectStream = true,
                SupportsTranscoding = true,
                IsRemote = false,
                // Metadata is already known from the catalog: cap ffmpeg's probe window on this HTTP input
                // instead of the global -analyzeduration (200M in the official image), which can make
                // ffmpeg pull hundreds of MB from the remote server before the first remux/transcode segment.
                AnalyzeDurationMs = RemoteAnalyzeDurationMs,
                MediaStreams = streams
            });
        }

        return Task.FromResult<IEnumerable<MediaSourceInfo>>(results);
    }

    /// <summary>
    /// ffmpeg probe window (ms) applied to remote MergerFin sources.
    /// </summary>
    public const int RemoteAnalyzeDurationMs = 5000;

    /// <summary>
    /// Returns the remote source's embedded (in-container) streams in container order.
    /// Remote Jellyfin numbers external subtitles FIRST, which shifts every embedded stream's Index;
    /// position in this list is the real container (ffprobe) index.
    /// </summary>
    public static List<IndexedMediaStream> GetEmbeddedStreams(IndexedRemoteSource src)
    {
        return src.Streams
            .Where(st => !st.IsExternal)
            .OrderBy(st => st.Index)
            .ToList();
    }

    /// <summary>
    /// Maps a container-order embedded stream position to the remote server's own stream Index.
    /// </summary>
    public static int? MapEmbeddedPositionToRemoteIndex(IndexedRemoteSource src, int position, string expectedType)
    {
        var embedded = GetEmbeddedStreams(src);
        if (position < 0 || position >= embedded.Count)
        {
            return null;
        }

        var st = embedded[position];
        return string.Equals(st.Type, expectedType, StringComparison.OrdinalIgnoreCase) ? st.Index : null;
    }

    /// <summary>
    /// Builds client-facing MediaStreams for a remote source. Embedded streams are renumbered to their
    /// container order (0..n-1), exactly like Jellyfin numbers a locally probed file, so clients that map
    /// Index to the in-container track (ExoPlayer/AVPlayer/mpv based direct play) pick the right subtitle
    /// and audio track. Subtitle requests are translated back to remote indexes by RemoteSubtitleProxy.
    /// </summary>
    public static List<MediaStream> BuildMediaStreams(IndexedRemoteSource src)
    {
        return GetEmbeddedStreams(src)
            .Select((st, position) => new MediaStream
            {
                Type = ParseStreamType(st.Type),
                Index = position,
                Codec = string.IsNullOrWhiteSpace(st.Codec) ? null : st.Codec,
                CodecTag = st.CodecTag,
                Language = string.IsNullOrWhiteSpace(st.Language) ? null : st.Language,
                Title = string.IsNullOrWhiteSpace(st.Title) ? null : st.Title,
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
                IsExternal = false,
                Width = st.Width,
                Height = st.Height,
                BitRate = st.BitRate,
                Channels = st.Channels,
                SampleRate = st.SampleRate
            })
            .ToList();
    }

    public static string NormalizeContainer(string? raw)
    {
        if (string.IsNullOrWhiteSpace(raw) ||
            string.Equals(raw.Trim(), "strm", StringComparison.OrdinalIgnoreCase))
        {
            return "mkv";
        }

        var parts = raw.Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries);
        if (parts.Length == 0)
        {
            return "mkv";
        }

        var chosen = parts.FirstOrDefault(p => string.Equals(p, "mp4", StringComparison.OrdinalIgnoreCase))
            ?? parts.FirstOrDefault(p => string.Equals(p, "mkv", StringComparison.OrdinalIgnoreCase))
            ?? parts[0];

        chosen = chosen.ToLowerInvariant() switch
        {
            "matroska" => "mkv",
            "mpegts" => "ts",
            _ => chosen.ToLowerInvariant()
        };

        var clean = new string(chosen.Where(char.IsLetterOrDigit).ToArray());
        return string.IsNullOrEmpty(clean) || string.Equals(clean, "strm", StringComparison.OrdinalIgnoreCase)
            ? "mkv"
            : clean;
    }

    public Task<ILiveStream> OpenMediaSource(string openToken, List<ILiveStream> currentLiveStreams, CancellationToken cancellationToken)
    {
        throw new NotSupportedException("MergerFin HTTP media sources do not require live stream opening.");
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
