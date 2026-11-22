using System;
using System.Collections.Generic;
using System.Text.Json.Serialization;

namespace Jellyfin.Plugin.MergerFin.Models;

public class IndexedRemoteSource
{
    public string SourceId { get; set; } = string.Empty;

    public string ServerId { get; set; } = string.Empty;

    public string ServerName { get; set; } = string.Empty;

    public int ServerPriority { get; set; } = 100;

    public string ItemType { get; set; } = string.Empty; // "Movie" or "Episode"

    public string RemoteItemId { get; set; } = string.Empty;

    public string RemoteMediaSourceId { get; set; } = string.Empty;

    public string Title { get; set; } = string.Empty;

    public int? ProductionYear { get; set; }

    public string SeriesName { get; set; } = string.Empty;

    public int? SeasonNumber { get; set; }

    public int? EpisodeNumber { get; set; }

    public Dictionary<string, string> ProviderIds { get; set; } = new(StringComparer.OrdinalIgnoreCase);

    public Dictionary<string, string> SeriesProviderIds { get; set; } = new(StringComparer.OrdinalIgnoreCase);

    public string Label { get; set; } = string.Empty;

    public string Container { get; set; } = string.Empty;

    public long Size { get; set; }

    public int Bitrate { get; set; }

    public long RunTimeTicks { get; set; }

    public List<IndexedMediaStream> Streams { get; set; } = [];
}

public class IndexedMediaStream
{
    public string Type { get; set; } = string.Empty; // Video, Audio, Subtitle

    public int Index { get; set; }

    public string Codec { get; set; } = string.Empty;

    public string? CodecTag { get; set; }

    public string Language { get; set; } = string.Empty;

    public string Title { get; set; } = string.Empty;

    public string DisplayTitle { get; set; } = string.Empty;

    public string? Profile { get; set; }

    public double? Level { get; set; }

    public int? BitDepth { get; set; }

    public string? PixelFormat { get; set; }

    public string? ChannelLayout { get; set; }

    public float? AverageFrameRate { get; set; }

    public float? RealFrameRate { get; set; }

    public string? AspectRatio { get; set; }

    public string? ColorRange { get; set; }

    public string? ColorSpace { get; set; }

    public string? ColorTransfer { get; set; }

    public string? ColorPrimaries { get; set; }

    public int? DvVersionMajor { get; set; }

    public int? DvVersionMinor { get; set; }

    public int? DvProfile { get; set; }

    public int? DvLevel { get; set; }

    public int? RpuPresentFlag { get; set; }

    public int? ElPresentFlag { get; set; }

    public int? BlPresentFlag { get; set; }

    public int? DvBlSignalCompatibilityId { get; set; }

    public bool? Hdr10PlusPresentFlag { get; set; }

    public bool IsInterlaced { get; set; }

    public bool? IsAnamorphic { get; set; }

    public bool? IsAVC { get; set; }

    public int? RefFrames { get; set; }

    public bool IsDefault { get; set; }

    public bool IsForced { get; set; }

    public bool IsHearingImpaired { get; set; }

    public bool IsOriginal { get; set; }

    public bool IsExternal { get; set; }

    public int? Width { get; set; }

    public int? Height { get; set; }

    public int? BitRate { get; set; }

    public int? Channels { get; set; }

    public int? SampleRate { get; set; }
}

public class RemoteCatalogSnapshot
{
    public DateTime LastSyncUtc { get; set; }

    public string LastError { get; set; } = string.Empty;

    public List<IndexedRemoteSource> Sources { get; set; } = [];
}

public class ProbeResult
{
    [JsonPropertyName("online")]
    public bool Online { get; set; }

    [JsonPropertyName("latencyMs")]
    public long LatencyMs { get; set; }

    [JsonPropertyName("serverName")]
    public string ServerName { get; set; } = string.Empty;

    [JsonPropertyName("version")]
    public string Version { get; set; } = string.Empty;

    [JsonPropertyName("userCount")]
    public int UserCount { get; set; }

    [JsonPropertyName("error")]
    public string Error { get; set; } = string.Empty;
}

public class PluginStatusResponse
{
    [JsonPropertyName("running")]
    public bool Running { get; set; }

    [JsonPropertyName("totalSources")]
    public int TotalSources { get; set; }

    [JsonPropertyName("movies")]
    public int Movies { get; set; }

    [JsonPropertyName("episodes")]
    public int Episodes { get; set; }

    [JsonPropertyName("lastSyncUtc")]
    public DateTime? LastSyncUtc { get; set; }

    [JsonPropertyName("lastError")]
    public string LastError { get; set; } = string.Empty;
}

public class UpstreamSessionIdentity
{
    public string LocalUsername { get; set; } = string.Empty;

    public string ClientName { get; set; } = string.Empty;

    public string DeviceName { get; set; } = string.Empty;

    public string DeviceId { get; set; } = string.Empty;

    public bool IsMappedUser { get; set; }
}

// --- Upstream Jellyfin API DTOs ---

public class RemoteItemsResponse
{
    [JsonPropertyName("Items")]
    public List<RemoteItemDto> Items { get; set; } = [];

    [JsonPropertyName("TotalRecordCount")]
    public int TotalRecordCount { get; set; }
}

public class RemoteItemDto
{
    [JsonPropertyName("Id")]
    public string Id { get; set; } = string.Empty;

    [JsonPropertyName("Name")]
    public string Name { get; set; } = string.Empty;

    [JsonPropertyName("Type")]
    public string Type { get; set; } = string.Empty;

    [JsonPropertyName("ProductionYear")]
    public int? ProductionYear { get; set; }

    [JsonPropertyName("SeriesId")]
    public string? SeriesId { get; set; }

    [JsonPropertyName("SeriesName")]
    public string? SeriesName { get; set; }

    [JsonPropertyName("ParentIndexNumber")]
    public int? ParentIndexNumber { get; set; }

    [JsonPropertyName("IndexNumber")]
    public int? IndexNumber { get; set; }

    [JsonPropertyName("RunTimeTicks")]
    public long? RunTimeTicks { get; set; }

    [JsonPropertyName("Path")]
    public string? Path { get; set; }

    [JsonPropertyName("ProviderIds")]
    public Dictionary<string, string>? ProviderIds { get; set; }

    [JsonPropertyName("MediaSources")]
    public List<RemoteMediaSourceDto>? MediaSources { get; set; }
}

public class RemoteMediaSourceDto
{
    [JsonPropertyName("Id")]
    public string Id { get; set; } = string.Empty;

    [JsonPropertyName("Name")]
    public string? Name { get; set; }

    [JsonPropertyName("Path")]
    public string? Path { get; set; }

    [JsonPropertyName("Protocol")]
    public string? Protocol { get; set; }

    [JsonPropertyName("IsRemote")]
    public bool IsRemote { get; set; }

    [JsonPropertyName("Container")]
    public string? Container { get; set; }

    [JsonPropertyName("Size")]
    public long? Size { get; set; }

    [JsonPropertyName("Bitrate")]
    public int? Bitrate { get; set; }

    [JsonPropertyName("RunTimeTicks")]
    public long? RunTimeTicks { get; set; }

    [JsonPropertyName("MediaStreams")]
    public List<RemoteMediaStreamDto>? MediaStreams { get; set; }
}

public class RemoteMediaStreamDto
{
    [JsonPropertyName("Type")]
    public string Type { get; set; } = string.Empty;

    [JsonPropertyName("Index")]
    public int Index { get; set; }

    [JsonPropertyName("Codec")]
    public string? Codec { get; set; }

    [JsonPropertyName("CodecTag")]
    public string? CodecTag { get; set; }

    [JsonPropertyName("Language")]
    public string? Language { get; set; }

    [JsonPropertyName("Title")]
    public string? Title { get; set; }

    [JsonPropertyName("DisplayTitle")]
    public string? DisplayTitle { get; set; }

    [JsonPropertyName("Profile")]
    public string? Profile { get; set; }

    [JsonPropertyName("Level")]
    public double? Level { get; set; }

    [JsonPropertyName("BitDepth")]
    public int? BitDepth { get; set; }

    [JsonPropertyName("PixelFormat")]
    public string? PixelFormat { get; set; }

    [JsonPropertyName("ChannelLayout")]
    public string? ChannelLayout { get; set; }

    [JsonPropertyName("AverageFrameRate")]
    public float? AverageFrameRate { get; set; }

    [JsonPropertyName("RealFrameRate")]
    public float? RealFrameRate { get; set; }

    [JsonPropertyName("AspectRatio")]
    public string? AspectRatio { get; set; }

    [JsonPropertyName("ColorRange")]
    public string? ColorRange { get; set; }

    [JsonPropertyName("ColorSpace")]
    public string? ColorSpace { get; set; }

    [JsonPropertyName("ColorTransfer")]
    public string? ColorTransfer { get; set; }

    [JsonPropertyName("ColorPrimaries")]
    public string? ColorPrimaries { get; set; }

    [JsonPropertyName("DvVersionMajor")]
    public int? DvVersionMajor { get; set; }

    [JsonPropertyName("DvVersionMinor")]
    public int? DvVersionMinor { get; set; }

    [JsonPropertyName("DvProfile")]
    public int? DvProfile { get; set; }

    [JsonPropertyName("DvLevel")]
    public int? DvLevel { get; set; }

    [JsonPropertyName("RpuPresentFlag")]
    public int? RpuPresentFlag { get; set; }

    [JsonPropertyName("ElPresentFlag")]
    public int? ElPresentFlag { get; set; }

    [JsonPropertyName("BlPresentFlag")]
    public int? BlPresentFlag { get; set; }

    [JsonPropertyName("DvBlSignalCompatibilityId")]
    public int? DvBlSignalCompatibilityId { get; set; }

    [JsonPropertyName("Hdr10PlusPresentFlag")]
    public bool? Hdr10PlusPresentFlag { get; set; }

    [JsonPropertyName("IsInterlaced")]
    public bool IsInterlaced { get; set; }

    [JsonPropertyName("IsAnamorphic")]
    public bool? IsAnamorphic { get; set; }

    [JsonPropertyName("IsAVC")]
    public bool? IsAVC { get; set; }

    [JsonPropertyName("RefFrames")]
    public int? RefFrames { get; set; }

    [JsonPropertyName("IsDefault")]
    public bool IsDefault { get; set; }

    [JsonPropertyName("IsForced")]
    public bool IsForced { get; set; }

    [JsonPropertyName("IsHearingImpaired")]
    public bool IsHearingImpaired { get; set; }

    [JsonPropertyName("IsOriginal")]
    public bool IsOriginal { get; set; }

    [JsonPropertyName("IsExternal")]
    public bool IsExternal { get; set; }

    [JsonPropertyName("Width")]
    public int? Width { get; set; }

    [JsonPropertyName("Height")]
    public int? Height { get; set; }

    [JsonPropertyName("BitRate")]
    public int? BitRate { get; set; }

    [JsonPropertyName("Channels")]
    public int? Channels { get; set; }

    [JsonPropertyName("SampleRate")]
    public int? SampleRate { get; set; }
}
