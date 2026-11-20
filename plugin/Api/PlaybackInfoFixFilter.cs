using System;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Services;
using MediaBrowser.Model.MediaInfo;
using Microsoft.AspNetCore.Mvc;
using Microsoft.AspNetCore.Mvc.Filters;

namespace Jellyfin.Plugin.MergerFin.Api;

/// <summary>
/// Fixes Jellyfin's PlaybackInfo response for MergerFin HTTP/.strm sources:
/// 1. Replaces the 75-byte .strm stub file size with the real remote file Size & Bitrate.
/// 2. When Jellyfin places a Static=true URL inside TranscodingUrl (e.g. when video transcoding
///    is disabled on the user policy), moves it to DirectStreamUrl + SupportsDirectStream=true
///    so jellyfin-web seeks natively via HTTP Byte-Range instead of doubling transcodingOffsetTicks!
/// </summary>
public class PlaybackInfoFixFilter : IAsyncResultFilter
{
    private readonly RemoteCatalogStore _store;

    public PlaybackInfoFixFilter(RemoteCatalogStore store)
    {
        _store = store;
    }

    public Task OnResultExecutionAsync(ResultExecutingContext context, ResultExecutionDelegate next)
    {
        var path = context.HttpContext.Request.Path.Value;
        if (!string.IsNullOrEmpty(path) &&
            path.EndsWith("/PlaybackInfo", StringComparison.OrdinalIgnoreCase) &&
            context.Result is ObjectResult { Value: PlaybackInfoResponse playbackInfo } &&
            playbackInfo.MediaSources != null)
        {
            foreach (var ms in playbackInfo.MediaSources)
            {
                var sourceId = ExtractSourceId(ms.Path);
                if (string.IsNullOrEmpty(sourceId))
                {
                    continue;
                }

                var remote = _store.GetBySourceId(sourceId);
                if (remote != null)
                {
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
                }

                if (!string.IsNullOrEmpty(ms.TranscodingUrl) &&
                    ms.TranscodingUrl.Contains("Static=true", StringComparison.OrdinalIgnoreCase))
                {
                    ms.SupportsDirectStream = true;
                    ms.TranscodingUrl = null;
                    ms.TranscodeReasons = 0;
                }
            }
        }

        return next();
    }

    private static string? ExtractSourceId(string? mediaPath)
    {
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
        return (qIdx >= 0 ? tail[..qIdx] : tail).Trim();
    }
}
