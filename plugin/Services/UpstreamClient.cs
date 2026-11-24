using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Jellyfin.Plugin.MergerFin.Configuration;
using Jellyfin.Plugin.MergerFin.Models;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.MergerFin.Services;

/// <summary>
/// HTTP client for communicating with remote Jellyfin servers exclusively via API Key.
/// </summary>
public class UpstreamClient
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNameCaseInsensitive = true
    };

    private static readonly TimeSpan UserCacheTtl = TimeSpan.FromMinutes(15);

    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<UpstreamClient> _logger;
    private readonly ConcurrentDictionary<string, (DateTime FetchedAtUtc, Dictionary<string, string> UsersByName)> _remoteUsersCache = new(StringComparer.OrdinalIgnoreCase);
    private readonly ConcurrentDictionary<string, bool> _usePlayingFallbackForProgress = new(StringComparer.OrdinalIgnoreCase);

    public UpstreamClient(IHttpClientFactory httpClientFactory, ILogger<UpstreamClient> logger)
    {
        _httpClientFactory = httpClientFactory;
        _logger = logger;
    }

    public static string BuildMediaBrowserHeader(string apiKey, UpstreamSessionIdentity? id = null)
    {
        if (id == null || (string.IsNullOrWhiteSpace(id.LocalUsername) && string.IsNullOrWhiteSpace(id.ClientName)))
        {
            const string baseHdr = "MediaBrowser Client=\"MergerFin Plugin\", Device=\"Jellyfin Host\", DeviceId=\"mergerfin-plugin\", Version=\"0.1.14\"";
            return string.IsNullOrEmpty(apiKey) ? baseHdr : $"{baseHdr}, Token=\"{Sanitize(apiKey, 256)}\"";
        }

        var client = string.IsNullOrWhiteSpace(id.ClientName) ? "MergerFin Plugin" : Sanitize(id.ClientName, 48);
        var user = Sanitize(id.LocalUsername, 48);
        var device = Sanitize(id.DeviceName, 48);

        var displayDevice = !string.IsNullOrEmpty(user)
            ? (string.IsNullOrEmpty(device) ? user : $"{user} ({device})")
            : (string.IsNullOrEmpty(device) ? "Jellyfin Host" : device);

        var devId = BuildDeviceId(user, id.DeviceId);
        var header = $"MediaBrowser Client=\"{client}\", Device=\"{displayDevice}\", DeviceId=\"{devId}\", Version=\"0.1.14\"";
        if (!string.IsNullOrEmpty(apiKey))
        {
            header += $", Token=\"{Sanitize(apiKey, 256)}\"";
        }

        return header;
    }

    private static string BuildDeviceId(string user, string rawDeviceId)
    {
        var u = SanitizeToken(user, 24);
        var d = SanitizeToken(rawDeviceId, 32);
        if (!string.IsNullOrEmpty(u) && !string.IsNullOrEmpty(d))
        {
            return $"mf-{u}-{d}";
        }

        if (!string.IsNullOrEmpty(u))
        {
            return $"mf-{u}";
        }

        return !string.IsNullOrEmpty(d) ? $"mf-{d}" : "mergerfin-plugin";
    }

    private static string Sanitize(string? value, int maxLen)
    {
        if (string.IsNullOrWhiteSpace(value))
        {
            return string.Empty;
        }

        var sb = new StringBuilder(Math.Min(value.Length, maxLen));
        foreach (var c in value.Trim())
        {
            if (c < 0x20 || c == 0x7f || c == '"' || c == '\\')
            {
                continue;
            }

            sb.Append(c);
            if (sb.Length >= maxLen)
            {
                break;
            }
        }

        return sb.ToString().Trim();
    }

    private static string SanitizeToken(string? value, int maxLen)
    {
        if (string.IsNullOrWhiteSpace(value))
        {
            return string.Empty;
        }

        var sb = new StringBuilder(Math.Min(value.Length, maxLen));
        foreach (var c in value.Trim().ToLowerInvariant())
        {
            if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_')
            {
                sb.Append(c);
                if (sb.Length >= maxLen)
                {
                    break;
                }
            }
        }

        return sb.ToString().Trim('-', '_');
    }

    /// <summary>
    /// Probes /System/Info/Public and validates the API Key against GET /Users.
    /// </summary>
    public async Task<ProbeResult> TestApiKeyAsync(string baseUrl, string apiKey, CancellationToken ct)
    {
        var cleanUrl = (baseUrl ?? string.Empty).Trim().TrimEnd('/');
        var cleanKey = (apiKey ?? string.Empty).Trim();
        if (string.IsNullOrWhiteSpace(cleanUrl))
        {
            return new ProbeResult { Online = false, Error = "Server Base URL is required." };
        }

        if (string.IsNullOrWhiteSpace(cleanKey))
        {
            return new ProbeResult { Online = false, Error = "API Key is required." };
        }

        var client = _httpClientFactory.CreateClient("MergerFin");
        client.Timeout = TimeSpan.FromSeconds(15);

        var sw = Stopwatch.StartNew();
        try
        {
            using var infoResp = await client.GetAsync($"{cleanUrl}/System/Info/Public", ct).ConfigureAwait(false);
            sw.Stop();
            if (!infoResp.IsSuccessStatusCode)
            {
                return new ProbeResult
                {
                    Online = false,
                    LatencyMs = sw.ElapsedMilliseconds,
                    Error = $"/System/Info/Public returned HTTP {(int)infoResp.StatusCode}"
                };
            }

            var infoJson = await infoResp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
            using var infoDoc = JsonDocument.Parse(infoJson);
            var serverName = infoDoc.RootElement.TryGetProperty("ServerName", out var sn) ? sn.GetString() ?? string.Empty : string.Empty;
            var version = infoDoc.RootElement.TryGetProperty("Version", out var vr) ? vr.GetString() ?? string.Empty : string.Empty;

            using var usersReq = new HttpRequestMessage(HttpMethod.Get, $"{cleanUrl}/Users");
            usersReq.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(cleanKey));
            usersReq.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));

            using var usersResp = await client.SendAsync(usersReq, ct).ConfigureAwait(false);
            if (!usersResp.IsSuccessStatusCode)
            {
                return new ProbeResult
                {
                    Online = true,
                    LatencyMs = sw.ElapsedMilliseconds,
                    ServerName = serverName,
                    Version = version,
                    Error = $"API Key rejected (HTTP {(int)usersResp.StatusCode})"
                };
            }

            var usersJson = await usersResp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
            var userCount = 0;
            using (var usersDoc = JsonDocument.Parse(usersJson))
            {
                if (usersDoc.RootElement.ValueKind == JsonValueKind.Array)
                {
                    userCount = usersDoc.RootElement.GetArrayLength();
                }
            }

            return new ProbeResult
            {
                Online = true,
                LatencyMs = sw.ElapsedMilliseconds,
                ServerName = serverName,
                Version = version,
                UserCount = userCount
            };
        }
        catch (Exception ex)
        {
            sw.Stop();
            return new ProbeResult
            {
                Online = false,
                LatencyMs = sw.ElapsedMilliseconds,
                Error = ex.Message
            };
        }
    }

    private static readonly HashSet<string> SupportedLibraryCollectionTypes = new(StringComparer.OrdinalIgnoreCase)
    {
        string.Empty,
        "movies",
        "tvshows",
        "mixed"
    };

    /// <summary>
    /// Fetches video libraries (movies, tvshows, mixed) from a remote Jellyfin server via GET /Library/VirtualFolders.
    /// </summary>
    public async Task<List<RemoteVirtualFolderDto>> FetchVirtualFoldersAsync(RemoteServerConfig srv, CancellationToken ct)
    {
        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var apiKey = srv.GetEffectiveApiKey();
        if (string.IsNullOrWhiteSpace(baseUrl) || string.IsNullOrWhiteSpace(apiKey))
        {
            return [];
        }

        var client = _httpClientFactory.CreateClient("MergerFin");
        client.Timeout = TimeSpan.FromSeconds(15);

        using var req = new HttpRequestMessage(HttpMethod.Get, $"{baseUrl}/Library/VirtualFolders");
        req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(apiKey));
        req.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));

        using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
        resp.EnsureSuccessStatusCode();

        var body = await resp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
        var folders = JsonSerializer.Deserialize<List<RemoteVirtualFolderDto>>(body, JsonOptions) ?? [];

        var result = new List<RemoteVirtualFolderDto>();
        foreach (var f in folders)
        {
            if (string.IsNullOrWhiteSpace(f.ItemId))
            {
                continue;
            }

            var colType = (f.CollectionType ?? string.Empty).Trim();
            if (!SupportedLibraryCollectionTypes.Contains(colType))
            {
                continue;
            }

            result.Add(new RemoteVirtualFolderDto
            {
                Name = (f.Name ?? string.Empty).Trim(),
                ItemId = f.ItemId.Trim(),
                CollectionType = colType
            });
        }

        return result;
    }

    public async Task<List<RemoteItemDto>> FetchAllItemsAsync(
        RemoteServerConfig srv,
        string[] itemTypes,
        CancellationToken ct,
        string? parentId = null)
    {
        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var apiKey = srv.GetEffectiveApiKey();
        var client = _httpClientFactory.CreateClient("MergerFin");
        client.Timeout = TimeSpan.FromSeconds(45);

        const int pageSize = 200;
        var all = new List<RemoteItemDto>();
        var startIndex = 0;
        var typesParam = Uri.EscapeDataString(string.Join(",", itemTypes));
        var parentParam = string.IsNullOrWhiteSpace(parentId)
            ? string.Empty
            : $"&ParentId={Uri.EscapeDataString(parentId.Trim())}";
        const string fields = "ProviderIds,MediaSources,MediaStreams,ProductionYear,Path";

        while (true)
        {
            var url = $"{baseUrl}/Items?Recursive=true{parentParam}&IncludeItemTypes={typesParam}&CollapseBoxSetItems=false&Fields={fields}&StartIndex={startIndex}&Limit={pageSize}";

            using var req = new HttpRequestMessage(HttpMethod.Get, url);
            req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(apiKey));
            req.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));

            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
            resp.EnsureSuccessStatusCode();

            var body = await resp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
            var page = JsonSerializer.Deserialize<RemoteItemsResponse>(body, JsonOptions);
            if (page == null || page.Items.Count == 0)
            {
                break;
            }

            all.AddRange(page.Items);
            startIndex += page.Items.Count;
            if (page.TotalRecordCount > 0 && all.Count >= page.TotalRecordCount)
            {
                break;
            }

            if (page.Items.Count < pageSize)
            {
                break;
            }
        }

        return all;
    }

    /// <summary>
    /// Automatically resolves a local Jellyfin username to the matching remote user's ID
    /// on the upstream server via GET /Users (cached 15 minutes).
    /// </summary>
    public async Task<string?> ResolveRemoteUserIdByUsernameAsync(RemoteServerConfig srv, string? localUsername, CancellationToken ct)
    {
        if (string.IsNullOrWhiteSpace(localUsername))
        {
            return null;
        }

        var apiKey = srv.GetEffectiveApiKey();
        if (string.IsNullOrWhiteSpace(apiKey))
        {
            return null;
        }

        if (_remoteUsersCache.TryGetValue(srv.Id, out var cached) &&
            DateTime.UtcNow - cached.FetchedAtUtc < UserCacheTtl)
        {
            return cached.UsersByName.TryGetValue(localUsername.Trim(), out var cachedId) ? cachedId : null;
        }

        var map = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        try
        {
            var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
            using var req = new HttpRequestMessage(HttpMethod.Get, $"{baseUrl}/Users");
            req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(apiKey));
            req.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));

            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);
            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
            if (resp.IsSuccessStatusCode)
            {
                var json = await resp.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
                using var doc = JsonDocument.Parse(json);
                if (doc.RootElement.ValueKind == JsonValueKind.Array)
                {
                    foreach (var el in doc.RootElement.EnumerateArray())
                    {
                        var name = el.TryGetProperty("Name", out var n) ? n.GetString() : null;
                        var id = el.TryGetProperty("Id", out var i) ? i.GetString() : null;
                        if (!string.IsNullOrWhiteSpace(name) && !string.IsNullOrWhiteSpace(id))
                        {
                            map[name.Trim()] = id.Trim();
                        }
                    }
                }
            }
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin failed fetching /Users from {Server}", srv.Name);
        }

        _remoteUsersCache[srv.Id] = (DateTime.UtcNow, map);
        return map.TryGetValue(localUsername.Trim(), out var resolvedId) ? resolvedId : null;
    }

    public async Task UpdateUserItemDataAsync(
        RemoteServerConfig srv,
        string remoteUserId,
        string remoteItemId,
        long playbackPositionTicks,
        bool played,
        UpstreamSessionIdentity? identity,
        CancellationToken ct)
    {
        var apiKey = srv.GetEffectiveApiKey();
        if (string.IsNullOrWhiteSpace(remoteUserId) || string.IsNullOrWhiteSpace(apiKey))
        {
            return;
        }

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var url = $"{baseUrl}/UserItems/{Uri.EscapeDataString(remoteItemId)}/UserData?userId={Uri.EscapeDataString(remoteUserId)}";
        var payload = new
        {
            PlaybackPositionTicks = played ? 0 : playbackPositionTicks,
            Played = played
        };
        using var req = new HttpRequestMessage(HttpMethod.Post, url);
        req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(apiKey, identity));
        req.Content = new StringContent(JsonSerializer.Serialize(payload), Encoding.UTF8, "application/json");

        try
        {
            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);
            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin UpdateUserItemData upstream failed for server {Server}", srv.Name);
        }
    }

    public async Task MarkPlayedAsync(
        RemoteServerConfig srv,
        string remoteUserId,
        string remoteItemId,
        bool played,
        UpstreamSessionIdentity? identity,
        CancellationToken ct)
    {
        var apiKey = srv.GetEffectiveApiKey();
        if (string.IsNullOrWhiteSpace(remoteUserId) || string.IsNullOrWhiteSpace(apiKey))
        {
            return;
        }

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var url = $"{baseUrl}/Users/{Uri.EscapeDataString(remoteUserId)}/PlayedItems/{Uri.EscapeDataString(remoteItemId)}";
        using var req = new HttpRequestMessage(played ? HttpMethod.Post : HttpMethod.Delete, url);
        req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(apiKey, identity));

        try
        {
            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);
            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin MarkPlayed upstream failed for server {Server}", srv.Name);
        }
    }

    public async Task MarkFavoriteAsync(
        RemoteServerConfig srv,
        string remoteUserId,
        string remoteItemId,
        bool favorite,
        UpstreamSessionIdentity? identity,
        CancellationToken ct)
    {
        var apiKey = srv.GetEffectiveApiKey();
        if (string.IsNullOrWhiteSpace(remoteUserId) || string.IsNullOrWhiteSpace(apiKey))
        {
            return;
        }

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var url = $"{baseUrl}/Users/{Uri.EscapeDataString(remoteUserId)}/FavoriteItems/{Uri.EscapeDataString(remoteItemId)}";
        using var req = new HttpRequestMessage(favorite ? HttpMethod.Post : HttpMethod.Delete, url);
        req.Headers.TryAddWithoutValidation("Authorization", BuildMediaBrowserHeader(apiKey, identity));

        try
        {
            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);
            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin MarkFavorite upstream failed for server {Server}", srv.Name);
        }
    }

    public async Task ReportPlaystateAsync(
        RemoteServerConfig srv,
        string endpointPath,
        object body,
        UpstreamSessionIdentity? identity,
        CancellationToken ct)
    {
        var apiKey = srv.GetEffectiveApiKey();
        if (string.IsNullOrWhiteSpace(apiKey))
        {
            return;
        }

        var isProgress = string.Equals(endpointPath, "/Sessions/Playing/Progress", StringComparison.OrdinalIgnoreCase);
        if (isProgress && _usePlayingFallbackForProgress.ContainsKey(srv.Id))
        {
            endpointPath = "/Sessions/Playing";
        }

        var baseUrl = srv.BaseUrl.Trim().TrimEnd('/');
        var authHeader = BuildMediaBrowserHeader(apiKey, identity);
        var jsonPayload = JsonSerializer.Serialize(body);

        try
        {
            var client = _httpClientFactory.CreateClient("MergerFin");
            client.Timeout = TimeSpan.FromSeconds(10);

            using var req = new HttpRequestMessage(HttpMethod.Post, $"{baseUrl}{endpointPath}");
            req.Headers.TryAddWithoutValidation("Authorization", authHeader);
            req.Content = new StringContent(jsonPayload, Encoding.UTF8, "application/json");

            using var resp = await client.SendAsync(req, ct).ConfigureAwait(false);

            // Jellyfin's PlaybackReporting plugin throws ArgumentOutOfRangeException (HTTP 400)
            // on /Sessions/Playing/Progress for API-key sessions (Users.Count == 0), whereas
            // /Sessions/Playing succeeds with HTTP 204 and updates PositionTicks + WebSocket live!
            if (isProgress && (int)resp.StatusCode == 400 &&
                !string.Equals(endpointPath, "/Sessions/Playing", StringComparison.OrdinalIgnoreCase))
            {
                _usePlayingFallbackForProgress[srv.Id] = true;
                using var retryReq = new HttpRequestMessage(HttpMethod.Post, $"{baseUrl}/Sessions/Playing");
                retryReq.Headers.TryAddWithoutValidation("Authorization", authHeader);
                retryReq.Content = new StringContent(jsonPayload, Encoding.UTF8, "application/json");
                using var retryResp = await client.SendAsync(retryReq, ct).ConfigureAwait(false);
            }
        }
        catch (Exception ex)
        {
            _logger.LogDebug(ex, "MergerFin ReportPlaystate {Path} failed for server {Server}", endpointPath, srv.Name);
        }
    }
}
