using System;
using System.Collections.Concurrent;
using System.IO;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// Resolves the MergerFin source id referenced by a generated .strm stub
/// (".../MergerFin/Stream/{sourceId}"), cached per file path and invalidated on file mtime change.
/// Avoids re-reading the .strm from disk on every PlaybackInfo / item DTO / playback event.
/// </summary>
public static class StrmSourceResolver
{
    private const string Marker = "/MergerFin/Stream/";

    private static readonly ConcurrentDictionary<string, (DateTime WriteTimeUtc, string? SourceId)> Cache =
        new(StringComparer.Ordinal);

    public static string? TryGetSourceId(string? itemPath)
    {
        if (string.IsNullOrWhiteSpace(itemPath) || !itemPath.EndsWith(".strm", StringComparison.OrdinalIgnoreCase))
        {
            return null;
        }

        try
        {
            var writeTime = File.GetLastWriteTimeUtc(itemPath);
            if (writeTime.Year <= 1601)
            {
                // File does not exist.
                Cache.TryRemove(itemPath, out _);
                return null;
            }

            if (Cache.TryGetValue(itemPath, out var cached) && cached.WriteTimeUtc == writeTime)
            {
                return cached.SourceId;
            }

            var sourceId = ParseSourceId(File.ReadAllText(itemPath));
            Cache[itemPath] = (writeTime, sourceId);
            return sourceId;
        }
        catch
        {
            return null;
        }
    }

    public static string? ParseSourceId(string? content)
    {
        if (string.IsNullOrWhiteSpace(content))
        {
            return null;
        }

        var text = content.Trim().TrimStart('﻿');
        var idx = text.IndexOf(Marker, StringComparison.OrdinalIgnoreCase);
        if (idx < 0)
        {
            return null;
        }

        var tail = text[(idx + Marker.Length)..];
        var end = tail.IndexOfAny(['?', '#', '\r', '\n', '/']);
        var id = (end >= 0 ? tail[..end] : tail).Trim();
        return id.Length == 0 ? null : id;
    }
}
